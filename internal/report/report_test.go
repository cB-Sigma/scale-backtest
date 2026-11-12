package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cb-Sigma/scale-backtest/internal/sim"
)

func TestDur(t *testing.T) {
	for in, want := range map[float64]string{
		0: "0s", 0.2134: "0.213s", 0.9996: "1.00s", 4.519: "4.52s", 37.14: "37.1s", 99.96: "1m40s",
		125: "2m05s", 3599.6: "1h00m", 3720: "1h02m", -2: "-2.00s",
	} {
		if got := Dur(in); got != want {
			t.Errorf("Dur(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestPctNumMoneyClock(t *testing.T) {
	for in, want := range map[float64]string{1: "100%", 0.99996: "99.99%", 0.9997: "99.97%", 0.912: "91.2%", 0.0799: "7.99%", 0: "0.00%"} {
		if got := Pct(in); got != want {
			t.Errorf("Pct(%v) = %q, want %q", in, got, want)
		}
	}
	if Num(1234567) != "1,234,567" || Num(999) != "999" || Num(-1200) != "-1,200" {
		t.Error("Num")
	}
	if Money(8.5) != "$8.50" || Money(12345.6) != "$12,346" {
		t.Error("Money")
	}
	if Clock(3725.9) != "01:02:05" {
		t.Error(Clock(3725.9))
	}
}

func TestSparkline(t *testing.T) {
	line, top := Sparkline([]float64{0, 1, 4, 8})
	if line != "▁▂▅█" || top != 8 {
		t.Errorf("%q %v", line, top)
	}
	if line, _ := Sparkline([]float64{0, 0}); line != "▁▁" {
		t.Errorf("%q", line)
	}
	got := bucketise([]float64{0, 10, 20, 90}, []float64{1, 5, 3, 2}, 100, 4, "max")
	if got[0] != 5 || got[1] != 5 || got[2] != 5 || got[3] != 2 {
		t.Errorf("bucketise max with carry: %v", got)
	}
}

func result(cost, attainment float64) *sim.Result {
	a := attainment
	return &sim.Result{Policy: "p", Requests: 1000, Completed: 990, Dropped: 10, DropReasons: map[string]int{"queue_timeout": 10},
		Attainment: attainment, TTFTAttainment: &a, TTFTTargetSeconds: 2,
		TTFT: sim.Percentiles{P50: 0.2, P95: 1.5, P99: 4, Max: 9, Mean: 0.5},
		E2E:  sim.Percentiles{P95: 30},
		Cost: sim.CostStats{Cost: cost, GPUHours: cost / 2.5, IdleShare: 0.2}}
}

func TestSLO(t *testing.T) {
	objs, err := ParseSLO("attainment>=99%, ttft_p95<=2s ,drop_rate<=0.5%,cost<20,ttft_attainment>0.9,e2e_p95<=500ms")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 6 || objs[0].Value != 0.99 || objs[1].Value != 2 || objs[2].Value != 0.005 || objs[5].Value != 0.5 {
		t.Fatalf("%+v", objs)
	}
	checks, pass := Evaluate(objs, result(10, 0.95))
	want := []bool{false, true, false, true, true, false}
	for i, c := range checks {
		if c.OK != want[i] {
			t.Errorf("%s: ok=%v, want %v (actual %s)", c.Text, c.OK, want[i], c.FormatActual())
		}
	}
	if pass {
		t.Error("the SLO must fail")
	}
	if checks[0].FormatActual() != "95.0%" || checks[1].FormatActual() != "1.50s" || checks[3].FormatActual() != "10.00" {
		t.Errorf("formatting: %s %s %s", checks[0].FormatActual(), checks[1].FormatActual(), checks[3].FormatActual())
	}
	if _, pass := Evaluate(objs[1:2], result(10, 0.95)); !pass {
		t.Error("a met objective must pass")
	}
	// An attainment objective without the corresponding target cannot pass.
	objs, _ = ParseSLO("e2e_attainment>=0.5")
	if _, pass := Evaluate(objs, result(1, 1)); pass {
		t.Error("e2e_attainment without --e2e-target must fail")
	}
	for _, bad := range []string{"", "attainment", "latency<=2s", "ttft_p95<=fast", "attainment>=ninety%", "cost<=2s"} {
		if _, err := ParseSLO(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
	if len(SLOMetrics()) < 20 {
		t.Error("metric list")
	}
}

func TestFrontier(t *testing.T) {
	runs := []*Run{
		{Result: result(10, 0.90)}, // frontier: cheapest
		{Result: result(12, 0.99)}, // frontier
		{Result: result(13, 0.95)}, // dominated by the second
		{Result: result(20, 1.00)}, // frontier: best
		{Result: result(20, 0.99)}, // dominated
	}
	MarkFrontier(runs)
	want := []bool{true, true, false, true, false}
	for i, r := range runs {
		if r.Frontier != want[i] {
			t.Errorf("run %d: frontier=%v, want %v", i, r.Frontier, want[i])
		}
	}
}

func TestRenderFormats(t *testing.T) {
	res := result(10, 0.95)
	res.Events = []sim.ScaleEvent{{T: 75, From: 1, To: 3, Source: "HPA", Reason: "pods metric q above target", Detail: "q=9/5 -> 3; "}}
	res.Timeline = []sim.TimelinePoint{{T: 0, Ready: 1}, {T: 60, Ready: 3, ArrivalRate: 2}}
	res.DurationSeconds = 120
	res.Approximate = true
	res.Warnings = []string{"something ignored"}
	objs, _ := ParseSLO("attainment>=99%")
	run := NewRun(res, objs)

	var tbl, md, js bytes.Buffer
	for format, buf := range map[string]*bytes.Buffer{FormatTable: &tbl, FormatMarkdown: &md, FormatJSON: &js} {
		if err := WriteRun(buf, run, format, Options{Version: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"Verdict   FAIL", "APPROXIMATE TRACE", "00:01:15  1 -> 3  HPA  pods metric q above target", "q=9/5 -> 3", "queue_timeout 10", "warning: something ignored"} {
		if !strings.Contains(tbl.String(), want) {
			t.Errorf("table output lacks %q:\n%s", want, tbl.String())
		}
	}
	for _, want := range []string{"**Verdict: FAIL**", "| TTFT |", "Approximate trace", "| 00:01:15 | 1 -> 3 | HPA |"} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("markdown output lacks %q:\n%s", want, md.String())
		}
	}
	var doc struct {
		Version string
		Runs    []struct {
			Result  map[string]any `json:"result"`
			SLOPass *bool          `json:"slo_pass"`
		}
	}
	if err := json.Unmarshal(js.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != "test" || len(doc.Runs) != 1 || doc.Runs[0].SLOPass == nil || *doc.Runs[0].SLOPass || doc.Runs[0].Result["attainment"].(float64) != 0.95 {
		t.Fatalf("json: %s", js.String())
	}

	var cmp, sweep, csv bytes.Buffer
	other := NewRun(result(20, 1), objs)
	other.Result.Timeline, other.Result.DurationSeconds = res.Timeline, 120
	run.Params, other.Params = map[string]float64{"target": 5}, map[string]float64{"target": 2}
	if err := WriteCompare(&cmp, []*Run{run, other}, FormatTable, Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmp.String(), "ready replicas over time") || !strings.Contains(cmp.String(), "PASS") {
		t.Errorf("compare:\n%s", cmp.String())
	}
	if err := WriteSweep(&sweep, []*Run{other, run}, FormatTable, Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sweep.String(), "cheapest setting that meets the SLO: target=2 ($20.00") {
		t.Errorf("sweep:\n%s", sweep.String())
	}
	if lines := strings.Split(sweep.String(), "\n"); !strings.HasPrefix(strings.TrimSpace(lines[3]), "5 ") {
		t.Errorf("a sweep lists the cheapest setting first:\n%s", sweep.String())
	}
	for _, write := range []func() error{
		func() error { return WriteTimelineCSV(&csv, res) },
		func() error { return WriteEventsCSV(&csv, res) },
		func() error { return WriteRequestsCSV(&csv, res) },
	} {
		if err := write(); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"t_seconds,desired,pending", "60.000,0,0,0,3,", "75.000,1,3,HPA,pods metric q above target", "arrival_seconds,queue_seconds"} {
		if !strings.Contains(csv.String(), want) {
			t.Errorf("csv lacks %q:\n%s", want, csv.String())
		}
	}
	if !ValidFormat("json") || ValidFormat("xml") {
		t.Error("ValidFormat")
	}
}
