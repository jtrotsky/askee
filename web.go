package main

// Two non-interactive faces of the same catalogue:
//
//   -print <group>  plain-text panels plus a numeric summary, for scripts and LLM skills to read.
//   -serve <addr>   the web dashboard: one group per screen, tiles sized to the viewport, drawn
//                   client-side on <canvas>, eight selectable themes.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed askee.html
var pageHTML []byte

//go:embed favicon.svg
var faviconSVG []byte

const csp = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:"

// pageGz is the page compressed once at start: ~30 KB of HTML becomes ~9 KB on the wire.
var pageGz = func() []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	zw.Write(pageHTML)
	zw.Close()
	return buf.Bytes()
}()

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
	gm := modeOf(ps)
	if c := gm.clamp(r); c.name != r.name {
		fmt.Fprintf(os.Stderr, "askee: %s starts at %s; showing %s, not %s\n", g, c.name, c.name, r.name)
		r = c
	}
	ms := fetchAll(ps, r)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	th := themeByID(map[bool]string{true: "carbon", false: cfg.DefaultTheme}[cfg.DefaultTheme == "auto"])
	fmt.Fprintf(out, "%s · %s · range %s · %s\n\n", cfg.Title, cfg.groupTitle(g), r.name, time.Now().Format("2006-01-02 15:04"))
	for i, p := range ps {
		m := ms[i]
		ph := h
		switch m.kind {
		case "bars":
			ph = len(m.bars) + 3
		case "text":
			ph = len(m.lines) + 3
		case "clock":
			ph = len(m.rows) + 4
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
		if m.kind == "clock" {
			for _, l := range clockSummary(m) {
				fmt.Fprintln(out, l)
			}
		}
		for _, rf := range m.refs {
			u := m.unit
			if m.kind == "blocks" {
				u = "%" // a capacity reference is a share of each row's whole
			}
			fmt.Fprintf(out, "  reference: %s = %s %s\n", rf.Label, fmtVal(rf.V), u)
		}
		if p.Info != "" {
			fmt.Fprintf(out, "  so what: %s\n", p.Info)
		}
		fmt.Fprintln(out)
	}
	return 0
}

// ── -serve ─────────────────────────────────────────────────────────────────────────
type tileJSON struct {
	ID      string       `json:"id"`
	Title   string       `json:"title"`
	Info    string       `json:"info,omitempty"`
	Kind    string       `json:"kind"`
	Unit    string       `json:"unit"`
	Note    string       `json:"note,omitempty"`
	Err     string       `json:"err,omitempty"`
	YMin    *float64     `json:"ymin,omitempty"`
	YMax    *float64     `json:"ymax,omitempty"`
	Start   int64        `json:"start,omitempty"`
	End     int64        `json:"end,omitempty"`
	Step    float64      `json:"step,omitempty"`
	Thermal bool         `json:"thermal,omitempty"`
	Stack   bool         `json:"stack,omitempty"`
	Refs    []ref        `json:"refs,omitempty"`
	Series  []seriesJSON `json:"series,omitempty"`
	Bars    []bar        `json:"bars,omitempty"`
	Lines   []string     `json:"lines,omitempty"`
	Span    [2]int       `json:"span"`
	Split   bool         `json:"split,omitempty"`
	Log     bool         `json:"log,omitempty"`
	Drawn   string       `json:"drawn,omitempty"` // what is shown: a range ("live" slides with the clock), "now" or "snapshot"
	Blocks  []block      `json:"blocks,omitempty"`
	Events  []event      `json:"events,omitempty"`
	Rows    []clockRow   `json:"rows,omitempty"`  // clock
	Clock   *ClockCfg    `json:"clock,omitempty"` // clock: the window, cell and states
	Mark    string       `json:"mark,omitempty"`  // days: bar | dot
	Bands   []band       `json:"bands,omitempty"` // a range behind a line, or a days page's usual range
	Empty   string       `json:"empty,omitempty"` // said when there is no data
}
type seriesJSON struct {
	Name string    `json:"name"`
	T    []float64 `json:"t"`
	V    []float64 `json:"v"`
}

func toTile(p page, m model) tileJSON {
	t := tileJSON{ID: p.ID, Title: p.Title, Info: p.Info, Kind: m.kind, Unit: m.unit, Note: m.note,
		YMin: m.yMin, YMax: m.yMax, Thermal: m.thermal, Stack: m.stack, Refs: m.refs, Bars: m.bars, Lines: m.lines, Step: m.step.Seconds(), Span: p.Span, Split: m.split, Events: m.events, Log: m.log, Blocks: m.blocks, Drawn: m.rng}
	if m.err != nil {
		t.Err = m.err.Error()
	}
	t.Mark, t.Bands, t.Empty = m.mark, m.bands, m.empty
	if m.kind == "clock" {
		c := m.clock
		t.Rows, t.Clock = m.rows, &c
	}
	if !m.start.IsZero() {
		t.Start, t.End = m.start.Unix(), m.end.Unix()
	}
	for _, s := range m.series {
		// an empty series keeps its slot (colour follows the entity) and goes out as [], never
		// null: a pack series that only one OS has is empty on the other
		if s.t == nil {
			s.t, s.v = []float64{}, []float64{}
		}
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

// served is everything one loaded catalogue gives the web face. It is swapped whole when the
// config file changes (watchConfig), so a request sees one consistent catalogue.
type served struct {
	cfg *Config
	all []page
	gc  *groupCache
	gen int64 // bumps on every reload; the page refetches its meta when it changes
}

func newServed(cfg *Config, gen int64) *served {
	all := cfg.pages()
	return &served{cfg: cfg, all: all, gc: newGroupCache(cfg, all, gen), gen: gen}
}

// serve runs the web face. path is the config file to watch for changes ("" = none, e.g. a
// -vm run with no file); vmOverride is the -vm flag, which a reloaded file must not undo.
func serve(cfg *Config, path, vmOverride, addr string) error {
	var cur atomic.Pointer[served]
	cur.Store(newServed(cfg, 1))
	if path != "" {
		go watchConfig(path, vmOverride, &cur)
	}
	srv := &http.Server{Addr: addr, Handler: handler(&cur), ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 60 * time.Second}
	fmt.Fprintf(os.Stderr, "askee: serving on http://%s/\n", addr)
	return srv.ListenAndServe()
}

func handler(cur *atomic.Pointer[served]) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("Vary", "Accept-Encoding")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(pageGz)
			return
		}
		w.Write(pageHTML)
	})
	mux.HandleFunc("GET /favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(faviconSVG)
	})
	mux.HandleFunc("GET /api/meta", func(w http.ResponseWriter, r *http.Request) {
		type g struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			N     int    `json:"n"`
			groupMode
		}
		s := cur.Load()
		cfg := s.cfg
		var gs []g
		for _, x := range cfg.Groups {
			if ps := groupPages(s.all, x.ID); len(ps) > 0 {
				gs = append(gs, g{x.ID, x.Title, len(ps), modeOf(ps)})
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
			"themes": ts, "default_theme": cfg.DefaultTheme, "dashes": dashPatterns, "thermal_range": cfg.Thermal, "gen": s.gen})
	})
	mux.HandleFunc("GET /api/group/{id}", func(w http.ResponseWriter, r *http.Request) {
		s := cur.Load()
		id := r.PathValue("id")
		ps := groupPages(s.all, id)
		rq := r.URL.Query().Get("range")
		rg, ok := rangeByName(rq)
		if len(ps) == 0 || (!ok && rq != "") {
			http.Error(w, "unknown group or range", http.StatusNotFound)
			return
		}
		if !ok {
			rg, _ = rangeByName(s.cfg.DefaultRange)
		}
		rg = modeOf(ps).clamp(rg) // never below what every chart in the group can show
		e := s.gc.get(id, rg)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Vary", "Accept-Encoding")
			w.Write(e.gz)
			return
		}
		w.Write(e.body)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	return mux
}

// watchConfig reloads the catalogue when its file changes, so editing the config file needs no
// restart. Every 30 s it compares the file's modification time and size; on a change it waits
// for the writer to finish (two equal readings a second apart), then loads the file through
// the same checks as at start. A file that fails to load is reported and the running
// catalogue stays: a half-saved or mistyped edit never takes the dashboard down.
// Not reloaded: the database URL (vm) and the live sources' URLs — those still need a restart.
func watchConfig(path, vmOverride string, cur *atomic.Pointer[served]) {
	sig := func() string {
		st, err := os.Stat(path)
		if err != nil {
			return ""
		}
		return fmt.Sprint(st.ModTime().UnixNano(), st.Size())
	}
	last := sig()
	for {
		time.Sleep(30 * time.Second)
		now := sig()
		if now == "" || now == last {
			continue
		}
		time.Sleep(time.Second)
		if again := sig(); again != now {
			continue // still being written; look again next round
		}
		last = now
		if err := reloadConfig(path, vmOverride, cur); err != nil {
			fmt.Fprintf(os.Stderr, "askee: %s changed but did not load, still serving the previous catalogue: %v\n", path, err)
		}
	}
}

func reloadConfig(path, vmOverride string, cur *atomic.Pointer[served]) error {
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	old := cur.Load()
	if vmOverride != "" {
		cfg.VM = vmOverride
	}
	if cfg.VM == "" {
		cfg.VM = old.cfg.VM
	}
	if cfg.VM != old.cfg.VM {
		fmt.Fprintf(os.Stderr, "askee: vm changed to %s — restart askee to use it; still querying %s\n", cfg.VM, old.cfg.VM)
		cfg.VM = old.cfg.VM
	}
	cfg.prune(30 * time.Second)
	reloadLive(cfg)
	cur.Store(newServed(cfg, old.gen+1))
	old.gc.close()
	fmt.Fprintf(os.Stderr, "askee: reloaded %s — %d groups, %d pages\n", path, len(cfg.Groups), len(cfg.Pages))
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ── group cache ─────────────────────────────────────────────────────────────────────
// Clicking a group must feel instant, so a request never waits on the TSDB if anything is
// cached: a stale entry is served at once and refreshed in the background (one refresh per
// key at a time). A warmer keeps every group at the default range fresh, so even the first
// click after a quiet hour is a cache hit. It does NOT warm the live range: a live entry is
// stale after 0.9 s, so warming it rebuilt every group every 20 s (32 of a 40-page catalogue's pages
// fall back to a 1h database query in the live view) and kept the once-a-second live reader
// running with nobody watching — 38% of a core on an idle box (2026-11-06). Live is only
// worth building for someone looking at it. Bodies are stored gzipped too: a
// 90-day group is ~100 KB of JSON and ~15 KB compressed, which matters over a phone link.
type cacheEntry struct {
	at       time.Time
	body, gz []byte
}

type groupCache struct {
	gen     int64
	stop    chan struct{} // closed when a reload replaces this cache: its warmer exits
	cfg     *Config
	all     []page
	mu      sync.Mutex
	entries map[string]*cacheEntry
	busy    map[string]chan struct{}
	tiles   map[string]tileCache // last GOOD tile per page and range
}

type tileCache struct {
	at time.Time
	t  tileJSON
}

// pageTTL: how long one page's answer stays good regardless of the group's refresh. Charts
// over a fixed month or more, and the 30-day hour-of-day profiles, move too slowly to re-ask.
func pageTTL(p page) time.Duration {
	if p.Kind == "hour" {
		return 15 * time.Minute
	}
	if r, ok := rangeByName(p.Range); ok && r.span >= 30*24*time.Hour {
		return 5 * time.Minute
	}
	return 0
}

func joinNote(a, b string) string {
	if b == "" {
		return a
	}
	return a + " · " + b
}

// ttl: how old an answer may be before it is refreshed, matched to how fast each range moves.
func ttl(r rng) time.Duration {
	switch {
	case r.name == "live":
		return 900 * time.Millisecond // a new sample every second
	case r.span <= time.Hour:
		return 30 * time.Second
	case r.span <= 6*time.Hour:
		return time.Minute
	default:
		return 2 * time.Minute
	}
}

func (gc *groupCache) close() { close(gc.stop) }

func newGroupCache(cfg *Config, all []page, gen int64) *groupCache {
	gc := &groupCache{gen: gen, stop: make(chan struct{}), cfg: cfg, all: all, entries: map[string]*cacheEntry{}, busy: map[string]chan struct{}{}, tiles: map[string]tileCache{}}
	go func() {
		for {
			select {
			case <-gc.stop:
				return
			default:
			}
			def, _ := rangeByName(cfg.DefaultRange)
			for _, g := range cfg.Groups {
				if len(groupPages(all, g.ID)) == 0 {
					continue
				}
				if def.name == "live" {
					continue // never warm live; see the comment above
				}
				gc.mu.Lock()
				e := gc.entries[g.ID+"|"+def.name]
				gc.mu.Unlock()
				if e == nil || time.Since(e.at) > ttl(def) {
					gc.refresh(g.ID, def)
				}
			}
			select {
			case <-gc.stop:
				return
			case <-time.After(20 * time.Second):
			}
		}
	}()
	return gc
}

func (gc *groupCache) get(id string, r rng) *cacheEntry {
	key := id + "|" + r.name
	gc.mu.Lock()
	e := gc.entries[key]
	gc.mu.Unlock()
	if e == nil {
		return gc.refresh(id, r) // nothing to show yet: wait for it
	}
	if time.Since(e.at) > ttl(r) {
		go gc.refresh(id, r) // serve what we have, fetch the new one behind it
	}
	return e
}

// refresh builds one group's JSON. Concurrent callers for the same key share one build.
func (gc *groupCache) refresh(id string, r rng) *cacheEntry {
	key := id + "|" + r.name
	gc.mu.Lock()
	if ch, ok := gc.busy[key]; ok {
		gc.mu.Unlock()
		<-ch
		gc.mu.Lock()
		e := gc.entries[key]
		gc.mu.Unlock()
		return e
	}
	ch := make(chan struct{})
	gc.busy[key] = ch
	gc.mu.Unlock()

	ps := groupPages(gc.all, id)
	tiles := make([]tileJSON, len(ps))
	var wg sync.WaitGroup
	for i, p := range ps {
		tk := p.ID + "|" + r.name
		gc.mu.Lock()
		last, have := gc.tiles[tk]
		gc.mu.Unlock()
		// a page that cannot have changed yet is not asked again (a 30-day profile, say)
		if have && time.Since(last.at) < pageTTL(p) {
			tiles[i] = last.t
			continue
		}
		wg.Add(1)
		go func(i int, p page) {
			defer wg.Done()
			t := toTile(p, p.fetch(r))
			gc.mu.Lock()
			defer gc.mu.Unlock()
			if t.Err == "" {
				gc.tiles[tk] = tileCache{time.Now(), t}
			} else if old, ok := gc.tiles[tk]; ok {
				// keep the last good chart and say how old it is, instead of an error
				st := old.t
				st.Note = joinNote(fmt.Sprintf("as of %s — %s", old.at.Format("15:04"), t.Err), old.t.Note)
				t = st
			}
			tiles[i] = t
		}(i, p)
	}
	wg.Wait()
	body, _ := json.Marshal(map[string]any{"group": id, "title": gc.cfg.groupTitle(id), "range": r.name,
		"generated": time.Now().Unix(), "gen": gc.gen, "tiles": tiles})
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	zw.Write(body)
	zw.Close()
	e := &cacheEntry{time.Now(), body, buf.Bytes()}

	gc.mu.Lock()
	gc.entries[key] = e
	delete(gc.busy, key)
	gc.mu.Unlock()
	close(ch)
	return e
}
