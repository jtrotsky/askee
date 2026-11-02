package main

// Terminal face: a btm-style deck. Boxed panels, braille line graphs (2×4 dots per cell),
// eighth-block bars, colours from the active theme (truecolor when COLORTERM says so,
// otherwise the nearest xterm-256 entry).

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type cell struct {
	r    rune
	fg   string
	bold bool
}

type screen struct {
	w, h  int
	cells []cell
	th    Theme
}

func newScreen(w, h int, th Theme) *screen {
	s := &screen{w: w, h: h, cells: make([]cell, w*h), th: th}
	for i := range s.cells {
		s.cells[i] = cell{' ', th.Text, false}
	}
	return s
}

func (s *screen) set(x, y int, r rune, fg string, bold bool) {
	if x < 0 || y < 0 || x >= s.w || y >= s.h {
		return
	}
	s.cells[y*s.w+x] = cell{r, fg, bold}
}

func (s *screen) text(x, y int, str string, fg string, bold bool) int {
	for _, r := range str {
		s.set(x, y, r, fg, bold)
		x++
	}
	return x
}

type legendItem struct {
	text, color string
}

func (s *screen) box(x0, y0, x1, y1 int, title, sub string, legend []legendItem) {
	th := s.th
	for x := x0 + 1; x < x1; x++ {
		s.set(x, y0, '─', th.Border, false)
		s.set(x, y1, '─', th.Border, false)
	}
	for y := y0 + 1; y < y1; y++ {
		s.set(x0, y, '│', th.Border, false)
		s.set(x1, y, '│', th.Border, false)
	}
	s.set(x0, y0, '┌', th.Border, false)
	s.set(x1, y0, '┐', th.Border, false)
	s.set(x0, y1, '└', th.Border, false)
	s.set(x1, y1, '┘', th.Border, false)
	if max := x1 - x0 - 6; len([]rune(title)) > max && max > 1 {
		title = string([]rune(title)[:max-1]) + "…"
	}
	x := s.text(x0+2, y0, " "+title+" ", th.Title, true)
	// sub is "[range] so-what": the range tag always shows; the so-what only if it fits
	tag, rest, _ := strings.Cut(sub, " ")
	if tag != "" {
		x = s.text(x, y0, tag+" ", th.Dim, false)
	}
	if rest != "" && x+len([]rune(rest))+2 < x1-24 {
		x = s.text(x, y0, rest+" ", th.Dim, false)
	}
	// legend right-aligned on the top border: a coloured swatch, then text in TEXT ink
	// (dataviz: text never wears the series colour). Items that don't fit are dropped.
	total := 0
	var fit []legendItem
	for _, l := range legend {
		n := len([]rune(l.text)) + 4
		if x1-2-(total+n) < x+1 {
			break
		}
		total += n
		fit = append(fit, l)
	}
	lx := x1 - 1 - total
	for _, l := range fit {
		s.set(lx, y0, ' ', th.Text, false)
		s.set(lx+1, y0, '■', l.color, false)
		s.set(lx+2, y0, ' ', th.Text, false)
		s.text(lx+3, y0, l.text, th.Text, false)
		s.set(lx+3+len([]rune(l.text)), y0, ' ', th.Text, false)
		lx += len([]rune(l.text)) + 4
	}
}

func (s *screen) flush(out *bufio.Writer, truecolor bool) {
	out.WriteString("\x1b[H")
	bg := sgrBG(s.th.Surface, truecolor)
	cur, curB := "", false
	for y := 0; y < s.h; y++ {
		for x := 0; x < s.w; x++ {
			c := s.cells[y*s.w+x]
			if c.fg != cur || c.bold != curB {
				b := "0"
				if c.bold {
					b = "0;1"
				}
				fmt.Fprintf(out, "\x1b[%s;%s;%sm", b, bg, sgrFG(c.fg, truecolor))
				cur, curB = c.fg, c.bold
			}
			out.WriteRune(c.r)
		}
		if y < s.h-1 {
			out.WriteString("\r\n")
		}
	}
	out.WriteString("\x1b[0m")
	out.Flush()
}

// ── drawing ───────────────────────────────────────────────────────────────────────
func fmtVal(v float64) string {
	a := math.Abs(v)
	switch {
	case a >= 1e6:
		return fmt.Sprintf("%.2fM", v/1e6)
	case a >= 1e4:
		return fmt.Sprintf("%.1fk", v/1e3)
	case a >= 1000:
		return fmt.Sprintf("%d,%03d", int(v)/1000, int(math.Abs(v))%1000) // thousands separator (Wei)
	case a >= 100:
		return fmt.Sprintf("%.0f", v)
	case a >= 10:
		return fmt.Sprintf("%.1f", v)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

// stacked returns cumulative series (parts of a whole), bottom first.
func stacked(ss []series) []series {
	out := make([]series, len(ss))
	var acc map[float64]float64 = map[float64]float64{}
	for i, s := range ss {
		n := series{name: s.name, t: append([]float64{}, s.t...), v: make([]float64, len(s.v))}
		for k := range s.t {
			acc[s.t[k]] += s.v[k]
			n.v[k] = acc[s.t[k]]
		}
		out[i] = n
	}
	return out
}

func yRange(m model, ss []series) (float64, float64, bool) {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, se := range ss {
		for _, v := range se.v {
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
	}
	for _, r := range m.refs {
		lo, hi = math.Min(lo, r.V), math.Max(hi, r.V)
	}
	if math.IsInf(lo, 1) {
		return 0, 0, false
	}
	if m.yMin != nil {
		lo = math.Min(lo, *m.yMin)
	} else {
		pad := (hi - lo) * 0.1
		if pad == 0 {
			pad = math.Max(1, math.Abs(hi)*0.1)
		}
		lo -= pad
	}
	if m.yMax != nil {
		hi = math.Max(hi, *m.yMax)
		if *m.yMax == 100 && hi > 100 {
			hi = 100
		}
	} else {
		hi += (hi - lo) * 0.1
	}
	if hi <= lo {
		hi = lo + 1
	}
	return lo, hi, true
}

// braille dash masks for mono themes, by series: solid, dashed, dotted, dash-dot, long dash.
// Indexed by the dot column (2 per cell): true = ink.
var dashMasks = [][]bool{
	{true},
	{true, true, true, true, false, false},
	{true, false, false},
	{true, true, true, true, false, false, true, false, false},
	{true, true, true, true, true, true, true, false, false},
	{true, true, false, false},
}

// legendGlyph shows the line style beside a mono legend entry.
var legendGlyph = []string{"━━", "╍╍", "┈┈", "─·", "━ ", "╌╌"}

// drawSplit draws small multiples: one band per series, each on its own scale and named at its
// right end, sharing one time axis at the bottom.
func drawSplit(s *screen, m model, x0, y0, x1, y1 int, thermal [2]float64) {
	n := len(m.series)
	inner := y1 - y0 - 2 // rows between the borders, less one for the time axis
	h := inner / n
	for i, se := range m.series {
		sub := m
		sub.series, sub.refs, sub.split = []series{se}, nil, false
		if i > 0 {
			sub.events = nil
		}
		top := y0 + i*h
		bot := top + h + 1
		last := i == n-1
		if last {
			bot = y1
		}
		drawLineArea(s, sub, x0, top, x1, bot, thermal, last)
		if k := len(se.v); k > 0 {
			lbl := " " + se.name + " " + fmtVal(se.v[k-1]) + m.unit + " "
			s.text(x1-1-len([]rune(lbl)), top+1, lbl, s.th.Text, false)
		}
	}
}

func drawLine(s *screen, m model, x0, y0, x1, y1 int, thermal [2]float64) []legendItem {
	if m.split && len(m.series) > 1 {
		drawSplit(s, m, x0, y0, x1, y1, thermal)
		return nil
	}
	return drawLineArea(s, m, x0, y0, x1, y1, thermal, true)
}

// drawLineArea draws one panel. The y axis is a range frame (Tufte): ticks only at the data's
// own min and max, no grid; a fixed scale end (0, 100 %) is stated once, dimmer.
func drawLineArea(s *screen, m model, x0, y0, x1, y1 int, thermal [2]float64, xAxis bool) []legendItem {
	th := s.th
	gut := 8
	px0, py0, px1, py1 := x0+gut, y0+1, x1-1, y1-2
	if !xAxis {
		py1 = y1 - 1
	}
	pw, ph := px1-px0, py1-py0+1
	if pw < 4 || ph < 2 {
		return nil
	}
	ss := m.series
	if m.stack {
		ss = stacked(m.series)
	}
	lo, hi, ok := yRange(m, ss)
	if !ok {
		s.text(px0+2, py0+ph/2, "no data in this range", th.Dim, false)
		return nil
	}
	Y := func(v float64) int { return py0 + int((1-(v-lo)/(hi-lo))*float64(ph-1)+0.5) }
	dlo, dhi := math.Inf(1), math.Inf(-1)
	for _, se := range ss {
		for _, v := range se.v {
			dlo, dhi = math.Min(dlo, v), math.Max(dhi, v)
		}
	}
	tick := func(v float64, ink string) {
		y := Y(v)
		lbl := fmtVal(v)
		s.text(px0-1-len([]rune(lbl)), y, lbl, ink, false)
		s.set(px0-1, y, '┤', th.Border, false)
	}
	for y := Y(dhi); y <= Y(dlo); y++ {
		s.set(px0-1, y, '│', th.Border, false)
	}
	if m.yMin != nil && Y(lo) > Y(dlo)+1 {
		tick(lo, th.Dim)
	}
	if m.yMax != nil && Y(hi) < Y(dhi)-1 {
		tick(hi, th.Dim)
	}
	tick(dhi, th.Text)
	if Y(dlo) > Y(dhi) {
		tick(dlo, th.Text)
	}
	// events (reboots): a dotted hairline and a word, drawn under the data
	t0e, t1e := float64(m.start.Unix()), float64(m.end.Unix())
	lastX := -99
	var evLabels []legendItem
	var evX []int
	for _, e := range m.events {
		if e.T < t0e || e.T > t1e {
			continue
		}
		x := px0 + int((e.T-t0e)/(t1e-t0e)*float64(pw-1))
		for y := py0; y <= py1; y++ {
			s.set(x, y, '┊', th.Dim, false)
		}
		if x-lastX > len(e.Label)+2 && x+len(e.Label) < px1 {
			evLabels = append(evLabels, legendItem{e.Label, ""})
			evX = append(evX, x)
			lastX = x
		}
	}
	// reference lines ("compared to what"): dashed, dim, labelled at the right
	for _, r := range m.refs {
		y := Y(r.V)
		for x := px0; x < px1; x += 2 {
			s.set(x, y, '┄', th.Dim, false)
		}
	}
	dw, dh := pw*2, ph*4
	dots := make([]uint8, pw*ph)
	col := make([]string, pw*ph)
	val := make([]float64, pw*ph)
	bits := [4][2]uint8{{0x01, 0x08}, {0x02, 0x10}, {0x04, 0x20}, {0x40, 0x80}}
	mask := []bool{true}
	plot := func(dx, dy int, c string, v float64) {
		if dx < 0 || dy < 0 || dx >= dw || dy >= dh {
			return
		}
		if !mask[dx%len(mask)] {
			return
		}
		i := (dy/4)*pw + dx/2
		dots[i] |= bits[dy%4][dx%2]
		col[i], val[i] = c, v
	}
	t0, t1 := float64(m.start.Unix()), float64(m.end.Unix())
	gap := 3 * m.step.Seconds()
	var legend []legendItem
	for si, se := range ss {
		c := th.Palette[si%len(th.Palette)]
		if th.Mono || th.Dashed {
			mask = dashMasks[si%len(dashMasks)]
		} else {
			mask = []bool{true}
		}
		px, py := -1, -1
		var pv, lastT float64
		for i := range se.t {
			dx := int((se.t[i] - t0) / (t1 - t0) * float64(dw-1))
			dy := dh - 1 - int((se.v[i]-lo)/(hi-lo)*float64(dh-1)+0.5)
			if px >= 0 && se.t[i]-lastT <= gap {
				ax, ay, bx, by := px, py, dx, dy
				ddx, ddy := abs(bx-ax), -abs(by-ay)
				sx, sy := sign(bx-ax), sign(by-ay)
				e := ddx + ddy
				steps := max(1, max(abs(bx-ax), abs(by-ay)))
				k := 0
				for {
					f := float64(k) / float64(steps)
					plot(ax, ay, c, pv+(se.v[i]-pv)*f)
					if ax == bx && ay == by {
						break
					}
					e2 := 2 * e
					if e2 >= ddy {
						e += ddy
						ax += sx
					}
					if e2 <= ddx {
						e += ddx
						ay += sy
					}
					k++
				}
			} else {
				plot(dx, dy, c, se.v[i])
			}
			px, py, pv, lastT = dx, dy, se.v[i], se.t[i]
		}
		orig := m.series[si]
		if n := len(orig.v); n > 0 {
			txt := fmt.Sprintf("%s %s%s", orig.name, fmtVal(orig.v[n-1]), m.unit)
			if th.Mono || th.Dashed {
				txt = legendGlyph[si%len(legendGlyph)] + " " + txt
			}
			legend = append(legend, legendItem{txt, c})
		}
	}
	for i := range dots {
		if dots[i] != 0 {
			c := col[i]
			if m.thermal && !th.Mono {
				c = th.thermalAt(val[i], thermal)
			}
			s.set(px0+i%pw, py0+i/pw, rune(0x2800+int(dots[i])), c, false)
		}
	}
	for i, l := range evLabels { // labels last, so data never overprints them
		s.text(evX[i]+1, py0, l.text, th.Dim, false)
	}
	for _, r := range m.refs {
		lbl := " " + r.Label + " " + fmtVal(r.V) + m.unit + " "
		s.text(px1-len([]rune(lbl)), Y(r.V), lbl, th.Dim, false)
	}
	layout := "15:04"
	if m.end.Sub(m.start) > 36*time.Hour {
		layout = "02 Jan"
	}
	for _, f := range []float64{0, 0.5, 1} {
		if !xAxis {
			break
		}
		t := m.start.Add(time.Duration(f * float64(m.end.Sub(m.start))))
		lbl := t.Format(layout)
		x := px0 + int(f*float64(pw-1)) - len(lbl)/2
		if f == 0 {
			x = px0
		}
		if f == 1 {
			x = px1 - len(lbl)
		}
		s.text(x, py1+1, lbl, th.Dim, false)
	}
	return legend
}

func statsLine(m model) string {
	if len(m.series) == 0 || len(m.series[0].v) == 0 {
		return ""
	}
	v := m.series[0].v
	mn, mx, sum := v[0], v[0], 0.0
	for _, x := range v {
		mn, mx, sum = math.Min(mn, x), math.Max(mx, x), sum+x
	}
	return fmt.Sprintf(" %s: min %s · max %s · mean %s %s ", m.series[0].name, fmtVal(mn), fmtVal(mx), fmtVal(sum/float64(len(v))), m.unit)
}

// drawBars: eighth-block bars coloured by magnitude on the theme's one-hue sequential ramp —
// the gradient carries the value, it is not decoration.
func drawBars(s *screen, m model, x0, y0, x1, y1 int) {
	th := s.th
	if len(m.bars) == 0 {
		s.text(x0+2, y0+2, "no data", th.Dim, false)
		return
	}
	lw, mx := 0, 0.0
	for _, b := range m.bars {
		lw = max(lw, len([]rune(b.Label)))
		mx = math.Max(mx, b.Value)
	}
	if m.unit == "%" { // a share of capacity is drawn against the whole, not against the largest bar
		mx = math.Max(mx, 100)
	}
	rows := y1 - y0 - 1
	if m.note != "" {
		rows--
	}
	bs := m.bars
	if len(bs) > rows {
		bs = bs[len(bs)-rows:]
	}
	ramp := th.seqRamp()
	bw := x1 - x0 - 3 - lw - 1 - 12
	for i, b := range bs {
		y := y0 + 1 + i
		s.text(x0+2, y, b.Label, th.Text, false)
		f := 0.0
		if mx > 0 {
			f = b.Value / mx
		}
		c := rampAt(ramp, f)
		eighths := int(f*float64(bw)*8 + 0.5)
		x := x0 + 3 + lw
		for k := 0; k < eighths/8; k++ {
			s.set(x, y, '█', c, false)
			x++
		}
		if r := eighths % 8; r > 0 {
			s.set(x, y, rune(0x2590-r), c, false)
			x++
		}
		s.text(x+1, y, fmtVal(b.Value)+" "+m.unit, th.Text, false)
	}
	if m.note != "" {
		s.text(x0+2, y1-1, m.note, th.Dim, false)
	}
}

func drawText(s *screen, m model, x0, y0, x1, y1 int) {
	th := s.th
	ramp := th.seqRamp()
	for i, l := range m.lines {
		y := y0 + 1 + i
		if y >= y1-1 && m.note != "" || y >= y1 {
			s.text(x0+2, y, fmt.Sprintf("… %d more lines", len(m.lines)-i), th.Dim, false)
			break
		}
		x := x0 + 2
		for _, r := range l {
			if x >= x1 {
				break
			}
			c := th.Text
			switch {
			case r >= 0x2580 && r <= 0x259F:
				c = ramp[len(ramp)-1]
			case r >= 0x2800 && r <= 0x28FF:
				c = th.Palette[1]
			}
			s.set(x, y, r, c, false)
			x++
		}
	}
	if m.note != "" {
		s.text(x0+2, y1-1, m.note, th.Dim, false)
	}
}

func abs(a int) int {
	if a < 0 {
		return -a
	}
	return a
}
func sign(a int) int {
	switch {
	case a > 0:
		return 1
	case a < 0:
		return -1
	}
	return 0
}

// panel draws one page into a box — shared by the deck and -print.
func panel(s *screen, cfg *Config, p page, m model, x0, y0, x1, y1 int, rangeName string) {
	th := s.th
	title := p.Title
	tag := rangeName
	if p.Range != "" {
		tag = p.Range
	}
	sub := "[" + tag + "]"
	switch {
	case m.err != nil:
		s.box(x0, y0, x1, y1, title, sub, nil)
		s.text(x0+3, y0+2, "error: "+m.err.Error(), th.Accent, false)
	case m.kind == "line":
		leg := drawLine(s, m, x0, y0, x1, y1, cfg.Thermal)
		s.box(x0, y0, x1, y1, title, sub, leg)
		if st := statsLine(m); st != "" && !m.split {
			s.text(x1-1-len([]rune(st)), y1, st, th.Dim, false)
		}
	case m.kind == "bars":
		s.box(x0, y0, x1, y1, title, sub, nil)
		drawBars(s, m, x0, y0, x1, y1)
	default:
		s.box(x0, y0, x1, y1, title, sub, nil)
		drawText(s, m, x0, y0, x1, y1)
	}
}

// ── interactive deck ──────────────────────────────────────────────────────────────
type deck struct {
	cfg   *Config
	pages []page
	cur   int
	ri    int
	ti    int
	mu    sync.Mutex
	cache map[string]model
	when  map[string]time.Time
	busy  bool
}

func (d *deck) key() string {
	p := d.pages[d.cur]
	r := ranges[d.ri].name
	if p.Range != "" {
		r = p.Range
	}
	return fmt.Sprintf("%d|%s", d.cur, r)
}

func (d *deck) get() (model, bool, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := d.key()
	m, ok := d.cache[k]
	r := ranges[d.ri]
	if d.pages[d.cur].Range != "" {
		r, _ = rangeByName(d.pages[d.cur].Range)
	}
	return m, ok, ok && time.Since(d.when[k]) < ttl(r) // the same cadence as the web face
}

func (d *deck) load(force bool, redraw chan<- struct{}) {
	if _, _, fresh := d.get(); fresh && !force {
		return
	}
	d.mu.Lock()
	if d.busy {
		d.mu.Unlock()
		return
	}
	d.busy = true
	k, p, r := d.key(), d.pages[d.cur], ranges[d.ri]
	d.mu.Unlock()
	go func() {
		m := p.fetch(r)
		d.mu.Lock()
		if old, ok := d.cache[k]; ok && m.err != nil && old.err == nil {
			// keep the last good chart, say how old it is, and retry on the next tick
			old.note = joinNote(fmt.Sprintf("as of %s — %v", d.when[k].Format("15:04:05"), m.err), "")
			m = old
			d.cache[k], d.busy = m, false
		} else {
			d.cache[k], d.when[k], d.busy = m, time.Now(), false
		}
		d.mu.Unlock()
		redraw <- struct{}{}
	}()
}

func termSize() (int, int) {
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err == nil {
		var h, w int
		if _, err := fmt.Sscan(string(out), &h, &w); err == nil && w > 0 && h > 0 {
			return w, h
		}
	}
	return 100, 30
}

func (d *deck) render(w, h int) *screen {
	th := themes[d.ti]
	s := newScreen(w, h, th)
	p := d.pages[d.cur]
	x := s.text(1, 0, d.cfg.Title, th.Title, true)
	x = s.text(x+2, 0, d.cfg.groupTitle(p.Group), th.Accent, true)
	x = s.text(x+1, 0, fmt.Sprintf("%d/%d", d.cur+1, len(d.pages)), th.Dim, false)
	rx := x + 2
	for i, r := range ranges {
		c, b := th.Dim, false
		if i == d.ri {
			c, b = th.Accent, true
		}
		rx = s.text(rx, 0, " "+r.name+" ", c, b)
	}
	tn := "theme: " + th.Name
	switch th.Status {
	case "fails":
		tn += " (not colour-blind safe)"
	case "mono":
		tn += " (print: line styles)"
	}
	clock := time.Now().Format("15:04:05")
	s.text(w-1-len([]rune(tn))-len(clock)-3, 0, tn, th.Dim, false)
	s.text(w-1-len(clock), 0, clock, th.Dim, false)
	m, have, _ := d.get()
	if !have {
		s.box(0, 1, w-1, h-3, p.Title, "", nil)
		s.text(3, 3, "loading…", th.Dim, false)
	} else {
		// live, as btm does it: the chart does not slide between samples. Each refetch repaints
		// it with the right edge at the newest sample, so it changes in one step or not at all.
		panel(s, d.cfg, p, m, 0, 1, w-1, h-3, ranges[d.ri].name)
	}
	prev, next := "", ""
	if d.cur > 0 {
		prev = "◀ " + d.pages[d.cur-1].Title
	}
	if d.cur < len(d.pages)-1 {
		next = d.pages[d.cur+1].Title + " ▶"
	}
	s.text(1, h-2, prev, th.Dim, false)
	s.text(w-1-len([]rune(next)), h-2, next, th.Dim, false)
	help := "←/→ or click: page   ↑/↓ or 1-7: range   t: theme   r: refresh   q: quit"
	s.text((w-len([]rune(help)))/2, h-1, help, th.Border, false)
	return s
}

func runDeck(cfg *Config, startPage int) error {
	d := &deck{cfg: cfg, pages: cfg.pages(), cache: map[string]model{}, when: map[string]time.Time{}}
	d.ri = 2
	if r, ok := rangeByName(cfg.DefaultRange); ok {
		for i := range ranges {
			if ranges[i].name == r.name {
				d.ri = i
			}
		}
	}
	want := cfg.DefaultTheme
	if want == "auto" { // a terminal cannot be asked; COLORFGBG (set by many) says "fg;bg"
		want = "carbon"
		if bg := os.Getenv("COLORFGBG"); strings.HasSuffix(bg, ";15") || strings.HasSuffix(bg, ";7") {
			want = "newsprint"
		}
	}
	for i, t := range themes {
		if t.ID == want {
			d.ti = i
		}
	}
	if env := os.Getenv("ASKEE_THEME"); env != "" {
		for i, t := range themes {
			if t.ID == env {
				d.ti = i
			}
		}
	}
	d.cur = max(0, min(startPage-1, len(d.pages)-1))
	truecolor := strings.Contains(os.Getenv("COLORTERM"), "truecolor") || strings.Contains(os.Getenv("COLORTERM"), "24bit")

	save := exec.Command("stty", "-g")
	save.Stdin = os.Stdin
	saved, err := save.Output()
	if err != nil {
		return fmt.Errorf("needs a terminal (try -print <group> or -dump N)")
	}
	raw := exec.Command("stty", "raw", "-echo")
	raw.Stdin = os.Stdin
	raw.Run()
	out := bufio.NewWriterSize(os.Stdout, 1<<16)
	out.WriteString("\x1b[?1049h\x1b[?25l\x1b[?1000h")
	out.Flush()
	defer func() {
		out.WriteString("\x1b[?1000l\x1b[?25h\x1b[?1049l\x1b[0m")
		out.Flush()
		r := exec.Command("stty", strings.TrimSpace(string(saved)))
		r.Stdin = os.Stdin
		r.Run()
	}()

	redraw := make(chan struct{}, 8)
	keys := make(chan string, 16)
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(keys)
				return
			}
			keys <- string(buf[:n])
		}
	}()
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGWINCH, syscall.SIGTERM, syscall.SIGINT)
	tick := time.NewTicker(time.Second)
	w, h := termSize()
	draw := func() { d.render(w, h).flush(out, truecolor) }
	d.load(false, redraw)
	draw()
	for {
		select {
		case <-redraw:
			draw()
		case sg := <-sig:
			if sg == syscall.SIGWINCH {
				w, h = termSize()
				out.WriteString("\x1b[2J")
				draw()
				continue
			}
			return nil
		case <-tick.C:
			d.load(false, redraw)
			draw()
		case k, ok := <-keys:
			if !ok {
				return nil
			}
			before := d.key()
			for len(k) > 0 {
				if strings.HasPrefix(k, "\x1b[M") && len(k) >= 6 {
					b, mx := int(k[3])-32, int(k[4])-33
					switch {
					case b == 64:
						d.cur = max(0, d.cur-1)
					case b == 65:
						d.cur = min(len(d.pages)-1, d.cur+1)
					case b&3 == 0:
						if mx < w/2 {
							d.cur = max(0, d.cur-1)
						} else {
							d.cur = min(len(d.pages)-1, d.cur+1)
						}
					}
					k = k[6:]
					continue
				}
				if strings.HasPrefix(k, "\x1b[") && len(k) >= 3 {
					switch k[2] {
					case 'D':
						d.cur = max(0, d.cur-1)
					case 'C':
						d.cur = min(len(d.pages)-1, d.cur+1)
					case 'A':
						d.ri = max(0, d.ri-1)
					case 'B':
						d.ri = min(len(ranges)-1, d.ri+1)
					}
					k = k[3:]
					continue
				}
				switch k[0] {
				case 'q', 3, 4:
					return nil
				case 'h':
					d.cur = max(0, d.cur-1)
				case 'l', ' ':
					d.cur = min(len(d.pages)-1, d.cur+1)
				case 'k':
					d.ri = max(0, d.ri-1)
				case 'j':
					d.ri = min(len(ranges)-1, d.ri+1)
				case 't':
					d.ti = (d.ti + 1) % len(themes)
					out.WriteString("\x1b[2J")
				case 'r':
					d.load(true, redraw)
				default:
					if k[0] >= '1' && k[0] <= '7' {
						d.ri = int(k[0] - '1')
					}
				}
				k = k[1:]
			}
			if d.key() != before {
				d.load(false, redraw)
			}
			draw()
		}
	}
}
