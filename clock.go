package main

// The clock kind: one row per day (or night), time of day across, so the same hour lines up
// down the page and a pattern — a late night, a lie-in, an afternoon walk — reads as a shape.
// It is the actogram sleep researchers draw, as small multiples on one shared clock axis.
//
// Two encodings, chosen by the config:
//
//   states  a value from a short list (sleep stages). Each cell is a block whose HEIGHT names
//           the state, first-listed tallest, so identity never rests on colour alone; colour
//           repeats it. In text the heights are block glyphs: █ ▆ ▄ ▂ for four states.
//   amount  a quantity (movement). Each cell is shaded by value on the theme's ramp, and in
//           text drawn as a block column ▁…█, one per cell group.
//
// Every row carries its number at the right end, the way a bar does: hours in the counted
// states, or minutes at or above a threshold. A cell with no reading is blank, never zero.

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// ClockCfg shapes a clock page.
//
//	"clock": {"from": 20, "hours": 14, "cell": 5,
//	          "states": [{"value": 4, "name": "awake"}, {"value": 3, "name": "REM"}, …],
//	          "count": "asleep"}
//
// from/hours: the window each row shows, in local time (20 + 14 runs 20:00 to 10:00 next
// day; a row that crosses midnight is named by the day it ends on, the day you woke). cell:
// minutes per cell, and the query step. states: top to bottom, tallest first; the row's
// number is the hours spent in every state but the first. Without states the page is an
// amount: threshold sets which cells count towards the row's minutes.
type ClockCfg struct {
	From      float64    `json:"from"`
	Hours     float64    `json:"hours"`
	Cell      int        `json:"cell"`
	States    []StateCfg `json:"states,omitempty"`
	Threshold *float64   `json:"threshold,omitempty"`
	Count     string     `json:"count,omitempty"` // what the row's number is called in -print ("asleep", "active")
}

type StateCfg struct {
	Value float64 `json:"value"`
	Name  string  `json:"name"`
}

type clockRow struct {
	Label string     `json:"label"`
	Start int64      `json:"start"` // unix seconds of the row's first cell
	V     []*float64 `json:"v"`     // nil = no reading
	Text  string     `json:"text"`
	Total float64    `json:"total"` // the row's number: hours (states) or minutes (amount)
}

func (c ClockCfg) cells() int { return int(math.Round(c.Hours * 60 / float64(c.Cell))) }

func (c ClockCfg) validate() error {
	switch {
	case c.Cell <= 0 || c.Hours <= 0 || c.Hours > 24:
		return fmt.Errorf("clock needs cell > 0 and 0 < hours <= 24")
	case c.From < 0 || c.From >= 24:
		return fmt.Errorf("clock from must be an hour 0–23")
	case c.cells() > 1440:
		return fmt.Errorf("clock has %d cells a row; use a longer cell", c.cells())
	}
	return nil
}

// clockRows lays one series out as rows. loc is the wall clock the rows follow, now the
// moment the page is drawn: a row whose window has not begun is left off, and so is every
// row before the first reading (an empty run before the data starts is not a gap in it).
func clockRows(s series, c ClockCfg, start, end, now time.Time, loc *time.Location) []clockRow {
	n := c.cells()
	cell := time.Duration(c.Cell) * time.Minute
	span := time.Duration(c.Hours * float64(time.Hour))
	h, m := int(c.From), int(math.Round((c.From-math.Floor(c.From))*60))
	day := func(t time.Time) time.Time {
		y, mo, d := t.In(loc).Date()
		return time.Date(y, mo, d, h, m, 0, 0, loc)
	}
	var rows []clockRow
	k := 0 // series index: points are in time order
	// from the day before start (a night window can begin then and end inside the range)
	for w := day(start).AddDate(0, 0, -1); !w.After(end) && !w.After(now); w = w.AddDate(0, 0, 1) {
		wEnd := w.Add(span)
		if !wEnd.After(start) {
			continue
		}
		r := clockRow{Label: wEnd.Add(-time.Second).Format("Mon 02"), Start: w.Unix(), V: make([]*float64, n)}
		for k < len(s.t) && s.t[k] < float64(w.Unix()) {
			k++
		}
		for j := k; j < len(s.t) && s.t[j] < float64(wEnd.Unix()); j++ {
			if math.IsNaN(s.v[j]) {
				continue
			}
			i := int((s.t[j] - float64(w.Unix())) / cell.Seconds())
			if i >= 0 && i < n {
				v := s.v[j]
				r.V[i] = &v
			}
		}
		r.Total, r.Text = c.rowTotal(r.V)
		rows = append(rows, r)
	}
	for len(rows) > 0 && empty(rows[0].V) {
		rows = rows[1:]
	}
	return rows
}

func empty(v []*float64) bool {
	for _, x := range v {
		if x != nil {
			return false
		}
	}
	return true
}

// rowTotal is the number printed at a row's right end.
func (c ClockCfg) rowTotal(v []*float64) (float64, string) {
	if empty(v) {
		return 0, ""
	}
	n := 0
	for _, x := range v {
		if x == nil {
			continue
		}
		switch {
		case len(c.States) > 0:
			if *x != c.States[0].Value {
				n++
			}
		case c.Threshold != nil:
			if *x >= *c.Threshold {
				n++
			}
		default:
			n++
		}
	}
	mins := float64(n * c.Cell)
	if len(c.States) > 0 {
		return mins / 60, fmt.Sprintf("%.1f h", mins/60)
	}
	return mins, fmt.Sprintf("%.0f min", mins)
}

// stateIndex is a value's place in the states list (0 = first, tallest), or -1.
func (c ClockCfg) stateIndex(v float64) int {
	for i, s := range c.States {
		if s.Value == v {
			return i
		}
	}
	return -1
}

// ticks are 3–5 clock-hour labels across the window, as hour offsets from its start.
func (c ClockCfg) ticks() []float64 {
	every := 1.0
	for _, e := range []float64{1, 2, 3, 4, 6, 8, 12} {
		every = e
		if c.Hours/e <= 5 {
			break
		}
	}
	first := math.Ceil(c.From/every)*every - c.From
	var out []float64
	for o := first; o <= c.Hours+1e-9; o += every {
		out = append(out, o)
	}
	return out
}

func clockHour(c ClockCfg, offset float64) string {
	h := math.Mod(c.From+offset, 24)
	return fmt.Sprintf("%02d:%02d", int(h), int(math.Round((h-math.Floor(h))*60)))
}

// ── terminal and -print ──────────────────────────────────────────────────────────────
// One text row per day: label, a run of block glyphs (each column the cells under it, the
// most frequent state or the largest amount), then the row's number. The newest rows win
// when the box is short.
func drawClock(s *screen, m model, x0, y0, x1, y1 int) {
	th := s.th
	c := m.clock
	if len(m.rows) == 0 {
		s.text(x0+2, y0+2, "no data in this range", th.Dim, false)
		return
	}
	lw, tw := 6, 0
	for _, r := range m.rows {
		tw = max(tw, len([]rune(r.Text)))
	}
	gx := x0 + 3 + lw
	gw := x1 - gx - tw - 3
	if gw < 8 {
		return
	}
	top := y0 + 1
	if len(c.States) > 0 { // the key: each state's glyph, once for the whole chart
		x := gx
		for i, st := range c.States {
			x = s.text(x, top, string(stateGlyph(i, len(c.States)))+" "+st.Name+"  ", th.Palette[i%len(th.Palette)], false)
		}
		top++
	}
	room := y1 - 1 - top // the last line is the clock axis
	rows := m.rows
	if len(rows) > room {
		rows = rows[len(rows)-room:]
	}
	mx := 0.0
	for _, r := range m.rows {
		for _, v := range r.V {
			if v != nil {
				mx = math.Max(mx, *v)
			}
		}
	}
	ramp := th.seqRamp()
	n := c.cells()
	for ri, r := range rows {
		y := top + ri
		s.text(x0+2, y, r.Label, th.Text, false)
		for col := 0; col < gw; col++ {
			a, b := col*n/gw, max((col+1)*n/gw, col*n/gw+1)
			if len(c.States) > 0 {
				count := make([]int, len(c.States))
				best, seen := -1, false
				for _, v := range r.V[a:min(b, n)] {
					if v == nil {
						continue
					}
					if i := c.stateIndex(*v); i >= 0 {
						count[i]++
						seen = true
						if best < 0 || count[i] > count[best] {
							best = i
						}
					}
				}
				if seen {
					s.set(gx+col, y, stateGlyph(best, len(c.States)), th.Palette[best%len(th.Palette)], false)
				}
				continue
			}
			v, seen := 0.0, false
			for _, x := range r.V[a:min(b, n)] {
				if x != nil {
					v, seen = math.Max(v, *x), true
				}
			}
			if seen && mx > 0 {
				f := v / mx
				s.set(gx+col, y, rune(0x2581+int(math.Min(7, f*7.999))), rampAt(ramp, f), false)
			}
		}
		s.text(gx+gw+2, y, r.Text, th.Text, false)
	}
	// the clock axis
	ay := top + len(rows)
	for _, o := range c.ticks() {
		x := gx + int(o/c.Hours*float64(gw-1))
		lab := clockHour(c, o)
		if x+len(lab) > x1-1 {
			x = x1 - 1 - len(lab)
		}
		s.text(x, ay, lab, th.Dim, false)
	}
}

// stateGlyph: the first state is the tallest block, the last the shortest.
func stateGlyph(i, n int) rune {
	if n <= 1 || i < 0 {
		return '█'
	}
	h := 8 - int(math.Round(float64(i)*6/float64(n-1))) // 8 … 2 eighths
	return rune(0x2580 + h)
}

// clockSummary is -print's numbers: each row's total and, for states, the hours in each.
func clockSummary(m model) []string {
	c := m.clock
	var out []string
	count := c.Count
	if count == "" {
		count = "total"
	}
	for _, r := range m.rows {
		if empty(r.V) {
			out = append(out, fmt.Sprintf("  %-8s no reading", r.Label))
			continue
		}
		line := fmt.Sprintf("  %-8s %s %s", r.Label, count, r.Text)
		if len(c.States) > 0 {
			hrs := make([]float64, len(c.States))
			for _, v := range r.V {
				if v != nil {
					if i := c.stateIndex(*v); i >= 0 {
						hrs[i] += float64(c.Cell) / 60
					}
				}
			}
			var parts []string
			for i, st := range c.States {
				parts = append(parts, fmt.Sprintf("%s %.1f", st.Name, hrs[i]))
			}
			line += " · " + strings.Join(parts, " · ") + " h"
		} else {
			mx, sum, k := 0.0, 0.0, 0
			for _, v := range r.V {
				if v != nil {
					mx, sum, k = math.Max(mx, *v), sum+*v, k+1
				}
			}
			line += fmt.Sprintf(" · max %s · mean %s", fmtVal(mx), fmtVal(sum/float64(k)))
		}
		out = append(out, line)
	}
	return out
}
