package main

// Two non-interactive faces of the same catalogue:
//
//   -print <group>  plain-text panels plus a numeric summary, for scripts and LLM skills to read.
//   -serve <addr>   the web dashboard: one group per screen, tiles sized to the viewport, drawn
//                   client-side on <canvas>, eight selectable themes.

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed askee.html
var pageHTML []byte

func groupPages(all []page, g string) []page {
	var out []page
	for _, p := range all {
		if p.Group == g {
			out = append(out, p)
		}
	}
	return out
}

func fetchAll(ps []page, r rng) []model {
	ms := make([]model, len(ps))
	var wg sync.WaitGroup
	for i, p := range ps {
		wg.Add(1)
		go func(i int, p page) {
			defer wg.Done()
			ms[i] = p.fetch(r)
		}(i, p)
	}
	wg.Wait()
	return ms
}

// ── -print ─────────────────────────────────────────────────────────────────────────
func printGroup(cfg *Config, g string, r rng, w, h int) int {
	ps := groupPages(cfg.pages(), g)
	if len(ps) == 0 {
		var ids []string
		for _, x := range cfg.Groups {
			ids = append(ids, x.ID)
		}
		fmt.Fprintf(os.Stderr, "askee: no group %q (have: %s)\n", g, strings.Join(ids, " "))
		return 2
	}
	ms := fetchAll(ps, r)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	th := themeByID(cfg.DefaultTheme)
	fmt.Fprintf(out, "%s · %s · range %s · %s\n\n", cfg.Title, cfg.groupTitle(g), r.name, time.Now().Format("2006-01-02 15:04"))
	for i, p := range ps {
		m := ms[i]
		ph := h
		switch m.kind {
		case "bars":
			ph = len(m.bars) + 3
		case "text":
			ph = len(m.lines) + 3
		}
		s := newScreen(w, ph, th)
		panel(s, cfg, p, m, 0, 0, w-1, ph-1, r.name)
		for y := 0; y < s.h; y++ {
			var sb strings.Builder
			for x := 0; x < s.w; x++ {
				sb.WriteRune(s.cells[y*s.w+x].r)
			}
			fmt.Fprintln(out, strings.TrimRight(sb.String(), " "))
		}
		// numbers, so a reader never estimates from glyph heights
		for _, se := range m.series {
			if len(se.v) == 0 {
				continue
			}
			mn, mx, sum := se.v[0], se.v[0], 0.0
			for _, v := range se.v {
				mn, mx, sum = math.Min(mn, v), math.Max(mx, v), sum+v
			}
			fmt.Fprintf(out, "  %-18s latest %-8s min %-8s max %-8s mean %-8s %s (n=%d)\n", se.name,
				fmtVal(se.v[len(se.v)-1]), fmtVal(mn), fmtVal(mx), fmtVal(sum/float64(len(se.v))), m.unit, len(se.v))
		}
		for _, rf := range m.refs {
			fmt.Fprintf(out, "  reference: %s = %s %s\n", rf.Label, fmtVal(rf.V), m.unit)
		}
		if p.Subtitle != "" {
			fmt.Fprintf(out, "  so what: %s\n", p.Subtitle)
		}
		fmt.Fprintln(out)
	}
	return 0
}

// ── -serve ─────────────────────────────────────────────────────────────────────────
type tileJSON struct {
	ID       string       `json:"id"`
	Title    string       `json:"title"`
	Subtitle string       `json:"subtitle,omitempty"`
	Kind     string       `json:"kind"`
	Unit     string       `json:"unit"`
	Fixed    string       `json:"fixed,omitempty"`
	Note     string       `json:"note,omitempty"`
	Err      string       `json:"err,omitempty"`
	YMin     *float64     `json:"ymin,omitempty"`
	YMax     *float64     `json:"ymax,omitempty"`
	Start    int64        `json:"start,omitempty"`
	End      int64        `json:"end,omitempty"`
	Step     float64      `json:"step,omitempty"`
	Thermal  bool         `json:"thermal,omitempty"`
	Stack    bool         `json:"stack,omitempty"`
	Refs     []ref        `json:"refs,omitempty"`
	Series   []seriesJSON `json:"series,omitempty"`
	Bars     []bar        `json:"bars,omitempty"`
	Lines    []string     `json:"lines,omitempty"`
}
type seriesJSON struct {
	Name string    `json:"name"`
	T    []float64 `json:"t"`
	V    []float64 `json:"v"`
}

func toTile(p page, m model) tileJSON {
	t := tileJSON{ID: p.ID, Title: p.Title, Subtitle: p.Subtitle, Kind: m.kind, Unit: m.unit, Fixed: p.Range, Note: m.note,
		YMin: m.yMin, YMax: m.yMax, Thermal: m.thermal, Stack: m.stack, Refs: m.refs, Bars: m.bars, Lines: m.lines, Step: m.step.Seconds()}
	if m.err != nil {
		t.Err = m.err.Error()
	}
	if !m.start.IsZero() {
		t.Start, t.End = m.start.Unix(), m.end.Unix()
	}
	for _, s := range m.series {
		t.Series = append(t.Series, seriesJSON{s.name, s.t, s.v})
	}
	return t
}

type themeJSON struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Note    string   `json:"note"`
	Status  string   `json:"status"`
	Style   string   `json:"style"`
	Mono    bool     `json:"mono"`
	Dashed  bool     `json:"dashed"`
	Light   bool     `json:"light"`
	Thermal []string `json:"thermal"`
	Vars    []string `json:"vars"` // surface panel border grid text dim title accent
	Palette []string `json:"palette"`
	Seq     []string `json:"seq"`
}

func serve(cfg *Config, addr string) error {
	all := cfg.pages()
	type cached struct {
		at   time.Time
		body []byte
	}
	var mu sync.Mutex
	cache := map[string]cached{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:")
		w.Write(pageHTML)
	})
	mux.HandleFunc("GET /api/meta", func(w http.ResponseWriter, r *http.Request) {
		type g struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			N     int    `json:"n"`
		}
		var gs []g
		for _, x := range cfg.Groups {
			if n := len(groupPages(all, x.ID)); n > 0 {
				gs = append(gs, g{x.ID, x.Title, n})
			}
		}
		var rs []string
		for _, x := range ranges {
			rs = append(rs, x.name)
		}
		var ts []themeJSON
		for _, t := range themes {
			ts = append(ts, themeJSON{ID: t.ID, Name: t.Name, Note: t.Note, Status: t.Status, Style: t.Style, Mono: t.Mono, Dashed: t.Dashed, Light: t.Light,
				Thermal: t.thermalFor(), Vars: []string{t.Surface, t.Panel, t.Border, t.Grid, t.Text, t.Dim, t.Title, t.Accent},
				Palette: t.Palette, Seq: t.seqRamp()})
		}
		writeJSON(w, map[string]any{"title": cfg.Title, "groups": gs, "ranges": rs, "default_range": cfg.DefaultRange,
			"themes": ts, "default_theme": cfg.DefaultTheme, "dashes": dashPatterns, "thermal_range": cfg.Thermal})
	})
	mux.HandleFunc("GET /api/group/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ps := groupPages(all, id)
		rq := r.URL.Query().Get("range")
		rg, ok := rangeByName(rq)
		if len(ps) == 0 || (!ok && rq != "") {
			http.Error(w, "unknown group or range", http.StatusNotFound)
			return
		}
		key := id + "|" + rg.name
		mu.Lock()
		c, hit := cache[key]
		mu.Unlock()
		if !hit || time.Since(c.at) > 15*time.Second {
			ms := fetchAll(ps, rg)
			tiles := make([]tileJSON, len(ps))
			for i := range ps {
				tiles[i] = toTile(ps[i], ms[i])
			}
			body, _ := json.Marshal(map[string]any{"group": id, "title": cfg.groupTitle(id), "range": rg.name,
				"generated": time.Now().Unix(), "tiles": tiles})
			c = cached{time.Now(), body}
			mu.Lock()
			cache[key] = c
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(c.body)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 60 * time.Second}
	fmt.Fprintf(os.Stderr, "askee: serving on http://%s/\n", addr)
	return srv.ListenAndServe()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
