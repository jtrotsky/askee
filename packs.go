package main

// Packs are the standard charts, shipped inside the binary: a config names them in "packs"
// and gets their groups and pages without writing a query. Each pack is written for
// node_exporter on Linux AND FreeBSD at once, and assumes nothing else about the machine:
//
//   - Where the two systems name a metric differently, the query asks for both with `or`.
//   - A page that only makes sense on some machines (ZFS, a CPU sensor, swap, Linux memory
//     accounting) carries "requires": a query that must return something for the page to be
//     shown. A box without ZFS simply has no ZFS charts; nothing is drawn empty.
//   - $node stands for the config's "node_filter" (e.g. instance="web1:9100"), so one database
//     holding many machines can be charted one machine at a time. Empty means every series.
//   - $__rate is the window for rate(): long enough for any scrape interval
//     ("scrape_interval", default 60s), so the same query works on Prometheus, which needs an
//     explicit window, and VictoriaMetrics. The live range drops it (the live evaluator takes
//     rate() bare).
//
// A page in the config with the same id as a pack page replaces it; "hide" removes pages by
// id. Groups the config lists keep the config's order and title; the rest come first, in pack
// order.

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed packs/*.json
var packFS embed.FS

type pack struct {
	Groups []Group    `json:"groups"`
	Events []EventCfg `json:"events"`
	Pages  []PageCfg  `json:"pages"`
}

// packNames lists the embedded packs, sorted.
func packNames() []string {
	es, _ := packFS.ReadDir("packs")
	var out []string
	for _, e := range es {
		out = append(out, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(out)
	return out
}

// defaultPacks is what a config with no pages and no "packs" gets: everything. Pages whose
// metrics the database does not have are pruned, so this is safe on any node_exporter box.
func defaultPacks() []string { return packNames() }

func loadPack(name string) (pack, error) {
	var p pack
	b, err := packFS.ReadFile("packs/" + name + ".json")
	if err != nil {
		return p, fmt.Errorf("unknown pack %q (have: %s)", name, strings.Join(packNames(), ", "))
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("pack %s: %w", name, err)
	}
	return p, nil
}

// expandPacks merges the named packs into the config, then applies node_filter everywhere.
func (c *Config) expandPacks() error {
	names := c.Packs
	if names == nil && len(c.Pages) == 0 {
		names = defaultPacks()
	}
	var groups []Group
	var pages []PageCfg
	var events []EventCfg
	for _, n := range names {
		p, err := loadPack(n)
		if err != nil {
			return err
		}
		groups = append(groups, p.Groups...)
		pages = append(pages, p.Pages...)
		events = append(events, p.Events...)
	}
	// groups: pack groups the config does not list, then the config's own, in its order
	listed := map[string]bool{}
	for _, g := range c.Groups {
		listed[g.ID] = true
	}
	var gs []Group
	for _, g := range groups {
		if !listed[g.ID] {
			gs = append(gs, g)
			listed[g.ID] = true
		}
	}
	c.Groups = append(gs, c.Groups...)
	// pages: a config page replaces the pack page of the same id, in the pack page's place
	own := map[string]int{}
	for i, p := range c.Pages {
		own[p.ID] = i
	}
	hide := map[string]bool{}
	for _, id := range c.Hide {
		hide[id] = true
	}
	var ps []PageCfg
	used := map[int]bool{}
	for _, p := range pages {
		if i, ok := own[p.ID]; ok {
			p, used[i] = c.Pages[i], true
		}
		ps = append(ps, p)
	}
	for i, p := range c.Pages {
		if !used[i] {
			ps = append(ps, p)
		}
	}
	c.Pages = c.Pages[:0:0]
	for _, p := range ps {
		if !hide[p.ID] {
			c.Pages = append(c.Pages, p)
		}
	}
	// events: the config's, plus the packs' that are not already there
	have := map[string]bool{}
	for _, e := range c.Events {
		have[e.Label] = true
	}
	for _, e := range events {
		if !have[e.Label] {
			c.Events = append(c.Events, e)
			have[e.Label] = true
		}
	}
	// $node, everywhere a query can be
	nf := strings.TrimSpace(c.NodeFilter)
	for i := range c.Pages {
		p := &c.Pages[i]
		p.Expr, p.Key, p.Capacity, p.Requires = withNode(p.Expr, nf), withNode(p.Key, nf), withNode(p.Capacity, nf), withNode(p.Requires, nf)
		for k := range p.Series {
			p.Series[k].Expr = withNode(p.Series[k].Expr, nf)
		}
		for k := range p.Refs {
			p.Refs[k].Expr = withNode(p.Refs[k].Expr, nf)
		}
	}
	for i := range c.Events {
		c.Events[i].Expr = withNode(c.Events[i].Expr, nf)
	}
	return nil
}

// withNode puts the node filter where $node stands. Written as a matcher inside braces —
// `{$node}`, `{mode="idle",$node}` — and an empty filter leaves no stray comma or braces.
func withNode(expr, filter string) string {
	if !strings.Contains(expr, "$node") {
		return expr
	}
	if filter != "" {
		return strings.ReplaceAll(expr, "$node", filter)
	}
	r := strings.NewReplacer(",$node}", "}", "{$node,", "{", "{$node}", "", ",$node,", ",")
	return r.Replace(expr)
}

// scrapeEvery is the database's scrape interval, from the config; it sets the rate() window.
var scrapeEvery = 60 * time.Second

// rateWindow follows Grafana's $__rate_interval: at least four scrapes, so a rate always has
// two points to work from, and at least one step plus a scrape, so no sample falls between
// steps.
func rateWindow(step time.Duration) time.Duration {
	return max(step+scrapeEvery, 4*scrapeEvery)
}

// withWindow puts the rate() window where $__rate stands, for a query at this step (0 for an
// instant query).
func withWindow(expr string, step time.Duration) string {
	if !strings.Contains(expr, "$__rate") {
		return expr
	}
	return strings.ReplaceAll(expr, "$__rate", fmt.Sprintf("[%ds]", int(rateWindow(step).Seconds())))
}

// bare drops $__rate for the live evaluator, which computes rate() from consecutive reads.
func bare(expr string) string { return strings.ReplaceAll(expr, "$__rate", "") }

// prune drops the pages whose "requires" query returns nothing. If the database cannot be
// asked, the page stays (a chart with an error says more than a missing one) — but a server
// starting alongside its database waits for it a while first, so the answer is the real one.
func (c *Config) prune(wait time.Duration) {
	var need []int
	for i, p := range c.Pages {
		if p.Requires != "" {
			need = append(need, i)
		}
	}
	if len(need) == 0 {
		return
	}
	keep := make([]bool, len(c.Pages))
	for i := range keep {
		keep[i] = true
	}
	deadline := time.Now().Add(wait)
	for {
		var mu sync.Mutex
		var wg sync.WaitGroup
		failed := 0
		for _, i := range need {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				res, err := instant(c.Pages[i].Requires)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					failed++
					return
				}
				keep[i] = len(res) > 0
			}(i)
		}
		wg.Wait()
		if failed == 0 || time.Now().After(deadline) {
			if failed > 0 {
				fmt.Fprintf(os.Stderr, "askee: database unreachable; showing all %d pages that need a check\n", failed)
			}
			break
		}
		time.Sleep(5 * time.Second)
	}
	var ps []PageCfg
	for i, p := range c.Pages {
		if keep[i] {
			ps = append(ps, p)
		}
	}
	c.Pages = ps
}
