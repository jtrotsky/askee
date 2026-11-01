package main

// The page catalogue is DATA, not code: a JSON file lists groups and pages, so the same binary
// serves any Prometheus-compatible TSDB (VictoriaMetrics, Prometheus). examples/node-exporter.json
// is a generic starting point.
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
	"time"
)

type Config struct {
	Title        string     `json:"title"`
	VM           string     `json:"vm"`
	Snapshot     string     `json:"snapshot"`      // markdown file of `### key` sections (root-only figures)
	Sizes        string     `json:"sizes"`         // backup-sizes.sh --remote output
	PricePerTB   float64    `json:"price_per_tb"`  // for the cost column of the sizes table
	PriceLabel   string     `json:"price_label"`   // e.g. "cold $/mo"
	Thermal      [2]float64 `json:"thermal_range"` // °C mapped to the cool→hot ramp
	DefaultRange string     `json:"default_range"`
	DefaultTheme string     `json:"default_theme"`
	Events       []EventCfg `json:"events"` // drawn on every line chart: "what happened here?"
	Groups       []Group    `json:"groups"`
	Pages        []PageCfg  `json:"pages"`
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

type event struct {
	T     float64 `json:"t"`
	Label string  `json:"label"`
}

type SeriesCfg struct {
	Expr  string `json:"expr"`
	Name  string `json:"name"`  // fixed series name
	Label string `json:"label"` // or: name each result by this label
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
	Subtitle string      `json:"subtitle"` // the "so what" — one line under the title
	Kind     string      `json:"kind"`     // line | hour | instant | runs | snapshot | sizes
	Unit     string      `json:"unit"`
	Range    string      `json:"range"` // fixed range for this page ("" = the selected one)
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
	Span     [2]int      `json:"span"` // [columns, rows] in the web grid; 0 = automatic
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
	events  []event
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
	{"15m", 15 * time.Minute, 15 * time.Second},
	{"1h", time.Hour, 15 * time.Second},
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
	for i, p := range c.Pages {
		if p.ID == "" || p.Group == "" || p.Kind == "" {
			return nil, fmt.Errorf("page %d (%q): id, group and kind are required", i, p.Title)
		}
		if p.Range != "" && p.Range != "now" && p.Range != "snapshot" {
			if _, ok := rangeByName(p.Range); !ok {
				return nil, fmt.Errorf("page %s: unknown range %q", p.ID, p.Range)
			}
		}
	}
	return &c, nil
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

// fetch builds the model for one page. r is the selected range; a page with its own Range
// ignores it (e.g. pool free space only means something over 30 days).
func (p page) fetch(r rng) model {
	if p.Range != "" && p.Range != "now" && p.Range != "snapshot" {
		r, _ = rangeByName(p.Range)
	}
	m := model{kind: "line", unit: p.Unit, yMin: p.YMin, yMax: p.YMax, thermal: p.Thermal, stack: p.Stack, split: p.Split, note: p.Note}
	end := time.Now()
	start := end.Add(-r.span)
	switch p.Kind {
	case "line":
		var qs []q
		for _, s := range p.Series {
			qs = append(qs, q{s.Expr, s.Name, s.Label})
		}
		m.series, m.err = rangeSeries(qs, start, end, r.step)
		m.start, m.end, m.step = start, end, r.step
		m.refs = p.resolveRefs()
		m.events = p.cfg.eventsIn(start, end)
	case "hour":
		m.kind = "bars"
		s, err := rangeSeries([]q{{expr: p.Expr, name: "v"}}, end.Add(-30*24*time.Hour), end, time.Hour)
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
		res, err := instant(p.Expr)
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
	case "sizes":
		m.kind = "text"
		m.lines, m.note, m.err = sizesTable(p.cfg)
	default:
		m.err = fmt.Errorf("unknown page kind %q", p.Kind)
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
