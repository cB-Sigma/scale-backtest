package promql

import (
	"errors"
	"strings"
	"testing"
)

func TestParseSupported(t *testing.T) {
	cases := []struct {
		in     string
		agg    string
		rate   bool
		window float64
		metric string
		match  string
		scale  float64
	}{
		{"vllm:num_requests_waiting", "", false, 0, "vllm:num_requests_waiting", "", 1},
		{"sum(vllm:num_requests_running)", "sum", false, 0, "vllm:num_requests_running", "", 1},
		{` avg ( vllm:num_requests_waiting{namespace="inference", model=~"llama.*"} ) `, "avg", false, 0, "vllm:num_requests_waiting", `namespace="inference", model=~"llama.*"`, 1},
		{"max(gpu_utilization)", "max", false, 0, "gpu_utilization", "", 1},
		{"min(requests_running)", "min", false, 0, "requests_running", "", 1},
		{"rate(gateway_requests_total[2m])", "", true, 120, "gateway_requests_total", "", 1},
		{"sum(rate(vllm:generation_tokens_total{pod=~\"vllm-.*\"}[1m30s]))", "sum", true, 90, "vllm:generation_tokens_total", `pod=~"vllm-.*"`, 1},
		{"sum(rate(vllm:generation_tokens_total[30s])) / 4", "sum", true, 30, "vllm:generation_tokens_total", "", 0.25},
		{"sum(vllm:num_requests_running) * 2", "sum", false, 0, "vllm:num_requests_running", "", 2},
		{`sum(x{a="}"})`, "sum", false, 0, "x", `a="}"`, 1},
	}
	for _, tc := range cases {
		q, err := Parse(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if q.Agg != tc.agg || q.Rate != tc.rate || q.WindowSeconds != tc.window || q.Metric != tc.metric || q.Matchers != tc.match || q.Scale != tc.scale {
			t.Errorf("%q: got %+v", tc.in, q)
		}
		if q.String() == "" {
			t.Errorf("%q: empty String()", tc.in)
		}
	}
}

func TestParseRefused(t *testing.T) {
	cases := map[string]string{
		"": "empty",
		"sum by (pod) (vllm:num_requests_running)":          "grouping",
		"sum(vllm:num_requests_running) by (pod)":           "grouping",
		"histogram_quantile(0.95, sum(rate(x_bucket[5m])))": "not supported",
		"sum(a) / sum(b)": "numeric literal",
		"avg_over_time(vllm:num_requests_waiting[5m])":          "not supported",
		"vllm:num_requests_waiting[5m]":                         "range selector",
		"rate(vllm:generation_tokens_total)":                    "range",
		"sum(rate(vllm:generation_tokens_total[5x]))":           "bad range",
		"sum(vllm:num_requests_running":                         "expected )",
		"sum(vllm:num_requests_running) + 1":                    "unexpected",
		"sum(vllm:num_requests_running) / 0":                    "division by zero",
		"irate(vllm:generation_tokens_total[1m])":               "not supported",
		"sum(x{a=\"b\")":                                        "unterminated",
		"1 + 1":                                                 "expected a metric name",
		"sum(vllm:num_requests_running > 5)":                    "expected )",
		"sum(rate(vllm:generation_tokens_total[1m] offset 5m))": "expected )",
	}
	for in, want := range cases {
		_, err := Parse(in)
		var ue *UnsupportedError
		if !errors.As(err, &ue) {
			t.Errorf("%q: expected an UnsupportedError, got %v", in, err)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %q does not mention %q", in, err, want)
		}
		if !strings.Contains(err.Error(), "supported:") {
			t.Errorf("%q: the error must list what is supported", in)
		}
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]float64{"30s": 30, "2m": 120, "1h30m": 5400, "1d": 86400, "500ms": 0.5, "1.5m": 90} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("%q: got %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "5", "5parsecs"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("%q: expected an error", in)
		}
	}
}
