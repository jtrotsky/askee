package main

// days: one value per calendar day. Sleep, steps and a night's resting heart rate are daily
// figures; a time-series line draws them as a staircase, and a single night as a lone dot or
// nothing. Here each day is a slot: a column for an amount that starts at zero ("mark": "bar",
// stackable), or a dot for a level whose question is change ("mark": "dot", joined to the day
// before only when that day has a value).
//
// Each expr is evaluated hourly, with windows that cover each clock hour exactly: the query
// starts one second before the hour, so a [1h] rollup at H:59:59 sees H:00:00 to H:59:59 and a
// point stamped at local midnight lands in its own day. The day's hourly values are reduced by
// "day": max (the default: daily figures and running totals), sum (rollups of finer data),
// min, or last. So write exprs as hourly rollups: max(last_over_time(x[1h])) for a figure
// stamped once a day, sum(sum_over_time(x[1h])) for something to add up.
//
// "baseline": N draws the reader's usual range behind the first series: for each day, the
// mean ± one standard deviation of that series over the N days before it, once it has five.
// That is how ring apps judge HRV, resting heart rate and temperature: against yourself.

import (
	"math"
	"time"
)

type band struct {
	Of   int       `json:"of"`   // index of the series it belongs to
	Name string    `json:"name"` // named once in the margin
	T    []float64 `json:"t"`
	Lo   []float64 `json:"lo"`
	Hi   []float64 `json:"hi"`
}

const baselineMin = 5 // days of history before a usual range means anything

func dayOf(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// bucketDays reduces an hourly series to one point per local day, stamped at its midnight.
func bucketDays(s series, how string, loc *time.Location) series {
	out := series{name: s.name}
	var cur int64 = -1
	var acc []float64
	flush := func() {
		if cur < 0 || len(acc) == 0 {
			return
		}
		out.t = append(out.t, float64(cur))
		out.v = append(out.v, reduce(how, acc))
	}
	for i, t := range s.t {
		v := s.v[i]
		if math.IsNaN(v) {
			continue
		}
		d := dayOf(time.Unix(int64(t), 0), loc).Unix()
		if d != cur {
			flush()
			cur, acc = d, acc[:0]
		}
		acc = append(acc, v)
	}
	flush()
	return out
}

func reduce(how string, vs []float64) float64 {
	switch how {
	case "sum":
		x := 0.0
		for _, v := range vs {
			x += v
		}
		return x
	case "min":
		x := vs[0]
		for _, v := range vs {
			x = math.Min(x, v)
		}
		return x
	case "last":
		return vs[len(vs)-1]
	}
	x := vs[0]
	for _, v := range vs {
		x = math.Max(x, v)
	}
	return x
}

// usualRange: for each day of s at or after from, mean ± sd of the values in the n days
// before it (not counting the day itself), once there are baselineMin of them.
func usualRange(s series, n int, from float64, loc *time.Location) band {
	b := band{Name: "usual"}
	for i, t := range s.t {
		if t < from {
			continue
		}
		lo := float64(dayOf(time.Unix(int64(t), 0), loc).AddDate(0, 0, -n).Unix())
		var xs []float64
		for j := 0; j < i; j++ {
			if s.t[j] >= lo {
				xs = append(xs, s.v[j])
			}
		}
		if len(xs) < baselineMin {
			continue
		}
		mean := 0.0
		for _, x := range xs {
			mean += x
		}
		mean /= float64(len(xs))
		sd := 0.0
		for _, x := range xs {
			sd += (x - mean) * (x - mean)
		}
		sd = math.Sqrt(sd / float64(len(xs)-1))
		b.T, b.Lo, b.Hi = append(b.T, t), append(b.Lo, mean-sd), append(b.Hi, mean+sd)
	}
	return b
}

// fetchDays fills m for a days page over the whole local days that end inside [start, end].
func (p page) fetchDays(m *model, start, end time.Time, loc *time.Location) {
	first := dayOf(start, loc).AddDate(0, 0, 1)
	from := first
	if p.Baseline > 0 {
		from = first.AddDate(0, 0, -p.Baseline)
	}
	var qs []q
	for _, s := range p.Series {
		qs = append(qs, q{s.Expr, s.Name, s.Label})
	}
	raw, err := rangeSeries(qs, from.Add(-time.Second), end, time.Hour)
	m.err = err
	m.kind, m.mark, m.step = "days", p.Mark, 24*time.Hour
	m.start, m.end = first, dayOf(end, loc).AddDate(0, 0, 1)
	for i, s := range raw {
		d := bucketDays(s, p.Day, loc)
		if i == 0 && p.Baseline > 0 {
			if b := usualRange(d, p.Baseline, float64(first.Unix()), loc); len(b.T) > 0 {
				m.bands = append(m.bands, b)
			}
		}
		k := 0
		for k < len(d.t) && d.t[k] < float64(first.Unix()) {
			k++
		}
		d.t, d.v = d.t[k:], d.v[k:]
		m.series = append(m.series, d)
	}
	m.refs = p.resolveRefs()
}

// lineBands queries each line series' lo and hi: the range drawn behind it.
func (p page) lineBands(start, end time.Time, step time.Duration) []band {
	var out []band
	for i, s := range p.Series {
		if s.Lo == "" {
			continue
		}
		r, err := rangeSeries([]q{{expr: s.Lo, name: "lo"}, {expr: s.Hi, name: "hi"}}, start, end, step)
		if err != nil || len(r) != 2 {
			continue
		}
		hi := map[float64]float64{}
		for k, t := range r[1].t {
			hi[t] = r[1].v[k]
		}
		b := band{Of: i, Name: "range"}
		for k, t := range r[0].t {
			if h, ok := hi[t]; ok {
				b.T, b.Lo, b.Hi = append(b.T, t), append(b.Lo, r[0].v[k]), append(b.Hi, h)
			}
		}
		if len(b.T) > 0 {
			out = append(out, b)
		}
	}
	return out
}
