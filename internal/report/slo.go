// Package report renders simulation results and evaluates service-level
// objectives against them.
package report

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cb-Sigma/scale-backtest/internal/sim"
)

// Objective is one condition of an --slo expression, such as
// "ttft_p95<=2s" or "attainment>=99%".
type Objective struct {
	Metric string
	Op     string
	Value  float64
	Raw    string
}

// Check is the outcome of one objective on one result.
type Check struct {
	Objective Objective `json:"-"`
	Text      string    `json:"objective"`
	Actual    float64   `json:"actual"`
	OK        bool      `json:"ok"`
}

type metricDef struct {
	get  func(r *sim.Result) (float64, bool)
	kind string // "duration", "share", "number"
	help string
}

func perc(sel func(r *sim.Result) sim.Percentiles, field string) func(r *sim.Result) (float64, bool) {
	return func(r *sim.Result) (float64, bool) {
		p := sel(r)
		switch field {
		case "mean":
			return p.Mean, true
		case "p50":
			return p.P50, true
		case "p90":
			return p.P90, true
		case "p95":
			return p.P95, true
		case "p99":
			return p.P99, true
		}
		return p.Max, true
	}
}

var sloMetrics = func() map[string]metricDef {
	m := map[string]metricDef{
		"attainment": {func(r *sim.Result) (float64, bool) { return r.Attainment, true }, "share",
			"share of requests that completed within every latency target"},
		"ttft_attainment": {func(r *sim.Result) (float64, bool) {
			if r.TTFTAttainment == nil {
				return 0, false
			}
			return *r.TTFTAttainment, true
		}, "share", "share of requests within --ttft-target"},
		"e2e_attainment": {func(r *sim.Result) (float64, bool) {
			if r.E2EAttainment == nil {
				return 0, false
			}
			return *r.E2EAttainment, true
		}, "share", "share of requests within --e2e-target"},
		"drop_rate": {func(r *sim.Result) (float64, bool) {
			return float64(r.Dropped) / float64(max(r.Requests, 1)), true
		}, "share", "share of requests dropped or timed out"},
		"dropped":    {func(r *sim.Result) (float64, bool) { return float64(r.Dropped), true }, "number", "requests dropped or timed out"},
		"cost":       {func(r *sim.Result) (float64, bool) { return r.Cost.Cost, true }, "number", "total cost"},
		"gpu_hours":  {func(r *sim.Result) (float64, bool) { return r.Cost.GPUHours, true }, "number", "billed GPU-hours"},
		"idle_share": {func(r *sim.Result) (float64, bool) { return r.Cost.IdleShare, true }, "share", "share of billed time spent idle"},
		"max_ready":  {func(r *sim.Result) (float64, bool) { return float64(r.Replicas.MaxReady), true }, "number", "highest number of ready replicas"},
		"cold_hits":  {func(r *sim.Result) (float64, bool) { return float64(r.ColdHits), true }, "number", "requests that arrived with no ready replica"},
	}
	for name, sel := range map[string]func(r *sim.Result) sim.Percentiles{
		"ttft":  func(r *sim.Result) sim.Percentiles { return r.TTFT },
		"e2e":   func(r *sim.Result) sim.Percentiles { return r.E2E },
		"queue": func(r *sim.Result) sim.Percentiles { return r.Queue },
	} {
		for _, f := range []string{"mean", "p50", "p90", "p95", "p99", "max"} {
			m[name+"_"+f] = metricDef{perc(sel, f), "duration", name + " " + f + " of completed requests"}
		}
	}
	return m
}()

// SLOMetrics returns the metric names usable in --slo, sorted.
func SLOMetrics() []string {
	names := make([]string, 0, len(sloMetrics))
	for n := range sloMetrics {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ParseSLO parses a comma-separated list of objectives.
func ParseSLO(s string) ([]Objective, error) {
	var out []Objective
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var op string
		var at int
		for _, cand := range []string{"<=", ">=", "<", ">"} {
			if i := strings.Index(part, cand); i > 0 {
				op, at = cand, i
				break
			}
		}
		if op == "" {
			return nil, fmt.Errorf("SLO %q: expected metric<=value or metric>=value", part)
		}
		name := strings.ToLower(strings.TrimSpace(part[:at]))
		def, ok := sloMetrics[name]
		if !ok {
			return nil, fmt.Errorf("SLO %q: unknown metric %q (known: %s)", part, name, strings.Join(SLOMetrics(), ", "))
		}
		val, err := parseSLOValue(strings.TrimSpace(part[at+len(op):]), def.kind)
		if err != nil {
			return nil, fmt.Errorf("SLO %q: %w", part, err)
		}
		out = append(out, Objective{Metric: name, Op: op, Value: val, Raw: part})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty SLO expression")
	}
	return out, nil
}

func parseSLOValue(s, kind string) (float64, error) {
	if pct, ok := strings.CutSuffix(s, "%"); ok {
		v, err := strconv.ParseFloat(strings.TrimSpace(pct), 64)
		if err != nil {
			return 0, fmt.Errorf("bad percentage %q", s)
		}
		return v / 100, nil
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v, nil
	}
	if kind == "duration" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		return d.Seconds(), nil
	}
	return 0, fmt.Errorf("bad value %q", s)
}

// Evaluate checks objectives against a result.
func Evaluate(objs []Objective, r *sim.Result) (checks []Check, pass bool) {
	pass = true
	for _, o := range objs {
		def := sloMetrics[o.Metric]
		actual, ok := def.get(r)
		c := Check{Objective: o, Text: o.Raw, Actual: actual}
		if ok {
			switch o.Op {
			case "<=":
				c.OK = actual <= o.Value
			case ">=":
				c.OK = actual >= o.Value
			case "<":
				c.OK = actual < o.Value
			case ">":
				c.OK = actual > o.Value
			}
		}
		if !c.OK {
			pass = false
		}
		checks = append(checks, c)
	}
	return checks, pass
}

// FormatActual renders the actual value of a check in the unit of its metric.
func (c Check) FormatActual() string {
	switch sloMetrics[c.Objective.Metric].kind {
	case "duration":
		return Dur(c.Actual)
	case "share":
		return Pct(c.Actual)
	}
	return strconv.FormatFloat(c.Actual, 'f', 2, 64)
}
