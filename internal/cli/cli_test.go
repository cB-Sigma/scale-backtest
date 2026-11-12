package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const ex = "../../examples/"

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Main(args, &out, &errb)
	return code, out.String(), errb.String()
}

// golden runs a command and compares its output with testdata/<name>.golden.
// Regenerate with: go test ./internal/cli -update
func golden(t *testing.T, name string, wantCode int, args ...string) string {
	t.Helper()
	code, out, errOut := run(t, args...)
	if code != wantCode {
		t.Fatalf("%v: exit code %d, want %d\nstderr: %s", args, code, wantCode, errOut)
	}
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
		return out
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run `go test ./internal/cli -update` to create it)", err)
	}
	if out != string(want) {
		t.Errorf("%v: output differs from %s (run `go test ./internal/cli -update` and review the diff)\n--- got ---\n%s", args, path, out)
	}
	return out
}

func TestGoldenRunHPA(t *testing.T) {
	out := golden(t, "run-hpa-queue-depth", ExitOK,
		"run", "--policy", ex+"policies/hpa-queue-depth.yaml", "--trace", ex+"traces/demo.csv", "--replica", ex+"replicas/demo-gpu.yaml",
		"--ttft-target", "2s", "--e2e-target", "60s")
	// --demo is the same run from the embedded copies of the same files.
	_, demo, _ := run(t, "run", "--demo")
	if demo != out {
		t.Error("`run --demo` must equal the explicit run on the example files")
	}
}

func TestGoldenRunKEDAWithSLO(t *testing.T) {
	golden(t, "run-keda-scale-to-zero", ExitSLOFail,
		"run", "--policy", ex+"policies/keda-scale-to-zero.yaml", "--trace", ex+"traces/demo.csv", "--replica", ex+"replicas/demo-gpu.yaml",
		"--ttft-target", "2s", "--e2e-target", "60s", "--slo", "attainment>=99%,ttft_p95<=2s,drop_rate<=0.1%", "--max-events", "8")
}

func TestGoldenRunMarkdownAndNodePool(t *testing.T) {
	golden(t, "run-node-pool-markdown", ExitOK,
		"run", "--policy", ex+"policies/hpa-token-throughput.yaml", "--trace", ex+"traces/demo.csv", "--replica", ex+"replicas/cold-node-pool.yaml",
		"--ttft-target", "2s", "--queue-timeout", "45s", "--lb", "round-robin", "--format", "markdown", "--max-events", "5")
}

func TestGoldenCompare(t *testing.T) {
	golden(t, "compare-demo", ExitOK, "compare", "--demo")
	golden(t, "compare-demo-markdown", ExitSLOFail, "compare", "--demo", "--format", "markdown", "--slo", "attainment>=99%")
}

func TestGoldenSweep(t *testing.T) {
	golden(t, "sweep-demo", ExitOK, "sweep", "--demo", "--slo", "attainment>=99%")
	golden(t, "sweep-keda-cooldown", ExitOK, "sweep", "--demo", "--policy", "demo:policies/keda-scale-to-zero.yaml",
		"--param", "cooldownPeriod=60,300,900", "--param", "target=8,12")
}

func TestGoldenExplainAndMetrics(t *testing.T) {
	golden(t, "explain-keda", ExitOK, "explain", "--policy", ex+"policies/keda-prometheus.yaml")
	golden(t, "explain-hpa", ExitOK, "explain", ex+"policies/hpa-token-throughput.yaml")
	golden(t, "metrics", ExitOK, "metrics")
}

func TestGoldenTraceStats(t *testing.T) {
	golden(t, "trace-stats-demo", ExitOK, "trace", "stats", ex+"traces/demo.csv")
}

func TestGoldenCalibrate(t *testing.T) {
	golden(t, "calibrate", ExitOK, "calibrate", "--bench", ex+"benchmarks/concurrency-sweep.csv", "--base", ex+"replicas/demo-gpu.yaml",
		"--prefill-base", "40ms", "--doriki", ex+"coldstart/doriki-warm-node.json", "--doriki", ex+"coldstart/doriki-new-node.json")
}

func TestJSONOutput(t *testing.T) {
	code, out, _ := run(t, "run", "--demo", "--format", "json", "--slo", "attainment>=50%")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	var doc struct {
		Runs []struct {
			Result struct {
				Requests   int     `json:"requests"`
				Attainment float64 `json:"attainment"`
				Events     []any   `json:"scale_events"`
				Timeline   []any   `json:"timeline"`
				Cost       struct {
					GPUHours float64 `json:"gpu_hours"`
				} `json:"cost"`
			} `json:"result"`
			SLOPass *bool `json:"slo_pass"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	r := doc.Runs[0]
	if r.Result.Requests != 4797 || len(r.Result.Events) == 0 || len(r.Result.Timeline) < 100 || r.Result.Cost.GPUHours <= 0 || r.SLOPass == nil || !*r.SLOPass {
		t.Fatalf("unexpected JSON: %.300s", out)
	}
	// Same seed, same bytes.
	_, again, _ := run(t, "run", "--demo", "--format", "json", "--slo", "attainment>=50%")
	if again != out {
		t.Fatal("two runs with the same seed differ")
	}
	_, other, _ := run(t, "run", "--demo", "--format", "json", "--slo", "attainment>=50%", "--seed", "2")
	if other == out {
		t.Fatal("a different seed should change the cold-start draws and the result")
	}
}

func TestSLOExitCodes(t *testing.T) {
	base := []string{"run", "--demo", "--policy", "static:6"}
	if code, _, _ := run(t, append(base, "--slo", "attainment>=99%,dropped<=0")...); code != ExitOK {
		t.Errorf("a met SLO must exit 0, got %d", code)
	}
	if code, out, _ := run(t, append(base, "--slo", "cost<=1")...); code != ExitSLOFail || !strings.Contains(out, "Verdict   FAIL") {
		t.Errorf("a missed SLO must exit 2, got %d", code)
	}
	if code, _, errOut := run(t, append(base, "--slo", "speed>=9000")...); code != ExitError || !strings.Contains(errOut, "unknown metric") {
		t.Errorf("a bad SLO must exit 1, got %d: %s", code, errOut)
	}
	// A sweep passes when some setting meets the SLO.
	if code, _, _ := run(t, "sweep", "--demo", "--policy", "static:1", "--param", "replicas=1,6", "--slo", "attainment>=99%"); code != ExitOK {
		t.Errorf("sweep with one passing setting: exit %d", code)
	}
	if code, _, _ := run(t, "sweep", "--demo", "--policy", "static:1", "--param", "replicas=1,2", "--slo", "attainment>=99.9%"); code != ExitSLOFail {
		t.Errorf("sweep with no passing setting: exit %d", code)
	}
}

func TestOutputFiles(t *testing.T) {
	dir := t.TempDir()
	tl, ev, rq := filepath.Join(dir, "timeline.csv"), filepath.Join(dir, "events.csv"), filepath.Join(dir, "requests.csv")
	code, _, errOut := run(t, "run", "--demo", "--timeline-out", tl, "--events-out", ev, "--requests-out", rq, "--timeline-step", "30s")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	lines := func(path string) []string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	if n := len(lines(tl)); n < 91 || n > 96 {
		t.Errorf("timeline rows: %d, want about 45 min / 30 s", n)
	}
	if n := len(lines(ev)); n != 18 {
		t.Errorf("event rows: %d, want 17 events and a header", n)
	}
	if n := len(lines(rq)); n != 4798 {
		t.Errorf("request rows: %d", n)
	}
}

func TestTraceCommands(t *testing.T) {
	dir := t.TempDir()
	scaled, sliced, synth, prom, conv := filepath.Join(dir, "scaled.csv"), filepath.Join(dir, "sliced.csv"), filepath.Join(dir, "synth.csv"), filepath.Join(dir, "prom.csv"), filepath.Join(dir, "conv.csv")
	steps := [][]string{
		{"trace", "scale", ex + "traces/demo.csv", "-o", scaled, "--time-compress", "3", "--load", "0.5"},
		{"trace", "slice", "-o", sliced, "--from", "10m", "--to", "20m", ex + "traces/demo.csv"},
		{"trace", "convert", ex + "traces/azure-format-sample.csv", "-o", conv},
		{"trace", "synth", "-o", synth, "--profile", "0:2,5m:2", "--seed", "3"},
		{"trace", "from-prometheus", "-o", prom, "--rate-json", ex + "prometheus/request-rate.json",
			"--output-rate-json", ex + "prometheus/output-token-rate.json", "--prompt-rate-json", ex + "prometheus/prompt-token-rate.json"},
	}
	for _, args := range steps {
		if code, _, errOut := run(t, args...); code != ExitOK {
			t.Fatalf("%v: exit %d: %s", args, code, errOut)
		}
	}
	stats := func(path string) map[string]any {
		code, out, errOut := run(t, "trace", "stats", path, "--format", "json")
		if code != ExitOK {
			t.Fatalf("stats %s: %s", path, errOut)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if s := stats(scaled); s["duration_seconds"].(float64) != 900 || s["requests"].(float64) < 2200 || s["requests"].(float64) > 2600 {
		t.Errorf("scaled: %v requests over %v s", s["requests"], s["duration_seconds"])
	}
	if s := stats(sliced); s["duration_seconds"].(float64) != 600 || s["mean_rate_rps"].(float64) < 2.5 {
		t.Errorf("sliced: %v", s)
	}
	if s := stats(conv); s["requests"].(float64) != 10 {
		t.Errorf("converted: %v", s)
	}
	if s := stats(synth); s["duration_seconds"].(float64) != 300 {
		t.Errorf("synth: %v", s)
	}
	if s := stats(prom); s["approximate"] != true {
		t.Errorf("a trace from Prometheus must stay marked approximate: %v", s)
	}
	// The approximation is called out in the run report.
	code, out, _ := run(t, "run", "--policy", "static:3", "--trace", prom, "--replica", ex+"replicas/demo-gpu.yaml")
	if code != ExitOK || !strings.Contains(out, "APPROXIMATE TRACE") || !strings.Contains(out, "synthesised from a Prometheus rate series") {
		t.Errorf("approximate trace not labelled:\n%s", out)
	}
}

func TestCalibrateWritesUsableModel(t *testing.T) {
	out := filepath.Join(t.TempDir(), "fitted.yaml")
	code, _, errOut := run(t, "calibrate", "--bench", ex+"benchmarks/guidellm-benchmarks.json", "--base", ex+"replicas/demo-gpu.yaml", "-o", out, "--name", "fitted")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	code, stdout, errOut := run(t, "run", "--policy", "static:4", "--trace", ex+"traces/demo.csv", "--replica", out)
	if code != ExitOK || !strings.Contains(stdout, "replica model fitted") {
		t.Fatalf("the calibrated model must be usable: exit %d: %s", code, errOut)
	}
}

func TestMetricAlias(t *testing.T) {
	policy := filepath.Join(t.TempDir(), "hpa.yaml")
	doc := "apiVersion: autoscaling/v2\nkind: HorizontalPodAutoscaler\nmetadata: {name: custom}\nspec:\n  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: x}\n  maxReplicas: 4\n  metrics:\n  - type: Pods\n    pods:\n      metric: {name: inference_queue_size}\n      target: {type: AverageValue, averageValue: 3}\n"
	if err := os.WriteFile(policy, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, "run", "--demo", "--policy", policy); code != ExitError || !strings.Contains(errOut, "--metric-alias inference_queue_size=") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if code, _, errOut := run(t, "run", "--demo", "--policy", policy, "--metric-alias", "inference_queue_size=vllm:num_requests_waiting"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestUsageErrors(t *testing.T) {
	cases := [][]string{
		{},
		{"frobnicate"},
		{"run"},
		{"run", "--policy", "static:2"},
		{"run", "--demo", "--format", "xml"},
		{"run", "--demo", "--lb", "random"},
		{"run", "--demo", "extra"},
		{"run", "--demo", "--policy", "/nonexistent.yaml"},
		{"run", "--demo", "--load", "0"},
		{"run", "--demo", "--metric-alias", "x"},
		{"compare"},
		{"sweep", "--demo", "--param", "bogus=1,2"},
		{"sweep", "--demo", "--param", "target"},
		{"sweep", "--demo", "--param", "target=5:1:1"},
		{"sweep", "--policy", "static:1"},
		{"explain"},
		{"trace"},
		{"trace", "nope"},
		{"trace", "stats"},
		{"trace", "synth"},
		{"trace", "from-prometheus"},
		{"trace", "slice", ex + "traces/demo.csv", "--from", "50m"},
		{"calibrate"},
		{"calibrate", "--doriki", ex + "coldstart/doriki-warm-node.json"},
	}
	for _, args := range cases {
		code, _, errOut := run(t, args...)
		if code != ExitError || errOut == "" {
			t.Errorf("%v: exit %d, stderr %q; want exit 1 and a message", args, code, errOut)
		}
	}
	for _, args := range [][]string{{"help"}, {"version"}, {"run", "-h"}, {"trace", "help"}} {
		if code, _, _ := run(t, args...); code != ExitOK {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	// Invalid combinations inside a sweep are skipped, not fatal.
	code, out, errOut := run(t, "sweep", "--demo", "--param", "minReplicas=1,9", "--param", "maxReplicas=8")
	if code != ExitOK || !strings.Contains(errOut, "skipping") || strings.Count(out, "\n") < 4 {
		t.Errorf("exit %d\n%s\n%s", code, out, errOut)
	}
}
