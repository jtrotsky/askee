package main

// Tests run against a fake Prometheus API, so they need no database and run anywhere:
//
//	go test ./...                 every page in every config renders, without error
//	go test -bench . -run XXX     how long one group takes to build (the server's hot path)
//
// test/perf.mjs is the other half: a real browser, timing fetch, draw and the live tick.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeVM answers every query with two labelled series of a slow sine wave, sampled at the
// requested step, and every instant query with a vector of three values.
func fakeVM(t testing.TB) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var res []map[string]any
		if strings.HasSuffix(r.URL.Path, "query_range") {
			var start, end, step float64
			fmt.Sscan(q.Get("start"), &start)
			fmt.Sscan(q.Get("end"), &end)
			fmt.Sscan(strings.TrimSuffix(q.Get("step"), "s"), &step)
			if step <= 0 {
				step = 60
			}
			for k, name := range []string{"a", "b"} {
				var vals [][2]any
				for ts := start; ts <= end; ts += step {
					v := 50 + 40*math.Sin(ts/3600+float64(k))
					if strings.Contains(q.Get("query"), "boot_time") {
						v = end - 3600 // one reboot an hour ago
					}
					vals = append(vals, [2]any{ts, fmt.Sprint(v)})
				}
				res = append(res, map[string]any{"metric": map[string]string{"device": name, "pool": name, "group": name, "instance": name}, "values": vals})
			}
		} else {
			for k, name := range []string{"a", "b", "c"} {
				res = append(res, map[string]any{"metric": map[string]string{"group": name, "dataset": name}, "value": [2]any{time.Now().Unix(), fmt.Sprint(10 * (k + 1))}})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": res}})
	}))
	t.Cleanup(srv.Close)
	vmBase = srv.URL
}

func exampleConfig(t testing.TB) *Config {
	cfg, err := loadConfig("examples/node-exporter.json")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Every page of the example renders at every range, and line pages get data.
func TestExampleRendersEveryRange(t *testing.T) {
	fakeVM(t)
	cfg := exampleConfig(t)
	for _, g := range cfg.Groups {
		ps := groupPages(cfg.pages(), g.ID)
		for _, r := range ranges {
			for i, m := range fetchAll(ps, r) {
				if m.err != nil {
					t.Errorf("%s/%s at %s: %v", g.ID, ps[i].ID, r.name, m.err)
				}
				if m.kind == "line" && len(m.series) == 0 {
					t.Errorf("%s/%s at %s: no series", g.ID, ps[i].ID, r.name)
				}
			}
		}
	}
}

// The live range applies only to pages marked live; everything else shows the last hour.
func TestLiveOnlyForLivePages(t *testing.T) {
	fakeVM(t)
	live, _ := rangeByName("live")
	m := page{Kind: "line", Live: true, Series: []SeriesCfg{{Expr: "x", Name: "x"}}, cfg: exampleConfig(t)}.fetch(live)
	if m.rng != "live" {
		t.Errorf("live page drew %q, want live", m.rng)
	}
	m = page{Kind: "line", Series: []SeriesCfg{{Expr: "x", Name: "x"}}, cfg: exampleConfig(t)}.fetch(live)
	if m.rng != "1h" {
		t.Errorf("non-live page in the live view drew %q, want 1h", m.rng)
	}
}

// A page with min_range follows the selected range but never draws less than its minimum,
// and every header names what is actually drawn, not what was selected.
func TestDrawnRange(t *testing.T) {
	r := func(n string) rng { x, _ := rangeByName(n); return x }
	daily := page{PageCfg: PageCfg{Kind: "line", MinRange: "7d"}}
	for sel, want := range map[string]string{"live": "7d", "1h": "7d", "24h": "7d", "7d": "7d", "30d": "30d", "90d": "90d"} {
		if got := daily.tag(r(sel)); got != want {
			t.Errorf("min_range 7d at %s drew %s, want %s", sel, got, want)
		}
	}
	for _, c := range []struct {
		p    PageCfg
		sel  string
		want string
	}{
		{PageCfg{Kind: "line", Range: "90d"}, "1h", "90d"},
		{PageCfg{Kind: "line"}, "6h", "6h"},
		{PageCfg{Kind: "line"}, "live", "1h"},
		{PageCfg{Kind: "line", Live: true}, "live", "live"},
		{PageCfg{Kind: "hour"}, "1h", "30d"},
		{PageCfg{Kind: "hour", Range: "90d"}, "live", "90d"},
		{PageCfg{Kind: "instant", Expr: "increase(x[$__range])"}, "30d", "30d"},
		{PageCfg{Kind: "instant"}, "1h", "now"},
		{PageCfg{Kind: "instant", Range: "now"}, "1h", "now"},
		{PageCfg{Kind: "snapshot"}, "1h", "snapshot"},
	} {
		if got := (page{PageCfg: c.p}).tag(r(c.sel)); got != c.want {
			t.Errorf("%+v at %s: tag %s, want %s", c.p, c.sel, got, c.want)
		}
	}
}

// Configured events land on the chart once each, even though a boot time jitters.
func TestEventsDeduplicated(t *testing.T) {
	fakeVM(t)
	cfg := exampleConfig(t)
	end := time.Now()
	ev := cfg.eventsIn(end.Add(-6*time.Hour), end)
	if len(ev) != 1 {
		t.Errorf("got %d events, want 1 reboot: %+v", len(ev), ev)
	}
}

// The zpool-list shape parses into capacity rows.
func TestCapacityLine(t *testing.T) {
	x := capLine.FindStringSubmatch("tank (0.86 of 1.99 TB, frag 23%) [████──]  43.0%")
	if x == nil || x[1] != "tank" || x[2] != "0.86" || x[3] != "1.99" || x[4] != "TB" {
		t.Fatalf("capLine: %q", x)
	}
}

func TestBlockLines(t *testing.T) {
	ls := blockLines([]block{{Label: "pool", Segs: []seg{{"used", 1}}, Total: 4, Text: "1 of 4"}}, 8)
	if len(ls) != 1 || !strings.Contains(ls[0], "██······") {
		t.Errorf("capacity row: %q", ls)
	}
}

// A real catalogue, when ASKEE_CONFIG names one, must load and every page id must be unique.
func TestOwnConfigLoads(t *testing.T) {
	p := os.Getenv("ASKEE_CONFIG")
	if p == "" {
		t.Skip("set ASKEE_CONFIG to check a real catalogue")
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, pg := range cfg.pages() {
		if ids[pg.ID] {
			t.Errorf("duplicate page id %q", pg.ID)
		}
		ids[pg.ID] = true
	}
}

// One group built from scratch: the work a cache miss costs (fake VM, so this is askee's own
// share — decoding, shaping, JSON, gzip).
func BenchmarkGroup(b *testing.B) {
	fakeVM(b)
	cfg := exampleConfig(b)
	gc := &groupCache{cfg: cfg, all: cfg.pages(), entries: map[string]*cacheEntry{}, busy: map[string]chan struct{}{}, tiles: map[string]tileCache{}}
	r, _ := rangeByName("24h")
	for i := 0; i < b.N; i++ {
		gc.refresh("system", r)
	}
}

// The cache hands back a stale answer at once rather than waiting on the database.
func TestStaleServedImmediately(t *testing.T) {
	fakeVM(t)
	cfg := exampleConfig(t)
	gc := &groupCache{cfg: cfg, all: cfg.pages(), entries: map[string]*cacheEntry{}, busy: map[string]chan struct{}{}, tiles: map[string]tileCache{}}
	r, _ := rangeByName("24h")
	first := gc.get("system", r)
	first.at = time.Now().Add(-time.Hour) // make it stale
	vmBase = "http://127.0.0.1:1"         // and the database unreachable
	t0 := time.Now()
	got := gc.get("system", r)
	if d := time.Since(t0); d > 50*time.Millisecond {
		t.Errorf("stale hit took %v; it must not wait for the refresh", d)
	}
	if got != first {
		t.Errorf("did not serve the stale entry")
	}
}

// The live evaluator: exposition lines in, the subset of PromQL the live charts use out.
func TestLiveEvaluator(t *testing.T) {
	name, labels, v, ok := parseSample(`node_cpu_seconds_total{cpu="0",mode="system"} 12.5`)
	if !ok || name != "node_cpu_seconds_total" || labels["mode"] != "system" || v != 12.5 {
		t.Fatalf("parseSample: %v %v %v %v", name, labels, v, ok)
	}
	t0 := time.Now()
	mk := func(sec int, sys0, sys1, load float64) snapshot {
		return snapshot{t: t0.Add(time.Duration(sec) * time.Second), v: map[string]float64{
			seriesKey("node_cpu_seconds_total", map[string]string{"cpu": "0", "mode": "system"}): sys0,
			seriesKey("node_cpu_seconds_total", map[string]string{"cpu": "1", "mode": "system"}): sys1,
			seriesKey("node_cpu_seconds_total", map[string]string{"cpu": "0", "mode": "idle"}):   99,
			seriesKey("node_load1", nil): load,
		}}
	}
	snaps := []snapshot{mk(0, 10, 20, 1), mk(1, 10.5, 20.1, 2)}
	for expr, want := range map[string]float64{
		`100 * avg(rate(node_cpu_seconds_total{mode="system"}))`: 30, // (0.5 + 0.1) / 2 × 100
		`sum(rate(node_cpu_seconds_total{mode=~"sys.*"}))`:       0.6,
		`node_load1 * 2`: 4,
		`max(node_cpu_seconds_total{mode!="idle"}) / 1e1`: 2.01,
	} {
		n, err := parseLive(expr)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		var got float64
		for _, x := range n.eval(snaps, 1) {
			got = x
		}
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("%s = %v, want %v", expr, got, want)
		}
	}
	for _, bad := range []string{`rate(x[5m])`, `sum without (a) (x)`, `label_replace(x, "a", "b", "c", "d")`} {
		if _, err := parseLive(bad); err == nil {
			t.Errorf("%s: should fall back to the database", bad)
		}
	}
}

// Live rate() looks back a few seconds, so a counter that moves in bursts (ZFS writes every
// 5 s, busy time in whole seconds) reads as its average, not as nothing and then a spike. A
// counter reset inside the window falls back to the last step.
func TestLiveRateWindow(t *testing.T) {
	t0 := time.Now()
	key := seriesKey("c", nil)
	var snaps []snapshot
	for s, v := range []float64{0, 0, 0, 0, 0, 5, 5, 5, 5, 5, 10, 2, 3} { // +5 every 5 s, then a reset
		snaps = append(snaps, snapshot{t: t0.Add(time.Duration(s) * time.Second), v: map[string]float64{key: v}})
	}
	at := func(win time.Duration, i int) (float64, bool) {
		n, err := parseLiveWindow("rate(c)", win)
		if err != nil {
			t.Fatal(err)
		}
		v, ok := n.eval(snaps, i)[key]
		return v, ok
	}
	for _, c := range []struct {
		win  time.Duration
		i    int
		want float64
	}{
		{time.Second, 4, 0}, {time.Second, 5, 5}, // two reads: nothing, then the whole burst
		{5 * time.Second, 9, 1}, {5 * time.Second, 10, 1}, // five seconds: the average
		{5 * time.Second, 3, 0},  // the window is cut short at the first read
		{5 * time.Second, 12, 1}, // reset in the window: the last step alone (2 → 3)
	} {
		if got, ok := at(c.win, c.i); !ok || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("window %v at %d s = %v (%v), want %v", c.win, c.i, got, ok, c.want)
		}
	}
	if _, ok := at(5*time.Second, 11); ok { // the reset step itself has no rate
		t.Errorf("a reset step should have no rate")
	}
}

// When the database stops answering, a chart keeps its last good data and says how old it
// is — it never turns into a transport error.
func TestDatabaseOutageKeepsLastGood(t *testing.T) {
	fakeVM(t)
	cfg := exampleConfig(t)
	gc := &groupCache{cfg: cfg, all: cfg.pages(), entries: map[string]*cacheEntry{}, busy: map[string]chan struct{}{}, tiles: map[string]tileCache{}}
	r, _ := rangeByName("24h")
	gc.refresh("system", r)
	vmBase = "http://127.0.0.1:1" // the database goes away
	e := gc.refresh("system", r)
	var d struct {
		Tiles []tileJSON `json:"tiles"`
	}
	if err := json.Unmarshal(e.body, &d); err != nil {
		t.Fatal(err)
	}
	for _, tl := range d.Tiles {
		if tl.Err != "" {
			t.Errorf("%s shows an error instead of its last good data: %s", tl.ID, tl.Err)
		}
		if tl.Kind == "line" && !strings.Contains(tl.Note, "as of") {
			t.Errorf("%s: stale data must say how old it is (note %q)", tl.ID, tl.Note)
		}
	}
}

// A series with no data reaches the browser as [], not null (null broke the whole group).
func TestEmptySeriesIsArray(t *testing.T) {
	tl := toTile(page{PageCfg: PageCfg{ID: "x", Kind: "line"}}, model{kind: "line", series: []series{{name: "waiting"}}})
	b, _ := json.Marshal(tl)
	if !strings.Contains(string(b), `"t":[]`) || strings.Contains(string(b), "null") {
		t.Errorf("empty series: %s", b)
	}
}

// Editing the catalogue reloads it: a new group appears and the generation bumps. A file that
// does not load leaves the running catalogue in place.
func TestConfigReload(t *testing.T) {
	fakeVM(t)
	dir := t.TempDir()
	path := dir + "/askee.json"
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	one := `{"vm":"` + vmBase + `","groups":[{"id":"a","title":"A"}],"pages":[{"id":"p1","group":"a","kind":"line","series":[{"expr":"x","name":"x"}]}]}`
	write(one)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	var cur atomic.Pointer[served]
	cur.Store(newServed(cfg, 1))
	h := handler(&cur)
	meta := func() (gen float64, groups int) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/meta", nil))
		var m struct {
			Gen    float64 `json:"gen"`
			Groups []any   `json:"groups"`
		}
		json.Unmarshal(rec.Body.Bytes(), &m)
		return m.Gen, len(m.Groups)
	}
	if g, n := meta(); g != 1 || n != 1 {
		t.Fatalf("start: gen %v groups %d", g, n)
	}
	two := `{"vm":"` + vmBase + `","groups":[{"id":"a","title":"A"},{"id":"b","title":"B"}],"pages":[` +
		`{"id":"p1","group":"a","kind":"line","series":[{"expr":"x","name":"x"}]},` +
		`{"id":"p2","group":"b","kind":"line","series":[{"expr":"y","name":"y"}]}]}`
	write(two)
	if err := reloadConfig(path, "", &cur); err != nil {
		t.Fatal(err)
	}
	if g, n := meta(); g != 2 || n != 2 {
		t.Fatalf("after reload: gen %v groups %d, want 2 and 2", g, n)
	}
	write(`{"groups": [`) // a half-saved edit
	if err := reloadConfig(path, "", &cur); err == nil {
		t.Fatal("a broken file must not load")
	}
	if g, n := meta(); g != 2 || n != 2 {
		t.Fatalf("broken edit replaced the catalogue: gen %v groups %d", g, n)
	}
}

func TestCopyRules(t *testing.T) {
	ok := []PageCfg{
		{ID: "a", Title: "Free space on /", Info: "One sentence, with a clause, at 105 °C and 2–12 h."},
		{ID: "b", Subtitle: "Old key, still read.", Note: "last 30 days"},
	}
	bad := []PageCfg{
		{ID: "c", Info: "First sentence. Second sentence."},
		{ID: "d", Info: "x", Subtitle: "y"},
		{ID: "e", Note: "bedroom and study read at the heat pump, high on the wall"},
		{ID: "f", Note: "Short. Two."},
		{ID: "g", Info: "One clause; then another."},
		{ID: "h", Info: "One clause — then another."},
		{ID: "i", Title: "Growth, 30 days"},
	}
	for _, p := range ok {
		p.Group, p.Kind = "g", "line"
		c, err := finishConfig(&Config{Packs: []string{}, Pages: []PageCfg{p}})
		if err != nil {
			t.Errorf("%s: %v", p.ID, err)
		} else if c.Pages[0].Info == "" || c.Pages[0].Subtitle != "" {
			t.Errorf("%s: info %q subtitle %q after load", p.ID, c.Pages[0].Info, c.Pages[0].Subtitle)
		}
	}
	for _, p := range bad {
		p.Group, p.Kind = "g", "line"
		if _, err := finishConfig(&Config{Packs: []string{}, Pages: []PageCfg{p}}); err == nil {
			t.Errorf("%s: loaded, want a copy-rule error", p.ID)
		}
	}
}

// Each tab's floor is the longest of its charts' shortest ranges; it is live only when every
// chart that covers a stretch of time can be; readings do not count either way.
func TestGroupMode(t *testing.T) {
	cfg := &Config{LiveSource: "http://x/metrics"}
	pg := func(c PageCfg) page { return page{PageCfg: c, cfg: cfg} }
	liveLine := pg(PageCfg{Kind: "line", Live: true})
	dbLine := pg(PageCfg{Kind: "line"})
	daily := pg(PageCfg{Kind: "line", MinRange: "7d"})
	reading := pg(PageCfg{Kind: "snapshot"})
	for _, c := range []struct {
		name string
		ps   []page
		want groupMode
	}{
		{"all live", []page{liveLine, liveLine, reading}, groupMode{Min: "live", Live: true}},
		{"mixed", []page{liveLine, dbLine}, groupMode{Min: "1h", Mixed: true}},
		{"delayed", []page{dbLine, daily, reading}, groupMode{Min: "7d"}},
		{"readings", []page{reading}, groupMode{}},
	} {
		if got := modeOf(c.ps); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
	r, _ := rangeByName("1h")
	if got := (groupMode{Min: "7d"}).clamp(r).name; got != "7d" {
		t.Errorf("clamp 1h to a 7d floor: %s", got)
	}
}

func TestCheckTime(t *testing.T) {
	bad := []PageCfg{
		{ID: "a", Kind: "line", Range: "30d"},
		{ID: "b", Kind: "snapshot", MinRange: "7d"},
		{ID: "c", Kind: "hour", Live: true},
		{ID: "d", Kind: "line", Live: true, Series: []SeriesCfg{{Expr: `label_replace(x, "a", "b", "c", "d")`}}},
		{ID: "f", Kind: "line", LiveWin: "30s"},             // not live
		{ID: "g", Kind: "line", Live: true, LiveWin: "10m"}, // past the five minutes kept
		{ID: "h", Kind: "line", Live: true, LiveWin: "30"},  // no unit
	}
	for _, p := range bad {
		if checkTime(page{PageCfg: p}) == nil {
			t.Errorf("%s: passed, want an error", p.ID)
		}
	}
	ok := PageCfg{ID: "e", Kind: "line", Live: true, LiveWin: "30s", Series: []SeriesCfg{{Expr: "(sum(a) or vector(0)) / 2^30"}}}
	if err := checkTime(page{PageCfg: ok}); err != nil {
		t.Errorf("e: %v", err)
	}
}

// or, vector() and ^ in the live evaluator, and series matched on labels, not names.
func TestLiveOrPowVector(t *testing.T) {
	snap := func(v map[string]float64) snapshot { return snapshot{t: time.Now(), v: v} }
	s := []snapshot{snap(map[string]float64{
		seriesKey("a", map[string]string{"d": "x"}): 6,
		seriesKey("b", map[string]string{"d": "x"}): 2,
	})}
	for expr, want := range map[string]float64{
		"sum(missing) or vector(7)": 7,
		"sum(a) or vector(7)":       6,
		"sum(a) / 2^1":              3,
		"2^3^2":                     512,
		"sum(a - b)":                4,
	} {
		n, err := parseLive(expr)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if got := n.eval(s, 0)[""]; got != want {
			t.Errorf("%s = %v, want %v", expr, got, want)
		}
	}
}

// sum by (...) and histogram_quantile in the live evaluator, checked against values worked by
// hand the way Prometheus computes them.
func TestLiveByAndQuantile(t *testing.T) {
	k := func(name string, l map[string]string) string { return seriesKey(name, l) }
	s := []snapshot{{time.Now(), map[string]float64{
		k("resp", map[string]string{"type": "A", "c": "1"}): 1,
		k("resp", map[string]string{"type": "A", "c": "2"}): 2,
		k("resp", map[string]string{"type": "B", "c": "1"}): 4,
		k("h_bucket", map[string]string{"le": "0.1"}):       50,
		k("h_bucket", map[string]string{"le": "0.2"}):       90,
		k("h_bucket", map[string]string{"le": "+Inf"}):      100,
	}}}
	for expr, want := range map[string]map[string]float64{
		"sum by (type) (resp)":                            {k("", map[string]string{"type": "A"}): 3, k("", map[string]string{"type": "B"}): 4},
		"sum(resp) by (type)":                             {k("", map[string]string{"type": "A"}): 3, k("", map[string]string{"type": "B"}): 4},
		"histogram_quantile(0.5, sum(h_bucket) by (le))":  {"": 0.1},
		"histogram_quantile(0.7, sum(h_bucket) by (le))":  {"": 0.15},
		"histogram_quantile(0.99, sum(h_bucket) by (le))": {"": 0.2},
	} {
		n, err := parseLive(expr)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		got := n.eval(s, 0)
		for key, w := range want {
			if math.Abs(got[key]-w) > 1e-9 {
				t.Errorf("%s: %q = %v, want %v (all: %v)", expr, key, got[key], w, got)
			}
		}
	}
}
