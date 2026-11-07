package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestBucketDaysMidnightAndDST(t *testing.T) {
	loc, _ := time.LoadLocation("Pacific/Auckland")
	// NZ daylight saving began 2026-09-27 02:00: that day has 23 hours
	d0 := time.Date(2026, 9, 26, 0, 0, 0, 0, loc)
	var s series
	// hourly samples as fetchDays asks for them: one second before each hour boundary
	for h := 1; h <= 72; h++ {
		ts := d0.Add(time.Duration(h)*time.Hour - time.Second)
		s.t = append(s.t, float64(ts.Unix()))
		s.v = append(s.v, 1)
	}
	got := bucketDays(s, "sum", loc)
	want := []float64{24, 23, 24}
	if len(got.v) < 3 {
		t.Fatalf("days = %v", got.v)
	}
	for i, w := range want {
		if got.v[i] != w {
			t.Errorf("day %d: sum %v, want %v (hours in that local day)", i, got.v[i], w)
		}
		if day := time.Unix(int64(got.t[i]), 0).In(loc); day.Hour() != 0 || day.Day() != 26+i {
			t.Errorf("day %d stamped %v, want local midnight of the %dth", i, day, 26+i)
		}
	}
	// a figure stamped at local midnight, seen by the window ending at 00:59:59, is that day's
	m := series{t: []float64{float64(d0.Add(time.Hour - time.Second).Unix())}, v: []float64{51}}
	if g := bucketDays(m, "max", loc); len(g.t) != 1 || g.t[0] != float64(d0.Unix()) {
		t.Errorf("midnight figure went to %v, want %v", g.t, d0.Unix())
	}
}

func TestUsualRange(t *testing.T) {
	loc, _ := time.LoadLocation("Pacific/Auckland")
	d0 := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)
	var s series
	for i, v := range []float64{50, 52, 54, 56, 58, 70} {
		s.t = append(s.t, float64(d0.AddDate(0, 0, i).Unix()))
		s.v = append(s.v, v)
	}
	b := usualRange(s, 30, s.t[0], loc)
	if len(b.T) != 1 || b.T[0] != s.t[5] {
		t.Fatalf("band days %v, want only the sixth (five before it)", b.T)
	}
	sd := math.Sqrt(10) // sample sd of 50..58 in steps of 2
	if math.Abs(b.Lo[0]-(54-sd)) > 1e-9 || math.Abs(b.Hi[0]-(54+sd)) > 1e-9 {
		t.Errorf("band %v–%v, want 54 ± %.3f (the day itself is not in its own baseline)", b.Lo[0], b.Hi[0], sd)
	}
	if b := usualRange(s, 2, s.t[0], loc); len(b.T) != 0 {
		t.Errorf("a 2-day window never holds five days, got %v", b.T)
	}
}

func TestDaysConfigChecks(t *testing.T) {
	base := `{"vm":"http://x","groups":[{"id":"a","title":"A"}],"pages":[%s]}`
	for _, c := range []struct{ page, err string }{
		{`{"id":"p","group":"a","kind":"days","title":"T"}`, "needs series"},
		{`{"id":"p","group":"a","kind":"days","title":"T","mark":"pie","series":[{"expr":"x"}]}`, "mark is bar"},
		{`{"id":"p","group":"a","kind":"days","title":"T","day":"mean","series":[{"expr":"x"}]}`, "day is max"},
		{`{"id":"p","group":"a","kind":"days","title":"T","mark":"dot","stack":true,"series":[{"expr":"x"}]}`, "only bars stack"},
		{`{"id":"p","group":"a","kind":"line","title":"T","mark":"dot","series":[{"expr":"x"}]}`, "belong to days pages"},
		{`{"id":"p","group":"a","kind":"line","title":"T","series":[{"expr":"x","lo":"y"}]}`, "as a pair"},
		{`{"id":"p","group":"a","kind":"days","title":"T","series":[{"expr":"x","lo":"y","hi":"z"}]}`, "on line pages"},
	} {
		_, err := parseConfig([]byte(strings.Replace(base, "%s", c.page, 1)))
		if err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: err %v, want %q", c.page, err, c.err)
		}
	}
	ok := `{"id":"p","group":"a","kind":"days","title":"T","mark":"bar","stack":true,"day":"sum","baseline":30,"min_range":"7d","series":[{"expr":"x"}]}`
	if _, err := parseConfig([]byte(strings.Replace(base, "%s", ok, 1))); err != nil {
		t.Errorf("valid days page refused: %v", err)
	}
}

func TestStepPlaceholder(t *testing.T) {
	if got := withWindow("min_over_time(x[$__step])", 30*time.Minute); got != "min_over_time(x[1800s])" {
		t.Errorf("got %q", got)
	}
}

func parseConfig(b []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return finishConfig(&c)
}
