package main

// Prometheus-compatible HTTP API client (VictoriaMetrics, Prometheus, Thanos…).

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

var vmBase string

// A dashboard must never wait long or pile on: one query may take 10 s at most, and at most
// four run at once, so a refresh of every group cannot swamp a small database that is busy
// with something else (a backup, an image build). A query that fails is not shown as an
// error over good data — the group cache keeps the last good answer (web.go).
var client = &http.Client{Timeout: 10 * time.Second}
var vmSlots = make(chan struct{}, 4)

type vmResult struct {
	Metric map[string]string `json:"metric"`
	Values [][2]any          `json:"values"`
	Value  [2]any            `json:"value"`
}

func vmQuery(path string, p url.Values) ([]vmResult, error) {
	vmSlots <- struct{}{}
	defer func() { <-vmSlots }()
	resp, err := client.Get(vmBase + "/api/v1/" + path + "?" + p.Encode())
	if err != nil {
		return nil, shortErr(err)
	}
	defer resp.Body.Close()
	var d struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []vmResult `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, err
	}
	if d.Status != "success" {
		return nil, fmt.Errorf("query: %s", d.Error)
	}
	return d.Data.Result, nil
}

func pf(x any) float64 {
	switch v := x.(type) {
	case float64:
		return v
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	}
	return math.NaN()
}

type series struct {
	name string
	t, v []float64
}

type q struct {
	expr, name, label string
}

func rangeSeries(qs []q, start, end time.Time, step time.Duration) ([]series, error) {
	var out []series
	for _, qq := range qs {
		p := url.Values{}
		p.Set("query", qq.expr)
		p.Set("start", strconv.FormatInt(start.Unix(), 10))
		p.Set("end", strconv.FormatInt(end.Unix(), 10))
		p.Set("step", strconv.Itoa(int(step.Seconds())))
		res, err := vmQuery("query_range", p)
		if err != nil {
			return nil, err
		}
		sort.Slice(res, func(i, j int) bool { return res[i].Metric[qq.label] < res[j].Metric[qq.label] })
		if len(res) == 0 {
			out = append(out, series{name: qq.name}) // keep the slot: colour follows the entity
		}
		for _, r := range res {
			s := series{name: qq.name}
			if qq.label != "" {
				s.name = r.Metric[qq.label]
				if qq.name != "" {
					s.name = qq.name + " " + s.name
				}
			}
			for _, tv := range r.Values {
				v := pf(tv[1])
				if math.IsNaN(v) || math.IsInf(v, 0) {
					continue
				}
				s.t = append(s.t, pf(tv[0]))
				s.v = append(s.v, v)
			}
			out = append(out, s)
		}
	}
	return out, nil
}

type ires struct {
	metric map[string]string
	value  float64
}

func instant(expr string) ([]ires, error) {
	p := url.Values{}
	p.Set("query", expr)
	res, err := vmQuery("query", p)
	if err != nil {
		return nil, err
	}
	var out []ires
	for _, r := range res {
		v := pf(r.Value[1])
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			out = append(out, ires{r.Metric, v})
		}
	}
	return out, nil
}

// shortErr turns a transport error into words a reader can use; the URL (with its query) is
// noise on a chart and is dropped.
func shortErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return fmt.Errorf("the database did not answer within %s", client.Timeout)
		}
		return fmt.Errorf("the database is unreachable (%v)", ue.Err)
	}
	return err
}
