package replica

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cb-Sigma/scale-backtest/internal/trace"
)

const examples = "../../examples"

func TestExampleModels(t *testing.T) {
	m, err := Load(filepath.Join(examples, "replicas/demo-gpu.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "demo-gpu" || m.MaxConcurrency != 24 || m.MaxQueue != nil || m.Cost.GPUsPerReplica != 1 || m.TerminationGrace() != 600 {
		t.Fatalf("%+v", m)
	}
	// tpot(n) = 14 ms + 0.9 ms x n.
	if got := m.SecondsPerToken(1); math.Abs(got-0.0149) > 1e-12 {
		t.Errorf("tpot(1) = %v", got)
	}
	if got := m.Throughput(24); math.Abs(got-24/0.0356) > 1e-9 {
		t.Errorf("throughput(24) = %v", got)
	}
	if got := m.PeakThroughput(); got != m.Throughput(24) {
		t.Errorf("peak throughput %v", got)
	}
	if got := m.PrefillSeconds(600); math.Abs(got-0.13) > 1e-12 {
		t.Errorf("prefill(600) = %v", got)
	}
	if got := m.PrefillSlowdown(24); math.Abs(got-1.92) > 1e-12 {
		t.Errorf("prefill slowdown(24) = %v", got)
	}
	if m.Throughput(0) != 0 || m.DecodeRate(0) != m.DecodeRate(1) {
		t.Error("edge cases of the curve")
	}

	c, err := Load(filepath.Join(examples, "replicas/cold-node-pool.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if *c.MaxQueue != 200 || c.Node.KeepWarm != 600 || c.TerminationGrace() != 300 {
		t.Fatalf("%+v", c)
	}
	// Measured points: exact at a point, interpolated between, extended beyond.
	if got := c.SecondsPerToken(8); got != 0.0212 {
		t.Errorf("curve at 8 = %v", got)
	}
	if got := c.SecondsPerToken(12); math.Abs(got-0.0248) > 1e-12 {
		t.Errorf("curve at 12 = %v", got)
	}
	if got := c.SecondsPerToken(32); math.Abs(got-(0.0356+8*0.0009)) > 1e-12 {
		t.Errorf("curve at 32 = %v", got)
	}
}

func TestModelValidation(t *testing.T) {
	good := "maxConcurrency: 4\ndecode: {baseSecondsPerToken: 0.01}\n"
	if _, err := Parse([]byte(good), ""); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"decode: {baseSecondsPerToken: 0.01}\n":                   "maxConcurrency",
		"maxConcurrency: 4\n":                                     "time per output token",
		good + "maxQueue: -1\n":                                   "maxQueue",
		good + "bogus: 1\n":                                       "bogus",
		good + "kind: Deployment\n":                               "kind",
		good + "coldStart: {constant: 10s, samples: [1, 2]}\n":    "exactly one",
		good + "coldStart: {percentiles: {p50: 20s, p90: 10s}}\n": "must not decrease",
		good + "coldStart: {percentiles: {median: 20s}}\n":        "percentile key",
		good + "coldStart: {constant: soon}\n":                    "duration",
		good + "prefill: {interference: -1}\n":                    "prefill",
		good + "cost: {gpuPricePerHour: -1}\n":                    "cost",
		"maxConcurrency: 4\ndecode: {baseSecondsPerToken: 0.01, curve: [{concurrency: 1, secondsPerToken: 0.01}]}\n": "not both",
		"maxConcurrency: 4\ndecode: {curve: [{concurrency: 0, secondsPerToken: 0.01}]}\n":                            "at least 1",
		good + "coldStart: {doriki: [missing.json]}\n":                                                               "missing.json",
	}
	for in, want := range cases {
		_, err := Parse([]byte(in), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want an error mentioning %q", in, err, want)
		}
	}
}

func TestDurationForms(t *testing.T) {
	m, err := Parse([]byte("maxConcurrency: 1\ndecode: {baseSecondsPerToken: 0.01}\ncoldStart: {constant: 90}\nterminationGracePeriod: 2m30s\nnode: {keepWarm: 10m}\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if m.ColdStart.Mean() != 90 || m.TerminationGrace() != 150 || m.Node.KeepWarm.Seconds() != 600 {
		t.Fatalf("%v %v %v", m.ColdStart.Mean(), m.TerminationGrace(), m.Node.KeepWarm)
	}
	out, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(out, "")
	if err != nil {
		t.Fatalf("a marshalled model must parse again: %v\n%s", err, out)
	}
	if back.ColdStart.Mean() != 90 || back.TerminationGrace() != 150 || back.Kind != Kind {
		t.Fatalf("round trip: %s", out)
	}
}

func TestPercentileDistribution(t *testing.T) {
	m, err := Parse([]byte("maxConcurrency: 1\ndecode: {baseSecondsPerToken: 0.01}\ncoldStart: {percentiles: {p50: 95s, p90: 130s, p99: 180s}}\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	rng := trace.NewRand(1)
	n := 200000
	vals := make([]float64, n)
	sum := 0.0
	for i := range vals {
		vals[i] = m.ColdStart.Sample(rng)
		sum += vals[i]
	}
	sort.Float64s(vals)
	for q, want := range map[float64]float64{0.5: 95, 0.9: 130, 0.99: 180} {
		if got := trace.Quantile(vals, q); math.Abs(got-want) > 1.5 {
			t.Errorf("sampled p%.0f = %.1f, want %.0f", q*100, got, want)
		}
	}
	// Documented tails: the minimum mirrors the next segment (95 - 35 = 60)
	// and the highest given percentile is the maximum.
	if vals[0] < 60 || vals[n-1] > 180 {
		t.Errorf("range [%v, %v], want within [60, 180]", vals[0], vals[n-1])
	}
	if math.Abs(sum/float64(n)-m.ColdStart.Mean()) > 0.3 {
		t.Errorf("sample mean %v, Mean() %v", sum/float64(n), m.ColdStart.Mean())
	}
	if !strings.Contains(m.ColdStart.Describe(), "p50=95s p90=130s p99=180s") {
		t.Errorf("describe: %s", m.ColdStart.Describe())
	}
}

func TestSamplesDistribution(t *testing.T) {
	d := Dist{Samples: []Duration{10, 20, 30}}
	if err := d.resolve("x", ""); err != nil {
		t.Fatal(err)
	}
	rng := trace.NewRand(2)
	seen := map[float64]int{}
	for range 3000 {
		seen[d.Sample(rng)]++
	}
	if len(seen) != 3 || seen[10] < 800 || seen[20] < 800 || seen[30] < 800 || d.Mean() != 20 {
		t.Fatalf("%v", seen)
	}
}

// The field names are those of doriki's schema/report.v1.json
// (github.com/AmitwalaH/doriki): report_version, phases[].name/seconds/state
// and totals.pending_to_ready_seconds.
func TestDorikiImport(t *testing.T) {
	warm, err := LoadDorikiReport(filepath.Join(examples, "coldstart/doriki-warm-node.json"))
	if err != nil {
		t.Fatal(err)
	}
	if warm.PendingToReadySeconds != 98.5 || warm.NodeProvisionSeconds != 0 || warm.StartupSeconds != 98.5 {
		t.Fatalf("%+v", warm)
	}
	// Absent and skipped phases are left out.
	for _, p := range warm.Phases {
		if p.Name == "compile" || p.Name == "first-token" || p.Name == "node-provision" {
			t.Errorf("phase %s must not be listed", p.Name)
		}
	}
	cold, err := LoadDorikiReport(filepath.Join(examples, "coldstart/doriki-new-node.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Node provisioning is modelled separately and taken out of the cold start.
	if cold.NodeProvisionSeconds != 180 || cold.StartupSeconds != 106 {
		t.Fatalf("%+v", cold)
	}
	m, err := Parse([]byte("maxConcurrency: 1\ndecode: {baseSecondsPerToken: 0.01}\ncoldStart: {doriki: [coldstart/doriki-warm-node.json, coldstart/doriki-new-node.json]}\n"), examples)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.ColdStart.Mean(); math.Abs(got-102.25) > 1e-9 {
		t.Fatalf("cold start mean from doriki reports = %v", got)
	}
	for in, want := range map[string]string{
		`{"report_version": 2, "totals": {"pending_to_ready_seconds": 5}}`: "report_version 2",
		`{"report_version": 1, "totals": {}}`:                              "not Ready",
		`[]`:                                                               "not a doriki",
		`{"report_version": 1, "totals": {"pending_to_ready_seconds": 5}, "phases": [{"name": "node-provision", "state": "complete", "seconds": 9}]}`: "inconsistent",
	} {
		if _, err := ParseDorikiReport([]byte(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", in, err)
		}
	}
}

// Calibration must recover the parameters of a model that generated the
// benchmark exactly, and report a perfect fit.
func TestCalibrateRecoversKnownModel(t *testing.T) {
	truth := &Model{MaxConcurrency: 32,
		Prefill: Prefill{BaseSeconds: 0.03, SecondsPerToken: 0.0002, Interference: 0.05},
		Decode:  Decode{BaseSecondsPerToken: 0.012, SecondsPerTokenPerRequest: 0.0007}}
	if err := truth.Resolve(""); err != nil {
		t.Fatal(err)
	}
	var pts []BenchPoint
	for _, n := range []int{1, 2, 4, 8, 16, 32} {
		pts = append(pts, BenchPoint{
			Concurrency: float64(n), TPOT: truth.SecondsPerToken(n),
			TTFT: truth.PrefillSeconds(800) * truth.PrefillSlowdown(n), Throughput: truth.Throughput(n), PromptTokens: 800,
		})
	}
	fit, err := Calibrate(pts, FitOptions{PrefillBaseSeconds: 0.03, Name: "fitted"})
	if err != nil {
		t.Fatal(err)
	}
	m := fit.Model
	approx := func(name string, got, want float64) {
		if math.Abs(got-want) > 1e-5*want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	approx("decode a", m.Decode.BaseSecondsPerToken, 0.012)
	approx("decode b", m.Decode.SecondsPerTokenPerRequest, 0.0007)
	approx("prefill per token", m.Prefill.SecondsPerToken, 0.0002)
	approx("prefill interference", m.Prefill.Interference, 0.05)
	if m.MaxConcurrency != 32 || m.Name != "fitted" {
		t.Errorf("%+v", m)
	}
	for name, q := range map[string]FitQuality{"TPOT": fit.TPOT, "TTFT": fit.TTFT, "throughput": fit.Throughput} {
		if q.N != 6 || q.R2 < 0.999999 || q.MaxAPE > 1e-4 {
			t.Errorf("%s fit of exact data: %+v", name, q)
		}
	}
}

func TestCalibrateExampleCSV(t *testing.T) {
	pts, format, err := LoadBenchmark(filepath.Join(examples, "benchmarks/concurrency-sweep.csv"), "auto")
	if err != nil || format != BenchCSV || len(pts) != 8 {
		t.Fatalf("%v %s %d", err, format, len(pts))
	}
	if pts[3].Concurrency != 8 || pts[3].TTFT != 0.165 || math.Abs(pts[3].TPOT-0.0214) > 1e-12 || pts[3].Throughput != 364 || pts[3].PromptTokens != 600 {
		t.Fatalf("%+v", pts[3])
	}
	base, err := Load(filepath.Join(examples, "replicas/demo-gpu.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	fit, err := Calibrate(pts, FitOptions{Base: base, PrefillBaseSeconds: 0.04})
	if err != nil {
		t.Fatal(err)
	}
	// The sample was generated around tpot(n) = 14 ms + 0.9 ms x n with noise.
	if a, b := fit.Model.Decode.BaseSecondsPerToken, fit.Model.Decode.SecondsPerTokenPerRequest; math.Abs(a-0.014) > 0.0005 || math.Abs(b-0.0009) > 0.00005 {
		t.Errorf("fitted a=%v b=%v", a, b)
	}
	if fit.TPOT.R2 < 0.99 || fit.TPOT.R2 >= 1 || fit.TPOT.MAPE <= 0 || fit.TPOT.MaxAPE > 0.03 {
		t.Errorf("noisy data must fit well but not perfectly: %+v", fit.TPOT)
	}
	// Everything the benchmark cannot say comes from the base model.
	if fit.Model.MaxConcurrency != 24 || fit.Model.Cost.GPUPricePerHour != 2.5 || fit.Model.ColdStart.Mean() != base.ColdStart.Mean() {
		t.Errorf("base model fields lost: %+v", fit.Model)
	}
}

// The GuideLLM field paths (benchmarks[].metrics.<name>.successful.{mean,median})
// were taken from GuideLLM's schema sources (GenerativeBenchmarksReport,
// GenerativeMetrics, StatusDistributionSummary, DistributionSummary).
func TestCalibrateGuideLLM(t *testing.T) {
	pts, format, err := LoadBenchmark(filepath.Join(examples, "benchmarks/guidellm-benchmarks.json"), "auto")
	if err != nil || format != BenchGuideLLM || len(pts) != 8 {
		t.Fatalf("%v %s %d", err, format, len(pts))
	}
	p := pts[3]
	if math.Abs(p.Concurrency-7.92) > 1e-9 || p.TTFT != 0.165 || math.Abs(p.TPOT-0.0214) > 1e-12 || p.Throughput != 364 || p.PromptTokens != 600 || p.OutputTokens != 400 {
		t.Fatalf("%+v", p)
	}
	fit, err := Calibrate(pts, FitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if fit.Model.MaxConcurrency != 24 || fit.TPOT.R2 < 0.99 {
		t.Fatalf("%+v %+v", fit.Model, fit.TPOT)
	}
	// Falls back to time_per_output_token_ms when there is no inter-token latency.
	alt := `{"benchmarks":[{"metrics":{"request_concurrency":{"successful":{"mean":2}},"time_per_output_token_ms":{"successful":{"mean":20}}}}]}`
	pts, err = ParseGuideLLM([]byte(alt))
	if err != nil || pts[0].TPOT != 0.02 {
		t.Fatalf("%v %+v", err, pts)
	}
	for in, want := range map[string]string{
		`{"benchmarks":[]}`:               "no benchmarks",
		`{"benchmarks":[{"metrics":{}}]}`: "request_concurrency",
		`nope`:                            "not a GuideLLM",
	} {
		if _, err := ParseGuideLLM([]byte(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", in, err)
		}
	}
}

func TestCalibrateEdgeCases(t *testing.T) {
	// Throughput only: TPOT is derived and the note says so.
	pts := []BenchPoint{{Concurrency: 1, Throughput: 50}, {Concurrency: 4, Throughput: 160}, {Concurrency: 8, Throughput: 250}}
	fit, err := Calibrate(pts, FitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(fit.Notes, "\n"), "derived from throughput") || fit.TTFT.N != 0 {
		t.Errorf("notes: %v", fit.Notes)
	}
	// A decreasing TPOT cannot be fitted with a negative slope.
	fit, err = Calibrate([]BenchPoint{{Concurrency: 1, TPOT: 0.03}, {Concurrency: 2, TPOT: 0.02}, {Concurrency: 4, TPOT: 0.01}}, FitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if fit.Model.Decode.SecondsPerTokenPerRequest != 0 || fit.Model.Decode.BaseSecondsPerToken != 0.02 {
		t.Errorf("non-negative refit: %+v", fit.Model.Decode)
	}
	if _, err := Calibrate([]BenchPoint{{Concurrency: 1, TPOT: 0.01}}, FitOptions{}); err == nil {
		t.Error("one point must fail")
	}
	if _, err := Calibrate([]BenchPoint{{Concurrency: 1, TPOT: 0.01, TTFT: 0.1}, {Concurrency: 2, TPOT: 0.02, TTFT: 0.2}}, FitOptions{}); err == nil || !strings.Contains(err.Error(), "--prompt-tokens") {
		t.Errorf("TTFT without a prompt length: %v", err)
	}
	if _, err := Calibrate([]BenchPoint{{Concurrency: 1}, {Concurrency: 2}}, FitOptions{}); err == nil {
		t.Error("no TPOT and no throughput must fail")
	}
}

func TestBenchCSVErrors(t *testing.T) {
	for in, want := range map[string]string{
		"ttft_ms,tpot_ms\n1,2\n":        "no concurrency column",
		"concurrency,ttft\n1,2\n":       "no unit",
		"concurrency,tpot_ms\n0,2\n":    "at least 1",
		"concurrency,tpot_ms\n1,fast\n": "bad value",
	} {
		if _, err := ParseBenchCSV([]byte(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v", in, err)
		}
	}
	pts, err := ParseBenchCSV([]byte("Concurrency, ITL_s, TTFT_s\n2, 0.02, 0.5\n"))
	if err != nil || pts[0].TPOT != 0.02 || pts[0].TTFT != 0.5 {
		t.Fatalf("%v %+v", err, pts)
	}
	if _, _, err := LoadBenchmark(filepath.Join(os.TempDir(), "does-not-exist.csv"), "auto"); err == nil {
		t.Error("missing file must fail")
	}
	if _, _, err := LoadBenchmark(filepath.Join(examples, "benchmarks/concurrency-sweep.csv"), "xml"); err == nil {
		t.Error("unknown format must fail")
	}
}
