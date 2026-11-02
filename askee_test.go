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
	for _, bad := range []string{`rate(x[5m])`, `sum by (a) (x)`, `histogram_quantile(0.5, x)`} {
		if _, err := parseLive(bad); err == nil {
			t.Errorf("%s: should fall back to the database", bad)
		}
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
