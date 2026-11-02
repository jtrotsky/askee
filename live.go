package main

// The live range, without a database. While someone is watching, askee reads the exporter's
// /metrics page (config "live_source") once a second and keeps the last five minutes in
// memory: nothing is stored, and nothing is read when nobody is looking. Live pages are then
// computed here, from a small subset of PromQL that covers what a live chart needs:
//
//	metric{label="v", label=~"re", label!="v", label!~"re"}
//	rate(...)                    per second, from consecutive reads; counter resets skipped
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
)

type snapshot struct {
	t time.Time
	v map[string]float64 // series key (name + sorted labels) → value
}

type liveStore struct {
	url    string
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
		liveSt = &liveStore{url: c.LiveSource, names: map[string]bool{}}
		for _, p := range c.pages() {
			if !p.Live {
				continue
			}
			for _, s := range p.Series {
				for _, n := range metricNames(s.Expr) {
					liveSt.names[n] = true
				}
			}
		}
	})
	return liveSt
}

var nameRe = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)

// metricNames picks the identifiers in an expression that are not functions or keywords.
func metricNames(expr string) []string {
	var out []string
	for _, n := range nameRe.FindAllString(stripQuoted(expr), -1) {
		switch n {
		case "rate", "sum", "avg", "max", "min", "by", "without":
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
	resp, err := liveClient.Get(ls.url)
	if err != nil {
		ls.mu.Lock()
		ls.err = err
		ls.mu.Unlock()
		return
	}
	defer resp.Body.Close()
	snap := snapshot{t: time.Now(), v: map[string]float64{}}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name, labels, val, ok := parseSample(line)
		if !ok || !ls.names[name] {
			continue
		}
		snap.v[seriesKey(name, labels)] = val
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.err = nil
	ls.snaps = append(ls.snaps, snap)
	cut := 0
	for cut < len(ls.snaps) && time.Since(ls.snaps[cut].t) > liveKeep {
		cut++
	}
	ls.snaps = ls.snaps[cut:]
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
func (ls *liveStore) series(qs []q) ([]series, time.Time, error) {
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
		n, err := parseLive(qq.expr)
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

type rateOf struct{ sel selector }

func (r rateOf) eval(s []snapshot, i int) vec {
	out := vec{}
	cur, prev := r.sel.eval(s, i), r.sel.eval(s, i-1)
	dt := s[i].t.Sub(s[i-1].t).Seconds()
	if dt <= 0 {
		return out
	}
	for k, v := range cur {
		if p, ok := prev[k]; ok && v >= p { // a drop is a counter reset: skip that step
			out[k] = (v - p) / dt
		}
	}
	return out
}

type agg struct {
	op string
	in node
}

func (a agg) eval(s []snapshot, i int) vec {
	in := a.in.eval(s, i)
	if len(in) == 0 {
		return vec{}
	}
	var r float64
	first := true
	for _, v := range in {
		switch {
		case first:
			r = v
		case a.op == "sum" || a.op == "avg":
			r += v
		case a.op == "max":
			r = math.Max(r, v)
		case a.op == "min":
			r = math.Min(r, v)
		}
		first = false
	}
	if a.op == "avg" {
		r /= float64(len(in))
	}
	return vec{"": r}
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
	default: // series with series: match on the same key
		for k, v := range l {
			if w, ok := r[k]; ok {
				out[k] = apply(v, w)
			}
		}
	}
	return out
}

// parseLive is a small recursive-descent parser for the subset above.
func parseLive(expr string) (node, error) {
	p := &lparser{s: expr}
	n, err := p.expr()
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
	s string
	i int
}

func (p *lparser) space() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == '\n') {
		p.i++
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
	l, err := p.factor()
	if err != nil {
		return nil, err
	}
	for {
		p.space()
		if p.i < len(p.s) && (p.s[p.i] == '*' || p.s[p.i] == '/') {
			op := p.s[p.i]
			p.i++
			r, err := p.factor()
			if err != nil {
				return nil, err
			}
			l = binop{op, l, r}
			continue
		}
		return l, nil
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
		n, err := p.expr()
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
	case "rate", "sum", "avg", "max", "min":
		if p.i >= len(p.s) || p.s[p.i] != '(' {
			return nil, fmt.Errorf("live: %s needs (", id)
		}
		p.i++
		inner, err := p.expr()
		if err != nil {
			return nil, err
		}
		p.space()
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return nil, fmt.Errorf("live: %s: unsupported (a range or by clause?)", id)
		}
		p.i++
		if id == "rate" {
			sel, ok := inner.(selector)
			if !ok {
				return nil, fmt.Errorf("live: rate takes a metric")
			}
			return rateOf{sel}, nil
		}
		return agg{id, inner}, nil
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
