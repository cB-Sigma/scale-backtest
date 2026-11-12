package metrics

import (
	"math"
	"strings"
	"testing"

	"github.com/cb-Sigma/scale-backtest/internal/promql"
)

func vals(running, waiting, tokens float64) Values {
	var v Values
	v[RequestsRunning], v[RequestsWaiting], v[GenerationTokens] = running, waiting, tokens
	return v
}

func mustQuery(t *testing.T, s string) *promql.Query {
	t.Helper()
	q, err := promql.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	for name, want := range map[string]Ref{
		"vllm:num_requests_waiting": {RequestsWaiting, false},
		"vllm_num_requests_running": {RequestsRunning, false},
		"tokens_per_second":         {GenerationTokens, true},
		"gateway_requests_pending":  {GatewayPending, false},
		"DCGM_FI_DEV_GPU_UTIL":      {GPUUtilization, false},
	} {
		got, ok := r.Resolve(name)
		if !ok || got != want {
			t.Errorf("%s: got %+v, %v", name, got, ok)
		}
	}
	if _, ok := r.Resolve("nginx_connections"); ok {
		t.Error("unknown name resolved")
	}
	if err := r.Alias("my_queue", "vllm:num_requests_waiting"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Resolve("my_queue"); got.Series != RequestsWaiting {
		t.Errorf("alias: %+v", got)
	}
	if err := r.Alias("x", "nope"); err == nil {
		t.Error("alias to an unknown target must fail")
	}
	if !strings.Contains(UnknownError("foo").Error(), "--metric-alias foo=") {
		t.Error("the unknown-metric error should say how to map the name")
	}
	// Every built-in name refers to a described series.
	for _, a := range Builtin() {
		if a.Ref.Series.Describe().Canonical == "" {
			t.Errorf("%s: no description", a.Name)
		}
		if a.Ref.Rate && !a.Ref.Series.Describe().Counter {
			t.Errorf("%s: a rate of a gauge", a.Name)
		}
	}
}

func TestStoreValuesAndStaleness(t *testing.T) {
	s := NewStore(600)
	gauge := Ref{Series: RequestsWaiting}
	if _, ok := s.PodValue(0, 1, gauge); ok {
		t.Fatal("a pod that was never scraped has no value")
	}
	s.Scrape(0, 1, vals(3, 7, 0))
	// The autoscaler sees the scraped value, however stale, until the next
	// scrape or until the staleness limit.
	if v, ok := s.PodValue(14.9, 1, gauge); !ok || v != 7 {
		t.Fatalf("got %v, %v", v, ok)
	}
	if _, ok := s.PodValue(301, 1, gauge); ok {
		t.Fatal("a sample older than the staleness limit must not be used")
	}
	s.Forget(1)
	if _, ok := s.PodValue(1, 1, gauge); ok {
		t.Fatal("a forgotten pod has no value")
	}
}

func TestStoreRate(t *testing.T) {
	s := NewStore(600)
	for i := 0; i <= 8; i++ { // 100 tokens/s for 120 s, scraped every 15 s
		s.Scrape(float64(i)*15, 1, vals(0, 0, float64(i)*1500))
	}
	rate := Ref{Series: GenerationTokens, Rate: true}
	if v, ok := s.PodValue(120, 1, rate); !ok || math.Abs(v-100) > 1e-9 {
		t.Fatalf("rate = %v, %v; want 100", v, ok)
	}
	// One sample in the window is not enough for a rate.
	s2 := NewStore(600)
	s2.Scrape(0, 1, vals(0, 0, 10))
	if _, ok := s2.PodValue(5, 1, rate); ok {
		t.Fatal("a rate needs two samples")
	}
	// A query's own window overrides the default.
	q := mustQuery(t, "sum(rate(vllm:generation_tokens_total[30s]))")
	s.Scrape(135, 1, vals(0, 0, 8*1500+3000)) // 200 tokens/s in the last interval
	v, empty, err := s.Query(135, q, Ref{Series: GenerationTokens})
	if err != nil || empty || math.Abs(v-150) > 1e-9 { // samples at 105, 120, 135
		t.Fatalf("got %v, %v, %v; want 150", v, empty, err)
	}
}

func TestStoreQueryAggregations(t *testing.T) {
	s := NewStore(600)
	s.Scrape(0, 1, vals(4, 0, 0))
	s.Scrape(0, 2, vals(10, 6, 0))
	s.Scrape(0, 3, vals(1, 0, 0))
	var g Values
	g[GatewayPending] = 9
	s.ScrapeGateway(0, g)
	run := Ref{Series: RequestsRunning}
	for query, want := range map[string]float64{
		"sum(vllm:num_requests_running)":     15,
		"avg(vllm:num_requests_running)":     5,
		"max(vllm:num_requests_running)":     10,
		"min(vllm:num_requests_running)":     1,
		"sum(vllm:num_requests_running) / 3": 5,
	} {
		v, empty, err := s.Query(1, mustQuery(t, query), run)
		if err != nil || empty || math.Abs(v-want) > 1e-9 {
			t.Errorf("%s: got %v, %v, %v; want %v", query, v, empty, err, want)
		}
	}
	if v, _, err := s.Query(1, mustQuery(t, "gateway_requests_pending"), Ref{Series: GatewayPending}); err != nil || v != 9 {
		t.Errorf("gateway gauge: %v, %v", v, err)
	}
	if _, _, err := s.Query(1, mustQuery(t, "vllm:num_requests_running"), run); err == nil {
		t.Error("a bare per-replica selector with several series must be an error")
	}
	if total, empty := s.Total(1, run); empty || total != 15 {
		t.Errorf("Total = %v, %v", total, empty)
	}
	// No replicas: the query selects nothing.
	empty := NewStore(600)
	if v, isEmpty, err := empty.Query(1, mustQuery(t, "sum(vllm:num_requests_running)"), run); err != nil || !isEmpty || v != 0 {
		t.Errorf("empty store: %v, %v, %v", v, isEmpty, err)
	}
	if _, isEmpty := empty.Total(1, run); !isEmpty {
		t.Error("Total on an empty store must report empty")
	}
}

func TestCheckQuery(t *testing.T) {
	r := NewRegistry()
	ok := []string{
		"sum(vllm:num_requests_waiting)",
		"gateway_requests_pending",
		"rate(gateway_requests_total[1m])",
		"sum(rate(vllm:generation_tokens_total[2m]))",
		"sum(tokens_per_second)",
	}
	for _, q := range ok {
		if _, err := r.CheckQuery(mustQuery(t, q)); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
	bad := map[string]string{
		"sum(nginx_connections)":                   "not mapped",
		"vllm:num_requests_waiting":                "aggregate",
		"sum(vllm:generation_tokens_total)":        "counter",
		"sum(rate(vllm:num_requests_running[1m]))": "gauge",
		"sum(rate(tokens_per_second[1m]))":         "already",
	}
	for q, want := range bad {
		_, err := r.CheckQuery(mustQuery(t, q))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error mentioning %q", q, err, want)
		}
	}
}
