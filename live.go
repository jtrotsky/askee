package main

// The live range, without a database. While someone is watching, askee reads the exporter's
// /metrics pages (config "live_source" and "live_sources") once a second and keeps the last five minutes in
// memory: nothing is stored, and nothing is read when nobody is looking. Live pages are then
// computed here, from a small subset of PromQL that covers what a live chart needs:
//
//	metric{label="v", label=~"re", label!="v", label!~"re"}
//	rate(...)                    per second, over the last few seconds (liveRate); counter resets skipped
//	sum(...) avg(...) max(...) min(...)
//	+ - * /  between numbers and series, and parentheses
//
// Anything else (a range [5m], by(), functions beyond these) is not attempted: the page
// falls back to the database, so the subset can stay small without breaking a chart.

import (
	"bufio"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	liveKeep  = 5*time.Minute + 10*time.Second // the live range is 5 min; a little slack
	liveEvery = time.Second
	liveIdle  = time.Minute // stop reading this long after the last live request
	// liveRate is the window rate() looks back over. One second is too short: ZFS writes in
	// bursts every 5 s and FreeBSD counts disk busy time in whole seconds, so a rate of two
	// consecutive reads flicks between nothing and a spike. A page can ask for longer.
	liveRate = 5 * time.Second
)

type snapshot struct {
	t time.Time
	v map[string]float64 // series key (name + sorted labels) → value
}

type liveStore struct {
	urls   []string
	mu     sync.Mutex
	snaps  []snapshot
	wanted time.Time
	on     bool
	names  map[string]bool // metric names the live pages use; others are not kept
	err    error
}

var (
	liveOnce sync.Once
	liveSt   *liveStore
)

// live returns the config's live store, created on first use with the metric names every
// live page refers to, so a /metrics page of thousands of series costs a few dozen in memory.
func (c *Config) live() *liveStore {
	liveOnce.Do(func() {
		liveSt = &liveStore{urls: c.liveURLs(), names: liveNames(c)}
	})
	return liveSt
}

func liveNames(c *Config) map[string]bool {
	names := map[string]bool{}
	for _, p := range c.pages() {
		if !p.Live {
			continue
		}
		for _, s := range p.Series {
			for _, n := range metricNames(bare(s.Expr)) {
				names[n] = true
			}
		}
	}
	return names
}

// reloadLive points a running live store at a reloaded catalogue's live pages. The URL is
// kept: changing live_source or live_sources needs a restart (the reader may be mid-request).
func reloadLive(c *Config) {
	if liveSt == nil {
		return
	}
	n := liveNames(c)
	liveSt.mu.Lock()
	liveSt.names = n
	liveSt.mu.Unlock()
}

var nameRe = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)

// metricNames picks the identifiers in an expression that are not functions or keywords.
func metricNames(expr string) []string {
	var out []string
	for _, n := range nameRe.FindAllString(stripQuoted(expr), -1) {
		switch n {
		case "rate", "sum", "avg", "max", "min", "by", "without", "or", "vector", "histogram_quantile", "le":
			continue
		}
		out = append(out, n)
	}
	return out
}

func stripQuoted(s string) string {
	var b strings.Builder
	in := false
	for i := 0; i < len(s); i++ {
		if s[i] == '"' && (i == 0 || s[i-1] != '\\') {
			in = !in
			continue
		}
		if !in {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// touch records a live request and starts the reader if it is idle.
func (ls *liveStore) touch() {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.wanted = time.Now()
	if ls.on {
		return
	}
	ls.on = true
	go ls.run()
}

func (ls *liveStore) run() {
	tick := time.NewTicker(liveEvery)
	defer tick.Stop()
	for {
		ls.read()
		<-tick.C
		ls.mu.Lock()
		if time.Since(ls.wanted) > liveIdle {
			ls.on, ls.snaps = false, nil // nobody is watching: stop, and let the memory go
			ls.mu.Unlock()
			return
		}
		ls.mu.Unlock()
	}
}

var liveClient = &http.Client{Timeout: 900 * time.Millisecond}

func (ls *liveStore) read() {
	ls.mu.Lock()
	urls, names := ls.urls, ls.names
	ls.mu.Unlock()
	snap := snapshot{t: time.Now(), v: map[string]float64{}}
	parts := make([]map[string]float64, len(urls))
	errs := make([]error, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls { // every source at once, so one slow exporter does not delay the rest
		wg.Add(1)
		go func() {
			defer wg.Done()
			parts[i], errs[i] = readSource(u, names)
		}()
	}
	wg.Wait()
	var err error
	ok := 0
	for i := range urls {
		if errs[i] != nil {
			err = errs[i]
			continue
		}
		ok++
		for k, v := range parts[i] {
			snap.v[k] = v
		}
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ok == 0 { // a source that fails leaves only its own pages short; all failing is an error
		ls.err = err
		return
	}
	ls.err = nil
	ls.snaps = append(ls.snaps, snap)
	cut := 0
	for cut < len(ls.snaps) && time.Since(ls.snaps[cut].t) > liveKeep {
		cut++
	}
	ls.snaps = ls.snaps[cut:]
}

// readSource reads one /metrics page, keeping only the metric names live pages use.
func readSource(url string, names map[string]bool) (map[string]float64, error) {
	resp, err := liveClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := map[string]float64{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name, labels, val, ok := parseSample(line)
		if !ok || !names[name] {
			continue
		}
		out[seriesKey(name, labels)] = val
	}
	return out, sc.Err()
}

// parseSample reads one exposition-format line: name{a="b",c="d"} value [timestamp].
func parseSample(line string) (string, map[string]string, float64, bool) {
	labels := map[string]string{}
	i := strings.IndexAny(line, "{ ")
	if i < 0 {
		return "", nil, 0, false
	}
	name, rest := line[:i], line[i:]
	if rest[0] == '{' {
		j := 1
		for j < len(rest) && rest[j] != '}' {
			eq := strings.IndexByte(rest[j:], '=')
			if eq < 0 {
				return "", nil, 0, false
			}
			k := strings.TrimSpace(strings.TrimLeft(rest[j:j+eq], ","))
			j += eq + 1
			if j >= len(rest) || rest[j] != '"' {
				return "", nil, 0, false
			}
			j++
			var v strings.Builder
			for j < len(rest) && rest[j] != '"' {
				if rest[j] == '\\' && j+1 < len(rest) {
					j++
					switch rest[j] {
					case 'n':
						v.WriteByte('\n')
					default:
						v.WriteByte(rest[j])
					}
				} else {
					v.WriteByte(rest[j])
				}
				j++
			}
			labels[k] = v.String()
			j++ // closing quote
			for j < len(rest) && (rest[j] == ',' || rest[j] == ' ') {
				j++
			}
		}
		if j >= len(rest) {
			return "", nil, 0, false
		}
		rest = rest[j+1:]
	}
	f := strings.Fields(rest)
	if len(f) == 0 {
		return "", nil, 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return "", nil, 0, false
	}
	return name, labels, v, true
}

func seriesKey(name string, labels map[string]string) string {
	ks := make([]string, 0, len(labels))
	for k := range labels {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var b strings.Builder
	b.WriteString(name)
	for _, k := range ks {
		b.WriteString("\x00" + k + "=" + labels[k])
	}
	return b.String()
}

// dropName strips the metric name from a series key, leaving its labels.
func dropName(key string) string {
	if i := strings.IndexByte(key, 0); i >= 0 {
		return key[i:]
	}
	return ""
}

func splitKey(key string) (string, map[string]string) {
	parts := strings.Split(key, "\x00")
	labels := map[string]string{}
	for _, p := range parts[1:] {
		if i := strings.IndexByte(p, '='); i > 0 {
			labels[p[:i]] = p[i+1:]
		}
	}
	return parts[0], labels
}

// series evaluates each query over the snapshots in memory. It errors (and the caller falls
// back to the database) when an expression is outside the subset or nothing has been read yet.
func (ls *liveStore) series(qs []q, win time.Duration) ([]series, time.Time, error) {
	ls.touch()
	ls.mu.Lock()
	snaps := append([]snapshot(nil), ls.snaps...)
	rerr := ls.err
	ls.mu.Unlock()
	if len(snaps) < 2 {
		if rerr != nil {
			return nil, time.Time{}, rerr
		}
		return nil, time.Time{}, fmt.Errorf("live: warming up")
	}
	var out []series
	for _, qq := range qs {
		n, err := parseLiveWindow(bare(qq.expr), win) // $__rate is a database window; live, rate() looks back win
		if err != nil {
			return nil, time.Time{}, err
		}
		got := map[string]*series{}
		var order []string
		for i := 1; i < len(snaps); i++ {
			vec := n.eval(snaps, i)
			for k, v := range vec {
				if math.IsNaN(v) || math.IsInf(v, 0) {
					continue
				}
				s := got[k]
				if s == nil {
					name := qq.name
					if qq.label != "" {
						_, l := splitKey(k)
						name = l[qq.label]
					}
					s = &series{name: name}
					got[k] = s
					order = append(order, k)
				}
				s.t = append(s.t, float64(snaps[i].t.UnixMilli())/1000)
				s.v = append(s.v, v)
			}
		}
		sort.Strings(order)
		for _, k := range order {
			out = append(out, *got[k])
		}
	}
	return out, snaps[len(snaps)-1].t, nil
}

// ── the expression subset ──────────────────────────────────────────────────────────

type vec map[string]float64 // series key → value; key "" is a scalar or an aggregate

type node interface{ eval(s []snapshot, i int) vec }

type num float64

func (n num) eval([]snapshot, int) vec { return vec{"": float64(n)} }

type matcher struct {
	label, op, val string
	re             *regexp.Regexp
}

type selector struct {
	name string
	ms   []matcher
}

func (sel selector) eval(s []snapshot, i int) vec {
	out := vec{}
	for k, v := range s[i].v {
		if sel.match(k) {
			out[k] = v
		}
	}
	return out
}

func (sel selector) match(key string) bool {
	name, labels := splitKey(key)
	if name != sel.name {
		return false
	}
	for _, m := range sel.ms {
		v := labels[m.label]
		switch m.op {
		case "=":
			if v != m.val {
				return false
			}
		case "!=":
			if v == m.val {
				return false
			}
		case "=~":
			if !m.re.MatchString(v) {
				return false
			}
		case "!~":
			if m.re.MatchString(v) {
				return false
			}
		}
	}
	return true
}

type rateOf struct {
	sel selector
	win time.Duration // look back this far; never less than the previous read
}

// eval is the rate from the oldest read inside the window to this one. Only the two ends are
// read, so a wider window costs nothing; a counter that went down (a reset) falls back to the
// last step alone, and a reset inside that step is skipped.
func (r rateOf) eval(s []snapshot, i int) vec {
	j := i - 1
	for j > 0 && s[i].t.Sub(s[j-1].t) <= r.win {
		j--
	}
	out := vec{}
	cur, old := r.sel.eval(s, i), r.sel.eval(s, j)
	var prev vec
	for k, v := range cur {
		if p, ok := old[k]; ok && v >= p {
			if dt := s[i].t.Sub(s[j].t).Seconds(); dt > 0 {
				out[k] = (v - p) / dt
			}
			continue
		}
		if prev == nil {
			prev = r.sel.eval(s, i-1)
		}
		if p, ok := prev[k]; ok && v >= p {
			if dt := s[i].t.Sub(s[i-1].t).Seconds(); dt > 0 {
				out[k] = (v - p) / dt
			}
		}
	}
	return out
}

type agg struct {
	op string
	in node
	by []string // group on these labels; none = one aggregate
}

func (a agg) eval(s []snapshot, i int) vec {
	groups := map[string][]float64{}
	for k, v := range a.in.eval(s, i) {
		g := ""
		if len(a.by) > 0 {
			_, l := splitKey(k)
			keep := map[string]string{}
			for _, b := range a.by {
				if x, ok := l[b]; ok {
					keep[b] = x
				}
			}
			g = seriesKey("", keep)
		}
		groups[g] = append(groups[g], v)
	}
	out := vec{}
	for g, vs := range groups {
		r := vs[0]
		for _, v := range vs[1:] {
			switch a.op {
			case "sum", "avg":
				r += v
			case "max":
				r = math.Max(r, v)
			case "min":
				r = math.Min(r, v)
			}
		}
		if a.op == "avg" {
			r /= float64(len(vs))
		}
		out[g] = r
	}
	return out
}

// histq is histogram_quantile, as Prometheus computes it: within each group of buckets
// (series alike but for "le"), find the bucket the rank falls in and interpolate linearly
// inside it. No observations in the step gives no value, which draws as a gap.
type histq struct {
	q  float64
	in node
}

func (h histq) eval(s []snapshot, i int) vec {
	type bucket struct{ le, n float64 }
	groups := map[string][]bucket{}
	for k, v := range h.in.eval(s, i) {
		_, l := splitKey(k)
		le, err := strconv.ParseFloat(l["le"], 64)
		if err != nil {
			continue
		}
		delete(l, "le")
		g := seriesKey("", l)
		groups[g] = append(groups[g], bucket{le, v})
	}
	out := vec{}
	for g, bs := range groups {
		sort.Slice(bs, func(a, b int) bool { return bs[a].le < bs[b].le })
		total := bs[len(bs)-1].n
		if !math.IsInf(bs[len(bs)-1].le, 1) || total <= 0 {
			continue
		}
		rank := h.q * total
		lo, below := 0.0, 0.0
		for j, b := range bs {
			if b.n >= rank {
				if math.IsInf(b.le, 1) {
					out[g] = bs[j-1].le // the top bucket has no upper edge
				} else {
					out[g] = lo + (b.le-lo)*(rank-below)/(b.n-below)
				}
				break
			}
			lo, below = b.le, b.n
		}
	}
	return out
}

type binop struct {
	op   byte
	l, r node
}

func (b binop) eval(s []snapshot, i int) vec {
	l, r := b.l.eval(s, i), b.r.eval(s, i)
	out := vec{}
	apply := func(x, y float64) float64 {
		switch b.op {
		case '+':
			return x + y
		case '-':
			return x - y
		case '*':
			return x * y
		case '/':
			return x / y
		case '^':
			return math.Pow(x, y)
		}
		return math.NaN()
	}
	ls, lok := l[""]
	rs, rok := r[""]
	switch {
	case lok && len(l) == 1:
		for k, v := range r {
			out[k] = apply(ls, v)
		}
	case rok && len(r) == 1:
		for k, v := range l {
			out[k] = apply(v, rs)
		}
	default: // series with series: match on labels, not the metric name, as PromQL does
		rs := make(map[string]float64, len(r))
		for k, w := range r {
			rs[dropName(k)] = w
		}
		for k, v := range l {
			if w, ok := rs[dropName(k)]; ok {
				out[dropName(k)] = apply(v, w)
			}
		}
	}
	return out
}

// orOf is PromQL's `or`: every series on the left, plus those on the right whose key the
// left lacks. For two aggregates it reads "the left if it has a value, else the right".
type orOf struct{ l, r node }

func (o orOf) eval(s []snapshot, i int) vec {
	out := vec{}
	for k, v := range o.l.eval(s, i) {
		if !math.IsNaN(v) {
			out[k] = v
		}
	}
	for k, v := range o.r.eval(s, i) {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	return out
}

// parseLive is a small recursive-descent parser for the subset above, plus `or`, `^` and
// vector(n), which the packs use for fallbacks between exporter versions and for units.
func parseLive(expr string) (node, error) { return parseLiveWindow(expr, liveRate) }

// parseLiveWindow is parseLive with rate() looking back win instead of liveRate.
func parseLiveWindow(expr string, win time.Duration) (node, error) {
	p := &lparser{s: expr, win: win}
	n, err := p.or()
	if err != nil {
		return nil, err
	}
	p.space()
	if p.i != len(p.s) {
		return nil, fmt.Errorf("live: unsupported at %q", p.s[p.i:])
	}
	return n, nil
}

type lparser struct {
	s   string
	i   int
	win time.Duration // rate() window
}

func (p *lparser) space() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == '\n') {
		p.i++
	}
}

func (p *lparser) or() (node, error) {
	l, err := p.expr()
	if err != nil {
		return nil, err
	}
	for {
		p.space()
		rest := p.s[p.i:]
		if strings.HasPrefix(rest, "or") && len(rest) > 2 && (rest[2] == ' ' || rest[2] == '(') {
			p.i += 2
			r, err := p.expr()
			if err != nil {
				return nil, err
			}
			l = orOf{l, r}
			continue
		}
		return l, nil
	}
}

func (p *lparser) expr() (node, error) {
	l, err := p.term()
	if err != nil {
		return nil, err
	}
	for {
		p.space()
		if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
			op := p.s[p.i]
			p.i++
			r, err := p.term()
			if err != nil {
				return nil, err
			}
			l = binop{op, l, r}
			continue
		}
		return l, nil
	}
}

func (p *lparser) term() (node, error) {
	l, err := p.power()
	if err != nil {
		return nil, err
	}
	for {
		p.space()
		if p.i < len(p.s) && (p.s[p.i] == '*' || p.s[p.i] == '/') {
			op := p.s[p.i]
			p.i++
			r, err := p.power()
			if err != nil {
				return nil, err
			}
			l = binop{op, l, r}
			continue
		}
		return l, nil
	}
}

// power binds tighter than * and /, and to the right, as in PromQL: 2^3^2 is 2^9.
func (p *lparser) power() (node, error) {
	l, err := p.factor()
	if err != nil {
		return nil, err
	}
	p.space()
	if p.i < len(p.s) && p.s[p.i] == '^' {
		p.i++
		r, err := p.power()
		if err != nil {
			return nil, err
		}
		return binop{'^', l, r}, nil
	}
	return l, nil
}

// byClause reads an optional "by (a, b)". "without" is outside the subset.
func (p *lparser) byClause() ([]string, bool, error) {
	p.space()
	rest := p.s[p.i:]
	if strings.HasPrefix(rest, "without") {
		return nil, false, fmt.Errorf("live: without is unsupported")
	}
	if !strings.HasPrefix(rest, "by") || len(rest) < 3 || (rest[2] != ' ' && rest[2] != '(') {
		return nil, false, nil
	}
	p.i += 2
	p.space()
	if p.i >= len(p.s) || p.s[p.i] != '(' {
		return nil, false, fmt.Errorf("live: by needs (")
	}
	p.i++
	var labels []string
	for {
		p.space()
		if p.i < len(p.s) && p.s[p.i] == ')' {
			p.i++
			p.space()
			return labels, true, nil
		}
		l := nameRe.FindString(p.s[p.i:])
		if l == "" || !strings.HasPrefix(p.s[p.i:], l) {
			return nil, false, fmt.Errorf("live: bad label in by at %q", p.s[p.i:])
		}
		labels = append(labels, l)
		p.i += len(l)
		p.space()
		if p.i < len(p.s) && p.s[p.i] == ',' {
			p.i++
		}
	}
}

var numRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?`)

func (p *lparser) factor() (node, error) {
	p.space()
	if p.i >= len(p.s) {
		return nil, fmt.Errorf("live: unexpected end")
	}
	if p.s[p.i] == '(' {
		p.i++
		n, err := p.or()
		if err != nil {
			return nil, err
		}
		p.space()
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return nil, fmt.Errorf("live: missing )")
		}
		p.i++
		return n, nil
	}
	if m := numRe.FindString(p.s[p.i:]); m != "" {
		p.i += len(m)
		v, _ := strconv.ParseFloat(m, 64)
		return num(v), nil
	}
	id := nameRe.FindString(p.s[p.i:])
	if id == "" || !strings.HasPrefix(p.s[p.i:], id) {
		return nil, fmt.Errorf("live: unsupported at %q", p.s[p.i:])
	}
	p.i += len(id)
	p.space()
	switch id {
	case "vector":
		if p.i >= len(p.s) || p.s[p.i] != '(' {
			return nil, fmt.Errorf("live: vector needs (")
		}
		p.i++
		p.space()
		m := numRe.FindString(p.s[p.i:])
		if m == "" {
			return nil, fmt.Errorf("live: vector takes a number")
		}
		p.i += len(m)
		p.space()
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return nil, fmt.Errorf("live: vector: missing )")
		}
		p.i++
		v, _ := strconv.ParseFloat(m, 64)
		return num(v), nil
	case "histogram_quantile":
		if p.i >= len(p.s) || p.s[p.i] != '(' {
			return nil, fmt.Errorf("live: histogram_quantile needs (")
		}
		p.i++
		p.space()
		m := numRe.FindString(p.s[p.i:])
		if m == "" {
			return nil, fmt.Errorf("live: histogram_quantile takes a number first")
		}
		p.i += len(m)
		p.space()
		if p.i >= len(p.s) || p.s[p.i] != ',' {
			return nil, fmt.Errorf("live: histogram_quantile: missing ,")
		}
		p.i++
		inner, err := p.or()
		if err != nil {
			return nil, err
		}
		p.space()
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return nil, fmt.Errorf("live: histogram_quantile: missing )")
		}
		p.i++
		q, _ := strconv.ParseFloat(m, 64)
		return histq{q, inner}, nil
	case "rate", "sum", "avg", "max", "min":
		var by []string
		if id != "rate" { // sum by (a, b) (x)
			if b, ok, err := p.byClause(); err != nil {
				return nil, err
			} else if ok {
				by = b
			}
		}
		if p.i >= len(p.s) || p.s[p.i] != '(' {
			return nil, fmt.Errorf("live: %s needs (", id)
		}
		p.i++
		inner, err := p.or()
		if err != nil {
			return nil, err
		}
		p.space()
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return nil, fmt.Errorf("live: %s: unsupported (a range window?)", id)
		}
		p.i++
		if id == "rate" {
			sel, ok := inner.(selector)
			if !ok {
				return nil, fmt.Errorf("live: rate takes a metric")
			}
			return rateOf{sel, p.win}, nil
		}
		if by == nil { // sum(x) by (a, b)
			if b, ok, err := p.byClause(); err != nil {
				return nil, err
			} else if ok {
				by = b
			}
		}
		return agg{id, inner, by}, nil
	}
	sel := selector{name: id}
	if p.i < len(p.s) && p.s[p.i] == '{' {
		p.i++
		for {
			p.space()
			if p.i < len(p.s) && p.s[p.i] == '}' {
				p.i++
				break
			}
			lab := nameRe.FindString(p.s[p.i:])
			if lab == "" {
				return nil, fmt.Errorf("live: bad label at %q", p.s[p.i:])
			}
			p.i += len(lab)
			p.space()
			op := ""
			for _, o := range []string{"=~", "!~", "!=", "="} {
				if strings.HasPrefix(p.s[p.i:], o) {
					op = o
					break
				}
			}
			if op == "" {
				return nil, fmt.Errorf("live: bad matcher at %q", p.s[p.i:])
			}
			p.i += len(op)
			p.space()
			if p.i >= len(p.s) || p.s[p.i] != '"' {
				return nil, fmt.Errorf("live: matcher value must be quoted")
			}
			end := strings.IndexByte(p.s[p.i+1:], '"')
			if end < 0 {
				return nil, fmt.Errorf("live: unclosed quote")
			}
			val := p.s[p.i+1 : p.i+1+end]
			p.i += end + 2
			m := matcher{label: lab, op: op, val: val}
			if op == "=~" || op == "!~" {
				re, err := regexp.Compile("^(?:" + val + ")$")
				if err != nil {
					return nil, err
				}
				m.re = re
			}
			sel.ms = append(sel.ms, m)
			p.space()
			if p.i < len(p.s) && p.s[p.i] == ',' {
				p.i++
			}
		}
	}
	return sel, nil
}
