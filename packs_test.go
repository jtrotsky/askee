package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Every pack parses, its page ids are unique across all packs, and each page's kind is one
// askee draws.
func TestPacksLoad(t *testing.T) {
	ids := map[string]string{}
	for _, n := range packNames() {
		p, err := loadPack(n)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Pages) == 0 || len(p.Groups) == 0 {
			t.Errorf("pack %s: no pages or no groups", n)
		}
		for _, pg := range p.Pages {
			if other, ok := ids[pg.ID]; ok {
				t.Errorf("page %q is in packs %s and %s", pg.ID, other, n)
			}
			ids[pg.ID] = n
			switch pg.Kind {
			case "line", "hour", "instant":
			default:
				t.Errorf("pack %s page %s: kind %q needs a local file, not fit for a pack", n, pg.ID, pg.Kind)
			}
		}
	}
}

// Prometheus needs a window on every rate() and increase(); a bare one only works on
// VictoriaMetrics. Packs must run on both.
func TestPacksPrometheusSafe(t *testing.T) {
	bareRate := regexp.MustCompile(`\b(rate|increase)\(([^()]|\([^()]*\))*\)`)
	for _, n := range packNames() {
		p, _ := loadPack(n)
		for _, pg := range p.Pages {
			exprs := []string{pg.Expr, pg.Requires}
			for _, s := range pg.Series {
				exprs = append(exprs, s.Expr)
			}
			for _, r := range pg.Refs {
				exprs = append(exprs, r.Expr)
			}
			for _, e := range exprs {
				for _, m := range bareRate.FindAllString(e, -1) {
					if !strings.Contains(m, "$__rate") && !strings.Contains(m, "[") {
						t.Errorf("pack %s page %s: %s has no window", n, pg.ID, m)
					}
				}
			}
		}
	}
}

func TestWithNode(t *testing.T) {
	for _, c := range []struct{ in, filter, want string }{
		{`node_load1{$node}`, ``, `node_load1`},
		{`x{mode="idle",$node}`, ``, `x{mode="idle"}`},
		{`x{$node,mode="idle"}`, ``, `x{mode="idle"}`},
		{`x{a="1",$node,b="2"}`, ``, `x{a="1",b="2"}`},
		{`x{$node}`, `instance="web1:9100"`, `x{instance="web1:9100"}`},
		{`x{mode="idle",$node}`, `job="node"`, `x{mode="idle",job="node"}`},
	} {
		if got := withNode(c.in, c.filter); got != c.want {
			t.Errorf("withNode(%q, %q) = %q, want %q", c.in, c.filter, got, c.want)
		}
	}
}

// The window covers four scrapes at least, and a step plus a scrape on long ranges.
func TestRateWindow(t *testing.T) {
	defer func(d time.Duration) { scrapeEvery = d }(scrapeEvery)
	scrapeEvery = 15 * time.Second
	if got := withWindow("rate(x$__rate)", 30*time.Second); got != "rate(x[60s])" {
		t.Errorf("1h step: %s", got)
	}
	if got := withWindow("rate(x$__rate)", 6*time.Hour); got != "rate(x[21615s])" {
		t.Errorf("90d step: %s", got)
	}
	if got := bare("rate(x{a=\"b\"}$__rate)"); got != `rate(x{a="b"})` {
		t.Errorf("live: %s", got)
	}
}

// A config page with a pack page's id replaces it in place; hide removes; the config's group
// order and titles win; a config with nothing gets every pack; no $node survives.
func TestPackMerge(t *testing.T) {
	c := &Config{
		Packs:      []string{"system", "memory"},
		Hide:       []string{"arc_size"},
		NodeFilter: `instance="a"`,
		Groups:     []Group{{"memory", "RAM"}, {"mine", "Mine"}},
		Pages: []PageCfg{
			{ID: "temp", Group: "system", Kind: "line", Title: "Mine", Series: []SeriesCfg{{Expr: "t", Name: "t"}}},
			{ID: "own", Group: "mine", Kind: "line", Series: []SeriesCfg{{Expr: "o", Name: "o"}}},
		},
	}
	if _, err := finishConfig(c); err != nil {
		t.Fatal(err)
	}
	var gs []string
	for _, g := range c.Groups {
		gs = append(gs, g.ID+"="+g.Title)
	}
	if strings.Join(gs, ",") != "system=System,memory=RAM,mine=Mine" {
		t.Errorf("groups: %v", gs)
	}
	var ids []string
	for _, p := range c.Pages {
		ids = append(ids, p.ID)
		b, _ := json.Marshal(p)
		if strings.Contains(string(b), "$node") {
			t.Errorf("page %s still has $node", p.ID)
		}
		if p.ID == "temp" && p.Title != "Mine" {
			t.Errorf("temp not replaced by the config's page")
		}
		if p.ID == "load" && !strings.Contains(p.Series[0].Expr, `instance="a"`) {
			t.Errorf("node filter not applied: %s", p.Series[0].Expr)
		}
	}
	got := strings.Join(ids, ",")
	if !strings.HasPrefix(got, "cpu,temp,load,") || strings.Contains(got, "arc_size") || !strings.HasSuffix(got, ",own") {
		t.Errorf("pages: %s", got)
	}
	empty := &Config{}
	if _, err := finishConfig(empty); err != nil || len(empty.Groups) != len(packNames()) {
		t.Errorf("empty config: %v, %d groups", err, len(empty.Groups))
	}
}

// Pages whose "requires" finds nothing are dropped; an unreachable database keeps them.
func TestPrune(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res := []map[string]any{}
		if !strings.Contains(r.URL.Query().Get("query"), "MemAvailable") {
			res = append(res, map[string]any{"metric": map[string]string{}, "value": []any{1, "1"}})
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"result": res}})
	}))
	defer srv.Close()
	c := &Config{Packs: []string{"memory"}}
	finishConfig(c)
	vmBase = srv.URL
	c.prune(0)
	for _, p := range c.Pages {
		if p.ID == "mem_linux" {
			t.Error("mem_linux kept on a box without MemAvailable")
		}
	}
	if len(c.Pages) != 5 {
		t.Errorf("kept %d pages, want 5", len(c.Pages))
	}
	d := &Config{Packs: []string{"memory"}}
	finishConfig(d)
	vmBase = "http://127.0.0.1:1"
	d.prune(0)
	if len(d.Pages) != 6 {
		t.Errorf("database down: kept %d pages, want all 6", len(d.Pages))
	}
}
