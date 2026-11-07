package main

// The page catalogue is DATA, not code: a JSON file lists groups and pages, so the same binary
// serves any Prometheus-compatible TSDB (VictoriaMetrics, Prometheus). The standard charts come
// as packs inside the binary (packs.go); examples/node-exporter.json is a starting point.
//
// Every expression comes from the file. Neither the web API nor the CLI accepts query text from
// a caller — only a group id and one of the fixed ranges — so the dashboard cannot be used to
// run arbitrary queries against the TSDB.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Title        string     `json:"title"`
	VM           string     `json:"vm"`
	Snapshot     string     `json:"snapshot"`      // markdown file of `### key` sections (root-only figures)
	Sizes        string     `json:"sizes"`         // a sizes table, TARGET LOCAL then one column per store
	PricePerTB   float64    `json:"price_per_tb"`  // for the cost column of the sizes table
	PriceLabel   string     `json:"price_label"`   // e.g. "cold $/mo"
	Thermal      [2]float64 `json:"thermal_range"` // °C mapped to the cool→hot ramp
	DefaultRange string     `json:"default_range"`
	DefaultTheme string     `json:"default_theme"`
	Events       []EventCfg `json:"events"` // drawn on every line chart: "what happened here?"
	// LiveSource is an exporter's /metrics URL (e.g. node_exporter, optionally with
	// collect[] filters). On the live range, live pages are computed from it directly —
	// read every second while someone is watching, five minutes kept in memory, nothing
	// stored. All other ranges read the database as usual.
	LiveSource string `json:"live_source"`
	// LiveSources adds more /metrics URLs read the same way (e.g. a DNS server's own), so
	// pages over another exporter's metrics can be live too. Each is read every second.
	LiveSources []string `json:"live_sources"`
	// SizeLabels names the columns of the sizes file in plain words, e.g.
	// {"NAS": "home NAS", "COLD": "cold cloud"}. Unnamed columns show lower-cased.
	SizeLabels map[string]string `json:"size_labels"`
	// Packs names the built-in chart packs to include (packs.go). Absent with no pages of
	// its own, a config gets every pack; pages the database cannot fill are dropped.
	Packs          []string  `json:"packs"`
	Hide           []string  `json:"hide"`            // page ids to leave out, e.g. a pack page
	NodeFilter     string    `json:"node_filter"`     // label matcher for $node, e.g. instance="web1:9100"
	ScrapeInterval string    `json:"scrape_interval"` // the database's; sets the rate() window (default 60s)
	Groups         []Group   `json:"groups"`
	Pages          []PageCfg `json:"pages"`
}

type Group struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// EventCfg names a metric whose VALUE is a unix timestamp (node_boot_time_seconds,
// a job's last-run time): every distinct value inside the window becomes a labelled event.
type EventCfg struct {
	Label string `json:"label"`
	Expr  string `json:"expr"`
}

// block is one row of a block chart: a bar built from named segments, optionally inside a
// known total (capacity), with the row's number printed at its end.
type block struct {
	Label string  `json:"label"`
	Segs  []seg   `json:"segs"`
	Total float64 `json:"total,omitempty"` // capacity: the whole; the rest is drawn as free
	Text  string  `json:"text"`
}

type seg struct {
	Name string  `json:"name"`
	V    float64 `json:"v"`
}

type event struct {
	T     float64 `json:"t"`
	Label string  `json:"label"`
}

type SeriesCfg struct {
	Expr  string `json:"expr"`
	Name  string `json:"name"`  // fixed series name
	Label string `json:"label"` // or: name each result by this label
	Lo    string `json:"lo"`    // line: the bottom of a range drawn behind the line (min_over_time…)
	Hi    string `json:"hi"`    // line: its top; both may use $__step, the width of one point
}

type RefCfg struct {
	Value *float64 `json:"value"` // a fixed reference line…
	Expr  string   `json:"expr"`  // …or one read from an instant query (max of results)
	Label string   `json:"label"`
}

type PageCfg struct {
	ID       string      `json:"id"`
	Group    string      `json:"group"`
	Title    string      `json:"title"`
	Info     string      `json:"info"`     // the "so what": one sentence, behind the ⓘ beside the title
	Subtitle string      `json:"subtitle"` // the old name for info, still read
	Kind     string      `json:"kind"`     // line | hour | instant | runs | snapshot | sizes | clock
	Unit     string      `json:"unit"`
	Range    string      `json:"range"`       // fixed range for this page ("" = the selected one)
	MinRange string      `json:"min_range"`   // follows the selected range, but never shorter (daily data)
	Requires string      `json:"requires"`    // a query that must return something, or the page is left out
	Live     bool        `json:"live"`        // fast-moving: follows the "live" range (others show 1h)
	LiveWin  string      `json:"live_window"` // live rate() looks back this far ("30s"); default liveRate
	Log      bool        `json:"log"`         // log scale: for values spanning orders of magnitude (latency)
	Capacity string      `json:"capacity"`    // capacity kind: expr for the TOTAL; expr is the USED part
	YMin     *float64    `json:"ymin"`
	YMax     *float64    `json:"ymax"`
	Series   []SeriesCfg `json:"series"`
	Refs     []RefCfg    `json:"refs"`
	Thermal  bool        `json:"thermal"` // colour the line by absolute value (°C)
	Stack    bool        `json:"stack"`   // series are parts of a whole: draw stacked
	Split    bool        `json:"split"`   // small multiples: one panel per series, each with its own scale
	Expr     string      `json:"expr"`    // hour / instant / runs
	Label    string      `json:"label"`   // instant: label naming each bar
	Key      string      `json:"key"`     // runs: the per-run timestamp metric; snapshot: section key
	MinValue float64     `json:"min_value"`
	Limit    int         `json:"limit"`
	Note     string      `json:"note"`
	Span     [2]int      `json:"span"`     // [columns, rows] in the web grid; 0 = automatic
	Clock    *ClockCfg   `json:"clock"`    // clock: one row per day, time of day across (clock.go)
	Mark     string      `json:"mark"`     // days: "bar" (an amount from zero) or "dot" (a level); days.go
	Day      string      `json:"day"`      // days: how a day's hourly values reduce: max | sum | min | last
	Baseline int         `json:"baseline"` // days: shade the usual range over this many days before each
	Empty    string      `json:"empty"`    // said instead of "no data in this range", e.g. what the data waits for
}

type ref struct {
	V     float64 `json:"value"`
	Label string  `json:"label"`
}

type bar struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

type model struct {
	kind    string
	series  []series
	bars    []bar
	lines   []string
	refs    []ref
	unit    string
	yMin    *float64
	yMax    *float64
	thermal bool
	stack   bool
	split   bool
	log     bool
	rng     string // what the page shows: a range (see drawn), "now" or "snapshot"
	events  []event
	blocks  []block
	rows    []clockRow // clock: one per day
	mark    string     // days: bar | dot
	bands   []band     // a range behind a line (lo/hi) or a days page's usual range
	empty   string
	clock   ClockCfg
	note    string
	start   time.Time
	end     time.Time
	step    time.Duration
	err     error
}

type rng struct {
	name string
	span time.Duration
	step time.Duration
}

var ranges = []rng{
	// live: the last five minutes, refetched every few seconds and sliding with the clock.
	// Only pages marked "live" use it; the rest of the group shows the last hour.
	{"live", 5 * time.Minute, time.Second},
	{"1h", time.Hour, 30 * time.Second},
	{"6h", 6 * time.Hour, time.Minute},
	{"24h", 24 * time.Hour, 5 * time.Minute},
	{"7d", 7 * 24 * time.Hour, 30 * time.Minute},
	{"30d", 30 * 24 * time.Hour, 2 * time.Hour},
	{"90d", 90 * 24 * time.Hour, 6 * time.Hour},
}

func rangeByName(n string) (rng, bool) {
	for _, r := range ranges {
		if r.name == n {
			return r, true
		}
	}
	return ranges[2], false
}

type page struct {
	PageCfg
	cfg *Config
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return finishConfig(&c)
}

// finishConfig expands packs, fills defaults and checks every page.
func finishConfig(c *Config) (*Config, error) {
	if err := c.expandPacks(); err != nil {
		return nil, err
	}
	if c.ScrapeInterval != "" {
		d, err := time.ParseDuration(c.ScrapeInterval)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("scrape_interval %q: want a duration such as 15s", c.ScrapeInterval)
		}
		scrapeEvery = d
	}
	if c.Thermal == [2]float64{} {
		c.Thermal = [2]float64{30, 100}
	}
	if c.DefaultRange == "" {
		c.DefaultRange = "24h"
	}
	if c.DefaultTheme == "" {
		c.DefaultTheme = "auto" // newsprint on a light system, carbon on a dark one
	}
	if c.PriceLabel == "" {
		c.PriceLabel = "$/mo"
	}
	seen := map[string]bool{}
	for i := range c.Pages {
		p := &c.Pages[i]
		if p.Subtitle != "" {
			if p.Info != "" {
				return nil, fmt.Errorf("page %s: has both info and subtitle (the old name); keep info", p.ID)
			}
			p.Info, p.Subtitle = p.Subtitle, ""
		}
		if err := checkCopy(p); err != nil {
			return nil, err
		}
		if err := checkTime(page{PageCfg: *p, cfg: c}); err != nil {
			return nil, err
		}
		if seen[p.ID] {
			return nil, fmt.Errorf("page id %q is used twice", p.ID)
		}
		seen[p.ID] = true
		if p.ID == "" || p.Group == "" || p.Kind == "" {
			return nil, fmt.Errorf("page %d (%q): id, group and kind are required", i, p.Title)
		}
		if p.Range != "" && p.Range != "now" && p.Range != "snapshot" {
			if _, ok := rangeByName(p.Range); !ok {
				return nil, fmt.Errorf("page %s: unknown range %q", p.ID, p.Range)
			}
		}
		if p.MinRange != "" {
			if _, ok := rangeByName(p.MinRange); !ok {
				return nil, fmt.Errorf("page %s: unknown min_range %q", p.ID, p.MinRange)
			}
		}
		if p.Kind == "days" {
			if len(p.Series) == 0 {
				return nil, fmt.Errorf("page %s: a days page needs series", p.ID)
			}
			if p.Mark != "" && p.Mark != "bar" && p.Mark != "dot" {
				return nil, fmt.Errorf("page %s: mark is bar (an amount from zero) or dot (a level), not %q", p.ID, p.Mark)
			}
			if p.Day != "" && p.Day != "max" && p.Day != "sum" && p.Day != "min" && p.Day != "last" {
				return nil, fmt.Errorf("page %s: day is max, sum, min or last, not %q", p.ID, p.Day)
			}
			if p.Stack && p.Mark == "dot" {
				return nil, fmt.Errorf("page %s: only bars stack", p.ID)
			}
		} else if p.Mark != "" || p.Day != "" || p.Baseline != 0 {
			return nil, fmt.Errorf("page %s: mark, day and baseline belong to days pages", p.ID)
		}
		for _, s := range p.Series {
			if (s.Lo == "") != (s.Hi == "") || (s.Lo != "" && p.Kind != "line") {
				return nil, fmt.Errorf("page %s: lo and hi come as a pair, on line pages", p.ID)
			}
		}
		if p.Kind == "clock" {
			if p.Clock == nil || p.Expr == "" {
				return nil, fmt.Errorf("page %s: a clock page needs expr and clock", p.ID)
			}
			if err := p.Clock.validate(); err != nil {
				return nil, fmt.Errorf("page %s: %v", p.ID, err)
			}
		}
	}
	return c, nil
}

// The chart carries the data and words wait behind the ⓘ. A page's title is a few plain words
// with no commas or dashes, its info is one sentence with no semicolon or dash to splice a
// second onto it, and its
// note — printed under the chart, always visible — is a short label such as "last 30 days",
// never an explanation. A config that breaks either does not load (a live reload keeps the
// previous catalogue), so the rule holds whoever edits the file.
const maxNote = 50

var sentenceBreak = regexp.MustCompile(`[.!?]\s+\p{Lu}`)

func checkCopy(p *PageCfg) error {
	if strings.ContainsAny(p.Title, ",;:—–") {
		return fmt.Errorf("page %s: title is what, in a few plain words, with no commas or dashes (the range goes in the tag): %q", p.ID, p.Title)
	}
	if strings.ContainsAny(p.Info, ";—") {
		return fmt.Errorf("page %s: info has a semicolon or dash, which splices two sentences into one, so cut it to one: %q", p.ID, p.Info)
	}
	if sentenceBreak.MatchString(p.Info) {
		return fmt.Errorf("page %s: info must be one sentence (it sits behind the ⓘ): %q", p.ID, p.Info)
	}
	if n := len([]rune(p.Note)); n > maxNote || sentenceBreak.MatchString(p.Note) {
		return fmt.Errorf("page %s: note is %d characters; it shows under the chart, so keep it a label of %d or fewer and put explanations in info: %q", p.ID, n, maxNote, p.Note)
	}
	return nil
}

// checkTime keeps the picker honest: a windowed page never pins its own range, and a page
// marked live must be one the live evaluator can compute, or the tab's live mark would lie.
func checkTime(p page) error {
	if p.windowed() && p.Range != "" {
		return fmt.Errorf("page %s: covers a stretch of time, so it follows the range picker; drop \"range\" and set \"min_range\" to the shortest range it can show", p.ID)
	}
	if !p.windowed() && p.MinRange != "" {
		return fmt.Errorf("page %s: does not follow the range picker (a reading or a fixed pattern), so min_range means nothing here", p.ID)
	}
	if p.LiveWin != "" && !p.Live {
		return fmt.Errorf("page %s: live_window means nothing on a page that is not live", p.ID)
	}
	if p.Live {
		if p.Kind != "line" {
			return fmt.Errorf("page %s: only line pages can be live", p.ID)
		}
		if p.LiveWin != "" {
			if w, err := time.ParseDuration(p.LiveWin); err != nil || w < liveEvery || w > liveKeep/2 {
				return fmt.Errorf("page %s: live_window %q: want a duration from 1s to 2m, such as \"30s\"", p.ID, p.LiveWin)
			}
		}
		for _, s := range p.Series {
			if _, err := parseLive(bare(s.Expr)); err != nil {
				return fmt.Errorf("page %s: marked live, but %v in %q", p.ID, err, s.Expr)
			}
		}
	}
	return nil
}

func (c *Config) pages() []page {
	rank := map[string]int{}
	for i, g := range c.Groups {
		rank[g.ID] = i
	}
	var out []page
	for _, p := range c.Pages {
		out = append(out, page{p, c})
	}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Group] < rank[out[j].Group] })
	return out
}

func (c *Config) groupTitle(id string) string {
	for _, g := range c.Groups {
		if g.ID == id {
			return g.Title
		}
	}
	return id
}

// liveWindow is how far back rate() looks on the live range.
func (p page) liveWindow() time.Duration {
	if w, err := time.ParseDuration(p.LiveWin); err == nil {
		return w
	}
	return liveRate
}

// drawn is the range a page shows when r is selected: its own fixed Range if it has one;
// the last hour in the live view unless it is a live page; never shorter than its MinRange.
func (p page) drawn(r rng) rng {
	if f, ok := rangeByName(p.Range); ok {
		return f
	}
	if r.name == "live" && !p.Live {
		r, _ = rangeByName("1h")
	}
	if mr, ok := rangeByName(p.MinRange); ok && r.span < mr.span {
		r = mr
	}
	return r
}

// tag names the time a page's header claims when r is selected: the range for a windowed
// page, "now" for a reading of the present, "snapshot" for the snapshot file.
func (p page) tag(r rng) string {
	switch {
	case p.windowed():
		return p.drawn(r).name
	case p.Kind == "hour":
		return p.profileRange().name
	case p.Kind == "instant":
		return "now"
	}
	return "snapshot"
}

// ── tabs and their time ranges ──────────────────────────────────────────────────────
// A chart that covers a stretch of time (a windowed page) follows the range picker. One that
// shows a reading (a figure now, a snapshot) does not, and says so in its header. Each group
// has a floor: the longest of its windowed pages' shortest ranges. The picker offers nothing
// below it, so every windowed chart in a tab shows exactly the range the picker names. A
// group goes live only when every windowed page in it can; one that mixes live and delayed
// pages is "mixed", and none of its pages goes live.

// windowed reports whether a page covers a stretch of time chosen by the picker. An hour
// profile does not: it is a pattern over a fixed window (30 days unless its range says
// otherwise), so it neither follows the picker nor raises the tab's floor, and its header
// names its own window ("30d", plus "not live" on the live range).
func (p page) windowed() bool {
	switch p.Kind {
	case "line", "runs", "clock", "days":
		return true
	case "instant":
		return strings.Contains(p.Expr, "$__range")
	}
	return false
}

// floor is the shortest range a windowed page can show: live for a live line page (when a
// live source is set), else 1h, raised to its min_range.
func (p page) floor() rng {
	r, _ := rangeByName("1h")
	if p.Live && p.Kind == "line" && p.cfg != nil && len(p.cfg.liveURLs()) > 0 {
		r, _ = rangeByName("live")
	}
	if mr, ok := rangeByName(p.MinRange); ok && mr.span > r.span {
		r = mr
	}
	return r
}

type groupMode struct {
	Min   string `json:"min"`   // the shortest range every windowed chart can show; "" = readings only
	Live  bool   `json:"live"`  // every windowed chart can go live
	Mixed bool   `json:"mixed"` // some can and some cannot, so none does here
}

func modeOf(ps []page) groupMode {
	var g groupMode
	var lo rng
	n, anyLive := 0, false
	for _, p := range ps {
		// a days page widens itself to its min_range (a week of nights at the 24h setting, its
		// header tag saying so) rather than taking the 24h view away from the rest of its tab
		if !p.windowed() || p.Kind == "days" {
			continue
		}
		f := p.floor()
		anyLive = anyLive || f.name == "live"
		if n == 0 || f.span > lo.span {
			lo = f
		}
		n++
	}
	if n == 0 {
		return g
	}
	g.Min, g.Live = lo.name, lo.name == "live"
	g.Mixed = anyLive && !g.Live
	return g
}

func (c *Config) mode(group string) groupMode {
	var ps []page
	for _, p := range c.pages() {
		if p.Group == group {
			ps = append(ps, p)
		}
	}
	return modeOf(ps)
}

// clamp raises a selected range to the group's floor.
func (g groupMode) clamp(r rng) rng {
	if mr, ok := rangeByName(g.Min); ok && r.span < mr.span {
		return mr
	}
	return r
}

// profileRange is the fixed window an hour profile averages over.
func (p page) profileRange() rng {
	if r, ok := rangeByName(p.Range); ok {
		return r
	}
	r, _ := rangeByName("30d")
	return r
}

// liveURLs is every /metrics page the live store reads.
func (c *Config) liveURLs() []string {
	var out []string
	if c.LiveSource != "" {
		out = append(out, c.LiveSource)
	}
	return append(out, c.LiveSources...)
}

// withRange puts the drawn window where $__range stands, e.g. increase(x[$__range]).
func withRange(expr string, r rng) string {
	return strings.ReplaceAll(expr, "$__range", fmt.Sprintf("%ds", int(r.span.Seconds())))
}

// fetch builds the model for one page. r is the selected range; drawn says what the page
// shows instead (e.g. pool free space only means something over 90 days).
func (p page) fetch(sel rng) model {
	r := p.drawn(sel)
	m := model{kind: "line", unit: p.Unit, yMin: p.YMin, yMax: p.YMax, thermal: p.Thermal, stack: p.Stack, split: p.Split, log: p.Log, note: p.Note, rng: p.tag(sel), empty: p.Empty}
	end := time.Now()
	start := end.Add(-r.span)
	switch p.Kind {
	case "line":
		var qs []q
		for _, s := range p.Series {
			qs = append(qs, q{s.Expr, s.Name, s.Label})
		}
		if r.name == "live" && p.Live && len(p.cfg.liveURLs()) > 0 {
			if ss, last, err := p.cfg.live().series(qs, p.liveWindow()); err == nil && !math.IsInf(firstT(ss), 1) {
				m.series, m.start, m.end, m.step = ss, last.Add(-r.span), last, r.step
				m.refs = p.resolveRefs()
				// until five minutes have been read, the older part of the window comes from
				// the database (30 s points), so the chart is never a few lonely seconds
				if first := firstT(ss); first > float64(m.start.Unix())+30 {
					if old, err := rangeSeries(qs, m.start, time.Unix(int64(first), 0), 30*time.Second); err == nil {
						m.series = prepend(old, ss, first)
					}
				}
				break
			}
			// nothing read yet, or a metric the live source does not export: the database
		}
		m.series, m.err = rangeSeries(qs, start, end, r.step)
		m.start, m.end, m.step = start, end, r.step
		m.refs = p.resolveRefs()
		m.events = p.cfg.eventsIn(start, end)
		m.bands = p.lineBands(start, end, r.step)
	case "days":
		p.fetchDays(&m, start, end, time.Local)
	case "hour":
		m.kind = "bars"
		s, err := rangeSeries([]q{{expr: p.Expr, name: "v"}}, end.Add(-p.profileRange().span), end, time.Hour)
		m.err = err
		if len(s) > 0 {
			var sum [24]float64
			var n [24]int
			for i, t := range s[0].t {
				h := time.Unix(int64(t), 0).Hour()
				sum[h] += s[0].v[i]
				n[h]++
			}
			for h := 0; h < 24; h++ {
				if n[h] > 0 {
					m.bars = append(m.bars, bar{fmt.Sprintf("%02d:00", h), sum[h] / float64(n[h])})
				}
			}
		}
	case "instant":
		m.kind = "bars"
		res, err := instant(withRange(p.Expr, r))
		m.err = err
		sort.Slice(res, func(i, j int) bool { return res[i].value > res[j].value })
		for _, x := range res {
			if x.value >= p.MinValue {
				m.bars = append(m.bars, bar{x.metric[p.Label], x.value})
			}
		}
		if p.Limit > 0 && len(m.bars) > p.Limit {
			m.bars = m.bars[:p.Limit]
		}
	case "runs":
		// One bar per RUN, not per day: a gauge holds its last value until the next run, so a
		// daily max shows a midnight-spanning run twice. Dedupe on the run's own timestamp.
		m.kind = "bars"
		s, err := rangeSeries([]q{{expr: p.Expr, name: "v"}, {expr: p.Key, name: "k"}}, start, end, time.Hour)
		m.err = err
		if len(s) == 2 {
			seen := map[int64]bool{}
			for i := range s[1].t {
				k := int64(s[1].v[i])
				if k == 0 || seen[k] {
					continue
				}
				seen[k] = true
				if v, ok := valueAt(s[0], s[1].t[i]); ok {
					m.bars = append(m.bars, bar{time.Unix(k, 0).Format("Mon 02 Jan 15:04"), v})
				}
			}
		}
	case "snapshot":
		m.kind = "text"
		secs, when, err := readSnapshot(p.cfg.Snapshot)
		if err != nil {
			m.err = fmt.Errorf("no snapshot yet (%v)", err)
			break
		}
		s, ok := secs[p.Key]
		if !ok {
			m.err = fmt.Errorf("snapshot of %s has no %q section", when, p.Key)
			break
		}
		m.lines = cleanSnapshot(s[1])
		m.note = "snapshot " + when + noteSep(p.Note)
		if bars, unit, ok := barsFromText(m.lines); ok { // an hbar figure → real bars
			m.kind, m.bars, m.unit, m.lines = "bars", bars, unit, nil
		}
	case "clock":
		m.kind, m.clock = "clock", *p.Clock
		s, err := rangeSeries([]q{{expr: p.Expr, name: "v"}}, start, end, time.Duration(p.Clock.Cell)*time.Minute)
		m.err = err
		if err == nil && len(s) > 0 {
			m.rows = clockRows(s[0], *p.Clock, start, end, end, time.Local)
		}
		m.start, m.end = start, end
	case "sizes":
		m.kind = "blocks"
		m.blocks, m.unit, m.note, m.err = sizesBlocks(p.cfg)
	case "capacity":
		// a snapshot section whose lines read "name (USED of TOTAL UNIT …)" — zpool list's shape
		m.kind = "blocks"
		secs, when, err := readSnapshot(p.cfg.Snapshot)
		if err != nil {
			m.err = fmt.Errorf("no snapshot yet (%v)", err)
			break
		}
		s, ok := secs[p.Key]
		if !ok {
			m.err = fmt.Errorf("snapshot of %s has no %q section", when, p.Key)
			break
		}
		for _, l := range cleanSnapshot(s[1]) {
			x := capLine.FindStringSubmatch(l)
			if x == nil {
				continue
			}
			used, _ := strconv.ParseFloat(x[2], 64)
			total, _ := strconv.ParseFloat(x[3], 64)
			if total <= 0 {
				continue
			}
			m.unit = x[4]
			text := fmt.Sprintf("%s of %s %s", trimNum(used), trimNum(total), x[4])
			if m, ok := byteUnit[strings.ToUpper(x[4])]; ok { // each number in its own readable unit
				text = fmtBytes(used*m) + " of " + fmtBytes(total*m)
			}
			m.blocks = append(m.blocks, block{Label: x[1], Segs: []seg{{"used", used}}, Total: total,
				Text: fmt.Sprintf("%s · %.0f%%", text, 100*used/total)})
		}
		if len(m.blocks) == 0 {
			m.err = fmt.Errorf("no \"name (used of total unit)\" lines in %q", p.Key)
		}
		m.refs = p.resolveRefs()
		m.note = "snapshot " + when + noteSep(p.Note)
	default:
		m.err = fmt.Errorf("unknown page kind %q", p.Kind)
	}
	if m.kind == "blocks" {
		m.lines = blockLines(m.blocks, 32) // the terminal and -print draw these as text
	}
	if m.kind == "bars" && m.refs == nil {
		m.refs = p.resolveRefs() // e.g. "a missed night" on hours-since-backup
	}
	return m
}

func noteSep(n string) string {
	if n == "" {
		return ""
	}
	return " · " + n
}

func valueAt(s series, t float64) (float64, bool) {
	for i := range s.t {
		if s.t[i] == t {
			return s.v[i], true
		}
	}
	return 0, false
}

func (p page) resolveRefs() []ref {
	var out []ref
	for _, r := range p.Refs {
		if r.Value != nil {
			out = append(out, ref{*r.Value, r.Label})
			continue
		}
		if r.Expr != "" {
			if res, err := instant(r.Expr); err == nil && len(res) > 0 {
				mx := res[0].value
				for _, x := range res {
					mx = math.Max(mx, x.value)
				}
				out = append(out, ref{mx, r.Label})
			}
		}
	}
	return out
}

// ── snapshot (markdown `### key` sections written by a root-only collector) ───────────
func readSnapshot(path string) (map[string][2]string, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	st, _ := os.Stat(path)
	secs := map[string][2]string{}
	for _, blk := range strings.Split("\n"+string(b), "\n### ")[1:] {
		key, rest, _ := strings.Cut(blk, "\n")
		title, body, _ := strings.Cut(rest, "\n")
		secs[strings.TrimSpace(key)] = [2]string{strings.TrimSpace(title), body}
	}
	return secs, st.ModTime().Format("02 Jan 15:04"), nil
}

// cleanSnapshot drops fences and facts lines and turns markdown tables into aligned text.
func cleanSnapshot(body string) []string {
	var raw []string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "```") || strings.HasPrefix(l, "facts:") {
			continue
		}
		raw = append(raw, l)
	}
	for len(raw) > 0 && strings.TrimSpace(raw[len(raw)-1]) == "" {
		raw = raw[:len(raw)-1]
	}
	var table [][]string
	var out []string
	flush := func() {
		if table == nil {
			return
		}
		wd := map[int]int{}
		for _, row := range table {
			for i, c := range row {
				if n := len([]rune(c)); n > wd[i] {
					wd[i] = n
				}
			}
		}
		for _, row := range table {
			var sb strings.Builder
			for i, c := range row {
				sb.WriteString(c + strings.Repeat(" ", wd[i]-len([]rune(c))+2))
			}
			out = append(out, strings.TrimRight(sb.String(), " "))
		}
		table = nil
	}
	sep := regexp.MustCompile(`^\|[-| ]+\|$`)
	for _, l := range raw {
		if strings.HasPrefix(l, "|") {
			if sep.MatchString(strings.TrimSpace(l)) {
				continue
			}
			cells := strings.Split(strings.Trim(strings.TrimSpace(l), "|"), "|")
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}
			table = append(table, cells)
			continue
		}
		flush()
		out = append(out, l)
	}
	flush()
	return out
}

// ── sizes table: a 4-column text table TARGET LOCAL <REMOTE> <BILLED> in human units
// (B/KB/MB/GB/TB/PB); the header row names the two remote columns; cost is on the last ──
func parseHuman(s string) float64 {
	s = strings.TrimSpace(s)
	for _, u := range []struct {
		u string
		m float64
	}{{"PB", 1 << 50}, {"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(s, u.u) {
			f, _ := strconv.ParseFloat(strings.TrimSuffix(s, u.u), 64)
			return f * u.m
		}
	}
	return 0
}

func sizesTable(c *Config) ([]string, string, error) {
	b, err := os.ReadFile(c.Sizes)
	if err != nil {
		return nil, "", fmt.Errorf("no sizes yet (%v)", err)
	}
	st, _ := os.Stat(c.Sizes)
	type row struct {
		f              []string
		billed, remote float64
	}
	var rows []row
	hdr := []string{"TARGET", "LOCAL", "REMOTE", "BILLED"}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) == 4 && f[0] == "TARGET" {
			hdr = f
			continue
		}
		if len(f) != 4 || strings.HasPrefix(f[0], "─") {
			continue
		}
		rows = append(rows, row{f, parseHuman(f[3]), parseHuman(f[2])})
	}
	if len(rows) == 0 {
		return nil, "", fmt.Errorf("sizes file has no rows")
	}
	total := rows[len(rows)-1]
	rows = rows[:len(rows)-1]
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].billed+rows[i].remote > rows[j].billed+rows[j].remote })
	lines := []string{fmt.Sprintf("%-24s %9s %9s %9s %9s", "target", "local", strings.ToLower(hdr[2]), hdr[3], c.PriceLabel)}
	for _, r := range rows {
		if r.billed+r.remote < 50<<20 {
			continue
		}
		lines = append(lines, fmt.Sprintf("%-24s %9s %9s %9s %9.2f", r.f[0], r.f[1], r.f[2], r.f[3], r.billed/(1<<40)*c.PricePerTB))
	}
	lines = append(lines, "", fmt.Sprintf("%-24s %9s %9s %9s %9.2f", "TOTAL", total.f[1], total.f[2], total.f[3], total.billed/(1<<40)*c.PricePerTB))
	return lines, fmt.Sprintf("snapshot %s · $%.2f per TB-month · local '-' = not walked", st.ModTime().Format("02 Jan 15:04"), c.PricePerTB), nil
}

// barsFromText recognises a report-kit hbar figure ("label  ████▌  123.4 unit" per line) so a
// snapshot chart is drawn as real bars, with the theme's gradient, instead of as text.
var meterBox = regexp.MustCompile(`\s*\[[█▉▊▋▌▍▎▏─#.\s]*\]`)
var hbarLine = regexp.MustCompile(`^(.+?)\s+[█▉▊▋▌▍▎▏]*\s+(-?[\d.]+)\s*(\S*)\s*$`)

func barsFromText(lines []string) ([]bar, string, bool) {
	var bars []bar
	unit := ""
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if !strings.ContainsAny(l, "█▉▊▋▌▍▎▏") && !hbarLine.MatchString(l) {
			return nil, "", false
		}
		m := hbarLine.FindStringSubmatch(l)
		if m == nil {
			return nil, "", false
		}
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			return nil, "", false
		}
		label := strings.TrimSpace(meterBox.ReplaceAllString(m[1], "")) // a meter's [████────] is not part of the label
		bars = append(bars, bar{label, v})
		unit = m[3]
	}
	return bars, unit, len(bars) > 0
}

// eventsIn returns the configured events whose timestamps fall inside [start, end].
func (c *Config) eventsIn(start, end time.Time) []event {
	key := fmt.Sprintf("%d|%d", start.Unix()/60, end.Unix()/60)
	evMu.Lock()
	if e, ok := evCache[key]; ok {
		evMu.Unlock()
		return e
	}
	evMu.Unlock()
	out := c.eventsQuery(start, end)
	evMu.Lock()
	if len(evCache) > 64 {
		evCache = map[string][]event{}
	}
	evCache[key] = out
	evMu.Unlock()
	return out
}

var (
	evMu    sync.Mutex
	evCache = map[string][]event{}
)

func (c *Config) eventsQuery(start, end time.Time) []event {
	var out []event
	for _, e := range c.Events {
		res, err := rangeSeries([]q{{expr: e.Expr, name: e.Label}}, start, end, time.Hour)
		if err != nil {
			continue
		}
		// a boot time read off a live clock jitters by a second or two between scrapes:
		// values within five minutes of one already seen are the same event
		var seen []int64
	next:
		for _, s := range res {
			for _, v := range s.v {
				t := int64(v)
				if t < start.Unix() || t > end.Unix() {
					continue
				}
				for _, o := range seen {
					if t-o < 300 && o-t < 300 {
						continue next
					}
				}
				seen = append(seen, t)
				out = append(out, event{float64(t), e.Label})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out
}

var capLine = regexp.MustCompile(`^\s*(\S+)\s+\(([\d.]+) of ([\d.]+) (\w+)`)

func trimNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// sizesBlocks turns the backup sizes table (TARGET LOCAL NAS COLD, human sizes) into one block
// row per store, each split into the biggest targets plus "other", so the reader sees both
// how much each store holds and what fills it.
func sizesBlocks(c *Config) ([]block, string, string, error) {
	b, err := os.ReadFile(c.Sizes)
	if err != nil {
		return nil, "", "", fmt.Errorf("no sizes yet (%v)", err)
	}
	st, _ := os.Stat(c.Sizes)
	hdr := []string{"TARGET", "LOCAL", "REMOTE", "BILLED"}
	type row struct {
		name string
		v    [3]float64
	}
	var rows []row
	var estimated []string
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) == 4 && f[0] == "TARGET" {
			hdr = f
			continue
		}
		if len(f) != 4 || strings.HasPrefix(f[0], "─") {
			continue
		}
		r := row{name: f[0]}
		for i := 0; i < 3; i++ {
			r.v[i] = parseHuman(f[i+1]) / 1e9 // GB
		}
		if f[0] == "TOTAL" {
			continue // summed from the rows, so an estimated row is counted too
		}
		if f[1] == "-" && r.v[1] > 0 {
			// too big to walk nightly: the NAS copy is the best figure for what is here
			r.v[0] = r.v[1]
			estimated = append(estimated, r.name)
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		return nil, "", "", fmt.Errorf("sizes file has no rows")
	}
	// the same top targets in every row, so a segment means the same thing all the way down
	sort.SliceStable(rows, func(i, j int) bool {
		return math.Max(rows[i].v[1], rows[i].v[2]) > math.Max(rows[j].v[1], rows[j].v[2])
	})
	const top = 5
	names := map[string]string{"LOCAL": "on this box", "REMOTE": "remote", "BILLED": "billed"}
	for k, v := range c.SizeLabels { // a store's plain name, e.g. "NAS": "home NAS"
		names[strings.ToUpper(k)] = v
	}
	var out []block
	for col := 0; col < 3; col++ {
		var bl block
		bl.Label = names[hdr[col+1]]
		if bl.Label == "" {
			bl.Label = strings.ToLower(hdr[col+1])
		}
		other := 0.0
		for i, r := range rows {
			if i < top {
				bl.Segs = append(bl.Segs, seg{r.name, r.v[col]})
			} else {
				other += r.v[col]
			}
		}
		bl.Segs = append(bl.Segs, seg{"other", other})
		sum := 0.0
		for _, s := range bl.Segs {
			sum += s.V
		}
		bl.Text = fmtGB(sum)
		if hdr[col+1] == "COLD" && c.PricePerTB > 0 {
			bl.Text += fmt.Sprintf(" · $%.2f/mo", sum/1000*c.PricePerTB)
		}
		out = append(out, bl)
	}
	note := "snapshot " + st.ModTime().Format("02 Jan 15:04")
	if len(estimated) > 0 {
		note += " · on this box, " + strings.Join(estimated, ", ") + " is the NAS figure (too big to walk nightly)"
	}
	return out, "GB", note, nil
}

func fmtGB(gb float64) string { return fmtBytes(gb * 1e9) }

// byteUnit maps a decimal size unit to bytes.
var byteUnit = map[string]float64{"B": 1, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "PB": 1e15}

// fmtBytes writes a size the way a person would say it: the largest unit that keeps the number
// at 1 or more, three significant figures — "857 GB", "1.99 TB", "38.2 GB", never "0.04 TB".
// Used where each number stands alone (a capacity row, a total). A chart of many values keeps
// one unit for all of them instead (charting-rules.md, rule 5).
func fmtBytes(b float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	i := 0
	for math.Abs(b) >= 1000 && i < len(units)-1 {
		b /= 1000
		i++
	}
	return sig3(b) + " " + units[i]
}

func sig3(v float64) string {
	switch a := math.Abs(v); {
	case a >= 100:
		return fmt.Sprintf("%.0f", v)
	case a >= 10:
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

// blockLines draws block rows as text, the report kit's way: █ for used or a segment, ░ for
// the free part of a capacity row. Segmented rows share one scale; capacity rows are each
// their own whole.
func blockLines(bs []block, w int) []string {
	lw, max := 0, 0.0
	for _, b := range bs {
		lw = maxInt(lw, len([]rune(b.Label)))
		sum := 0.0
		for _, s := range b.Segs {
			sum += s.V
		}
		max = math.Max(max, math.Max(sum, b.Total))
	}
	glyph := []rune("█▓▒░")
	var out []string
	for _, b := range bs {
		var sb strings.Builder
		scale := float64(w) / max
		if b.Total > 0 {
			scale = float64(w) / b.Total
		}
		n := 0
		for k, s := range b.Segs {
			c := int(s.V*scale + 0.5)
			g := glyph[0]
			if b.Total == 0 {
				g = glyph[k%3]
			}
			sb.WriteString(strings.Repeat(string(g), c))
			n += c
		}
		if b.Total > 0 && n < w {
			sb.WriteString(strings.Repeat("·", w-n))
		}
		out = append(out, fmt.Sprintf("%-*s %s %s", lw, b.Label, sb.String(), b.Text))
		if b.Total == 0 {
			var parts []string
			for _, s := range b.Segs {
				if s.V > 0 {
					parts = append(parts, fmt.Sprintf("%s %s", s.Name, fmtGB(s.V)))
				}
			}
			out = append(out, fmt.Sprintf("%-*s   %s", lw, "", strings.Join(parts, " · ")))
		}
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func firstT(ss []series) float64 {
	f := math.Inf(1)
	for _, s := range ss {
		if len(s.t) > 0 {
			f = math.Min(f, s.t[0])
		}
	}
	return f
}

// prepend puts the database's older points in front of the live ones, series by name.
func prepend(old, live []series, before float64) []series {
	byName := map[string]series{}
	for _, o := range old {
		byName[o.name] = o
	}
	for i, l := range live {
		o, ok := byName[l.name]
		if !ok {
			continue
		}
		var t, v []float64
		for k := range o.t {
			if o.t[k] < before {
				t, v = append(t, o.t[k]), append(v, o.v[k])
			}
		}
		live[i].t, live[i].v = append(t, l.t...), append(v, l.v...)
	}
	return live
}
