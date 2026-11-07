package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

var sleepClock = ClockCfg{From: 20, Hours: 14, Cell: 5, Count: "asleep",
	States: []StateCfg{{4, "awake"}, {3, "REM"}, {2, "light"}, {1, "deep"}}}

// A night that starts before midnight lands in ONE row, named by the morning it ends on,
// with each 5-minute point in its own cell and the row's number counting every state but awake.
func TestClockRowsNight(t *testing.T) {
	loc, _ := time.LoadLocation("Pacific/Auckland")
	bed := time.Date(2026, 11, 7, 23, 0, 0, 0, loc) // Saturday night
	var s series
	codes := []float64{4, 2, 2, 1, 1, 3, 4} // 7 cells: 5 asleep, 2 awake
	for i, c := range codes {
		s.t = append(s.t, float64(bed.Add(time.Duration(i)*5*time.Minute).Unix()))
		s.v = append(s.v, c)
	}
	start := time.Date(2026, 11, 5, 12, 0, 0, 0, loc)
	end := time.Date(2026, 11, 8, 12, 0, 0, 0, loc)
	rows := clockRows(s, sleepClock, start, end, end, loc)
	if len(rows) != 1 {
		t.Fatalf("want 1 row (leading empty rows dropped), got %d: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Label != "Sun 08" {
		t.Errorf("label %q, want the wake day Sun 08", r.Label)
	}
	first := int((bed.Sub(time.Date(2026, 11, 7, 20, 0, 0, 0, loc))) / (5 * time.Minute)) // 36
	for i, c := range codes {
		if v := r.V[first+i]; v == nil || *v != c {
			t.Errorf("cell %d = %v, want %v", first+i, v, c)
		}
	}
	if r.V[first-1] != nil || r.V[first+len(codes)] != nil {
		t.Error("cells outside the readings must stay empty (missing is blank, not zero)")
	}
	if math.Abs(r.Total-25.0/60) > 1e-9 || r.Text != "0.4 h" {
		t.Errorf("total %v %q, want 25 min = 0.4 h", r.Total, r.Text)
	}
}

// A gap night in the middle stays as an empty row; a row whose window has not begun is left off.
func TestClockRowsGapAndFuture(t *testing.T) {
	loc, _ := time.LoadLocation("Pacific/Auckland")
	day := ClockCfg{From: 0, Hours: 24, Cell: 15, Threshold: ptr(2), Count: "active"}
	var s series
	for _, d := range []int{1, 3} {
		at := time.Date(2026, 11, d, 10, 0, 0, 0, loc)
		s.t, s.v = append(s.t, float64(at.Unix())), append(s.v, 3)
		s.t, s.v = append(s.t, float64(at.Add(15*time.Minute).Unix())), append(s.v, 1)
	}
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)
	now := time.Date(2026, 11, 3, 18, 0, 0, 0, loc)
	rows := clockRows(s, day, start, now.Add(48*time.Hour), now, loc)
	if len(rows) != 3 {
		t.Fatalf("want rows for the 1st, 2nd (empty) and 3rd, got %d", len(rows))
	}
	if !empty(rows[1].V) || rows[1].Text != "" {
		t.Error("the 2nd has no readings and must be an empty row with no number")
	}
	if rows[0].Text != "15 min" {
		t.Errorf("row text %q, want 15 min (one cell at or above the threshold)", rows[0].Text)
	}
}

func TestClockTicksAndGlyphs(t *testing.T) {
	var labels []string
	for _, o := range sleepClock.ticks() {
		labels = append(labels, clockHour(sleepClock, o))
	}
	if n := len(labels); n < 3 || n > 5 {
		t.Errorf("want 3–5 clock ticks, got %v", labels)
	}
	if strings.Join(labels, " ") != "21:00 00:00 03:00 06:00 09:00" {
		t.Errorf("ticks %v", labels)
	}
	var g []string
	for i := range sleepClock.States {
		g = append(g, string(stateGlyph(i, 4)))
	}
	if strings.Join(g, "") != "█▆▄▂" {
		t.Errorf("state glyphs %v, want tallest first", g)
	}
}

func ptr(v float64) *float64 { return &v }
