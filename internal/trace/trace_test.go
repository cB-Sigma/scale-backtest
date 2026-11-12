package trace

import (
	"bytes"
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func mustRead(t *testing.T, data, format, name string) *Trace {
	t.Helper()
	tr, err := Read(strings.NewReader(data), format, name)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestReadNativeCSV(t *testing.T) {
	tr := mustRead(t, "# a comment\ntimestamp,prompt_tokens,output_tokens,model,tenant\n2.5,100,20,m1,a\n0.5,300,40,m2,b\n10,50,5,m1,a\n", FormatAuto, "x.csv")
	if tr.Format != FormatCSV || len(tr.Requests) != 3 || tr.Duration != 10 {
		t.Fatalf("%+v", tr)
	}
	// Sorted by arrival; numeric timestamps near zero are kept as they are.
	if tr.Requests[0].Arrival != 0.5 || tr.Requests[0].PromptTokens != 300 || tr.Requests[0].Model != "m2" || tr.Requests[2].Tenant != "a" {
		t.Fatalf("%+v", tr.Requests)
	}
}

func TestReadColumnAliasesAndUnixTime(t *testing.T) {
	tr := mustRead(t, "arrival_time,input_tokens,completion_tokens\n1767225600.250,10,2\n1767225601.750,11,3\n", FormatAuto, "x.csv")
	if len(tr.Requests) != 2 || tr.Requests[0].Arrival != 0 || math.Abs(tr.Requests[1].Arrival-1.5) > 1e-6 {
		t.Fatalf("%+v", tr.Requests)
	}
	if tr.Start.Unix() != 1767225600 {
		t.Fatalf("start = %v", tr.Start)
	}
}

// The column names and both timestamp layouts are those of the Azure public
// dataset (AzureLLMInferenceDataset2023.md and AzureLLMInferenceDataset2024.md
// in github.com/Azure/AzurePublicDataset: TIMESTAMP, ContextTokens,
// GeneratedTokens).
func TestReadAzureFormats(t *testing.T) {
	y2023 := "TIMESTAMP,ContextTokens,GeneratedTokens\n2023-11-16 18:15:46.6805900,374,44\n2023-11-16 18:15:50.9951690,396,109\n"
	tr := mustRead(t, y2023, FormatAuto, "AzureLLMInferenceTrace_conv.csv")
	if tr.Format != FormatAzure || len(tr.Requests) != 2 {
		t.Fatalf("%+v", tr)
	}
	if got := tr.Requests[1].Arrival; math.Abs(got-4.314579) > 1e-6 {
		t.Fatalf("arrival = %v", got)
	}
	if tr.Requests[1].PromptTokens != 396 || tr.Requests[1].OutputTokens != 109 {
		t.Fatalf("%+v", tr.Requests[1])
	}
	if want := time.Date(2023, 11, 16, 18, 15, 46, 680590000, time.UTC); !tr.Start.Equal(want) {
		t.Fatalf("start = %v", tr.Start)
	}
	y2024 := "TIMESTAMP,ContextTokens,GeneratedTokens\n2024-05-10 00:00:00.009930+00:00,2162,5\n2024-05-10 00:00:00.017335+00:00,2399,6\n"
	tr = mustRead(t, y2024, FormatAuto, "x.csv")
	if tr.Format != FormatAzure || math.Abs(tr.Requests[1].Arrival-0.007405) > 1e-9 {
		t.Fatalf("%+v", tr)
	}
	if _, err := Read(strings.NewReader("timestamp,prompt_tokens,output_tokens\n1,2,3\n"), FormatAzure, ""); err == nil {
		t.Fatal("forcing the azure format on other columns must fail")
	}
	sample := mustLoad(t, "../../examples/traces/azure-format-sample.csv")
	if sample.Format != FormatAzure || len(sample.Requests) != 10 {
		t.Fatalf("example: %+v", sample)
	}
}

func mustLoad(t *testing.T, path string) *Trace {
	t.Helper()
	tr, err := Load(path, FormatAuto)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestReadJSONL(t *testing.T) {
	tr := mustLoad(t, "../../examples/traces/sample.jsonl")
	if tr.Format != FormatJSONL || len(tr.Requests) != 6 || tr.Name != "sample" {
		t.Fatalf("%+v", tr)
	}
	r := tr.Requests[1]
	if math.Abs(r.Arrival-0.75) > 1e-9 || r.PromptTokens != 1890 || r.OutputTokens != 420 || r.Model != "chat-8b" || r.Tenant != "acme" {
		t.Fatalf("%+v", r)
	}
	tr = mustRead(t, `{"arrival": 1.5, "input_tokens": 5, "completion_tokens": 6, "user": "u"}`+"\n", FormatAuto, "")
	if tr.Requests[0].Arrival != 1.5 || tr.Requests[0].Tenant != "u" {
		t.Fatalf("%+v", tr.Requests[0])
	}
}

func TestReadErrors(t *testing.T) {
	cases := map[string]string{
		"timestamp,prompt_tokens\n1,2\n":                                           "header needs",
		"timestamp,prompt_tokens,output_tokens\n":                                  "no requests",
		"timestamp,prompt_tokens,output_tokens\nyesterday,1,2\n":                   "cannot parse timestamp",
		"timestamp,prompt_tokens,output_tokens\n1,-4,2\n":                          "prompt tokens",
		"timestamp,prompt_tokens,output_tokens\n1,4\n":                             "expected at least 3 columns",
		"timestamp,prompt_tokens,output_tokens\n1,4,5\n2026-01-01T00:00:00Z,1,2\n": "mixes absolute and numeric",
		`{"timestamp": 1, "prompt_tokens": 5}` + "\n":                              "need prompt_tokens and output_tokens",
		`{"prompt_tokens": 5, "output_tokens": 5}` + "\n":                          "no timestamp",
		`{"timestamp": ` + "\n":                                                    "line 1",
	}
	for in, want := range cases {
		_, err := Read(strings.NewReader(in), FormatAuto, "")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want an error mentioning %q", in, err, want)
		}
	}
	if _, err := Read(strings.NewReader("x"), "parquet", ""); err == nil {
		t.Error("unknown format must fail")
	}
	if _, err := Load("/nonexistent/trace.csv", FormatAuto); err == nil {
		t.Error("missing file must fail")
	}
}

func TestWriteCSVRoundTrip(t *testing.T) {
	tr := &Trace{Duration: 60, Requests: []Request{{Arrival: 1.25, PromptTokens: 10, OutputTokens: 5, Model: "m"}, {Arrival: 2, PromptTokens: 20, OutputTokens: 6}},
		Approximate: true, Notes: []string{"from aggregates"}}
	var buf bytes.Buffer
	if err := tr.WriteCSV(&buf); err != nil {
		t.Fatal(err)
	}
	back := mustRead(t, buf.String(), FormatAuto, "x.csv")
	if back.Duration != 60 || !back.Approximate || back.Notes[0] != "from aggregates" || len(back.Requests) != 2 || back.Requests[0] != tr.Requests[0] {
		t.Fatalf("round trip: %+v\n%s", back, buf.String())
	}
}

func poisson(n int, rate float64, seed uint64) *Trace {
	rng := NewRand(seed)
	tr := &Trace{}
	now := 0.0
	for range n {
		now += rng.ExpFloat64() / rate
		tr.Requests = append(tr.Requests, Request{Arrival: now, PromptTokens: 100, OutputTokens: 50})
	}
	tr.Normalise()
	return tr
}

func TestStatsOnPoissonTraffic(t *testing.T) {
	s := poisson(50000, 5, 3).ComputeStats()
	if math.Abs(s.MeanRate-5) > 0.1 {
		t.Errorf("mean rate %v", s.MeanRate)
	}
	// A Poisson process has inter-arrival CV 1 and index of dispersion 1.
	if math.Abs(s.InterarrivalCV-1) > 0.03 || math.Abs(s.Dispersion10s-1) > 0.15 || math.Abs(s.Dispersion60s-1) > 0.3 {
		t.Errorf("burstiness of a Poisson process: CV %v, dispersion %v / %v", s.InterarrivalCV, s.Dispersion10s, s.Dispersion60s)
	}
	if s.PromptTokens.Mean != 100 || s.OutputTokens.P99 != 50 || math.Abs(s.OutputTokenRate-250) > 5 {
		t.Errorf("tokens: %+v %+v %v", s.PromptTokens, s.OutputTokens, s.OutputTokenRate)
	}
	if s.PeakRate60s < s.MeanRate || s.PeakRate10s < s.PeakRate60s || s.PeakRate1s < s.PeakRate10s {
		t.Errorf("peaks must not decrease with shorter windows: %v %v %v", s.PeakRate60s, s.PeakRate10s, s.PeakRate1s)
	}
}

func TestStatsDetectBursts(t *testing.T) {
	demo := mustLoad(t, "../../examples/traces/demo.csv")
	s := demo.ComputeStats()
	if s.Dispersion60s < 10 || s.PeakToMean60s < 2 {
		t.Errorf("the demo trace has two bursts: dispersion %v, peak-to-mean %v", s.Dispersion60s, s.PeakToMean60s)
	}
	if demo.Duration != 2700 {
		t.Errorf("duration %v, want 2700 (from the duration comment)", demo.Duration)
	}
	if got := Quantile([]float64{1, 2, 3, 4}, 0.5); got != 2.5 {
		t.Errorf("median = %v", got)
	}
	if Quantile(nil, 0.5) != 0 {
		t.Error("quantile of nothing")
	}
}

func TestScale(t *testing.T) {
	base := poisson(20000, 2, 5)
	fast, err := base.Scale(ScaleOptions{TimeCompression: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(fast.Requests) != len(base.Requests) || math.Abs(fast.Duration-base.Duration/4) > 1e-9 {
		t.Fatalf("time compression: %d requests over %v", len(fast.Requests), fast.Duration)
	}
	if got := fast.ComputeStats().MeanRate; math.Abs(got-8) > 0.2 {
		t.Fatalf("rate after 4x compression = %v, want about 8", got)
	}
	for _, mult := range []float64{0.25, 1.5, 3} {
		scaled, err := base.Scale(ScaleOptions{LoadMultiplier: mult, JitterSeconds: 1, Seed: 9})
		if err != nil {
			t.Fatal(err)
		}
		ratio := float64(len(scaled.Requests)) / float64(len(base.Requests))
		if math.Abs(ratio-mult) > 0.03*mult+0.01 {
			t.Errorf("load x%v: got x%v", mult, ratio)
		}
		if scaled.Duration != base.Duration {
			t.Errorf("load multiplier must keep the duration")
		}
		for i := 1; i < len(scaled.Requests); i++ {
			if scaled.Requests[i].Arrival < scaled.Requests[i-1].Arrival {
				t.Fatal("not sorted after scaling")
			}
		}
	}
	tok, _ := base.Scale(ScaleOptions{TokenMultiplier: 2})
	if tok.Requests[0].PromptTokens != 200 || tok.Requests[0].OutputTokens != 100 {
		t.Errorf("token multiplier: %+v", tok.Requests[0])
	}
	a, _ := base.Scale(ScaleOptions{LoadMultiplier: 0.5, Seed: 1})
	b, _ := base.Scale(ScaleOptions{LoadMultiplier: 0.5, Seed: 1})
	if len(a.Requests) != len(b.Requests) || a.Requests[100] != b.Requests[100] {
		t.Error("scaling must be deterministic for a seed")
	}
	if _, err := base.Scale(ScaleOptions{LoadMultiplier: -1}); err == nil {
		t.Error("negative factor must fail")
	}
}

func TestSlice(t *testing.T) {
	tr := &Trace{Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Duration: 100}
	for i := range 100 {
		model := "a"
		if i%2 == 1 {
			model = "b"
		}
		tr.Requests = append(tr.Requests, Request{Arrival: float64(i), PromptTokens: 1, OutputTokens: 1, Model: model, Tenant: "t" + model})
	}
	s, err := tr.Slice(SliceOptions{From: 20, To: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Requests) != 31 || s.Requests[0].Arrival != 0 || s.Duration != 30 || !s.Start.Equal(tr.Start.Add(20*time.Second)) {
		t.Fatalf("%d requests, first %v, duration %v, start %v", len(s.Requests), s.Requests[0].Arrival, s.Duration, s.Start)
	}
	s, _ = tr.Slice(SliceOptions{Model: "b"})
	if len(s.Requests) != 50 {
		t.Fatalf("model filter: %d", len(s.Requests))
	}
	s, _ = tr.Slice(SliceOptions{Tenant: "ta", From: 90})
	if len(s.Requests) != 5 {
		t.Fatalf("tenant filter: %d", len(s.Requests))
	}
	if _, err := tr.Slice(SliceOptions{From: 60, To: 50}); err == nil {
		t.Fatal("empty interval must fail")
	}
}

func parseSeconds(s string) (float64, error) {
	if s == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	return d.Seconds(), err
}

func TestSynthesize(t *testing.T) {
	pts, err := ParseProfile("0:1, 5m:1, 6m:10, 10m:10", parseSeconds)
	if err != nil {
		t.Fatal(err)
	}
	opts := SynthOptions{Profile: pts, PromptMean: 500, OutputMean: 200, PromptCV: 1, OutputCV: 0.8, Seed: 42}
	a, err := Synthesize(opts)
	if err != nil {
		t.Fatal(err)
	}
	// Expected arrivals: 300*1 + 60*5.5 + 240*10 = 3030.
	if n := float64(len(a.Requests)); math.Abs(n-3030) > 4*math.Sqrt(3030) {
		t.Errorf("%v requests, expected about 3030", n)
	}
	if a.Duration != 600 {
		t.Errorf("duration %v", a.Duration)
	}
	s := a.ComputeStats()
	if math.Abs(s.PromptTokens.Mean-500) > 40 || math.Abs(s.OutputTokens.Mean-200) > 15 {
		t.Errorf("token means %v / %v", s.PromptTokens.Mean, s.OutputTokens.Mean)
	}
	b, _ := Synthesize(opts)
	if len(a.Requests) != len(b.Requests) || a.Requests[len(a.Requests)-1] != b.Requests[len(b.Requests)-1] {
		t.Error("the same seed must give the same trace")
	}
	opts.Seed = 43
	c, _ := Synthesize(opts)
	if len(c.Requests) == len(a.Requests) && c.Requests[0] == a.Requests[0] {
		t.Error("a different seed should give a different trace")
	}
	for _, bad := range []string{"", "0:1", "5m:1,1m:2", "0:x,1m:2", "abc"} {
		if _, err := ParseProfile(bad, parseSeconds); err == nil {
			t.Errorf("profile %q: expected an error", bad)
		}
	}
}

// The bundled demo trace is the output of the documented synth command.
func TestDemoTraceIsReproducible(t *testing.T) {
	pts, err := ParseProfile("0:0.5,8m:0.5,10m:3,18m:3,20m:1,30m:1,31m:4.5,36m:4.5,38m:0.5,45m:0.4", parseSeconds)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := Synthesize(SynthOptions{Profile: pts, PromptMean: 600, OutputMean: 400, PromptCV: 1.0, OutputCV: 0.8, MaxPrompt: 8000, MaxOutput: 2000, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tr.WriteCSV(&buf); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("../../examples/traces/demo.csv")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatal("examples/traces/demo.csv is not what `trace synth` produces; regenerate it (see examples/README.md)")
	}
}

const promBody = `{"status":"success","data":{"resultType":"matrix","result":[
 {"metric":{"pod":"a"},"values":[[1000,"1.5"],[1060,"2"],[1120,"NaN"]]},
 {"metric":{"pod":"b"},"values":[[1000,"0.5"],[1060,"2"],[1120,"4"]]}]}}`

func TestParsePrometheusMatrix(t *testing.T) {
	s, n, err := ParsePrometheusMatrix([]byte(promBody))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(s.T) != 3 || s.V[0] != 2 || s.V[1] != 4 || s.V[2] != 4 {
		t.Fatalf("%d series, %+v", n, s)
	}
	// The bare data object is accepted too.
	if _, _, err := ParsePrometheusMatrix([]byte(`{"resultType":"matrix","result":[{"metric":{},"values":[[1,"1"],[2,"1"]]}]}`)); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		`{"status":"error","error":"bad query"}`:                          "bad query",
		`{"status":"success","data":{"resultType":"vector","result":[]}}`: "matrix",
		`{"status":"success","data":{"resultType":"matrix","result":[]}}`: "no series",
		`not json`: "not a Prometheus",
		`{"status":"success","data":{"resultType":"matrix","result":[{"values":[[1,"x"]]}]}}`: "bad sample value",
	} {
		if _, _, err := ParsePrometheusMatrix([]byte(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", in, err)
		}
	}
}

func TestFromRateSeries(t *testing.T) {
	var rate, out Series
	for i := 1; i <= 60; i++ { // one hour at 2 req/s, then the last 20 minutes at 6
		v := 2.0
		if i > 40 {
			v = 6
		}
		rate.T = append(rate.T, float64(1000+60*i))
		rate.V = append(rate.V, v)
		out.T = append(out.T, float64(1000+60*i))
		out.V = append(out.V, v*300)
	}
	tr, err := FromRateSeries(CoarseOptions{Rate: rate, OutputRate: &out, PromptMean: 500, OutputMean: 100, TokenCV: 1, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !tr.Approximate || len(tr.Notes) == 0 || !strings.Contains(tr.Notes[0], "synthesised") {
		t.Fatalf("the trace must be labelled approximate: %+v", tr.Notes)
	}
	want := 40*60*2.0 + 20*60*6.0
	if n := float64(len(tr.Requests)); math.Abs(n-want) > 4*math.Sqrt(want) {
		t.Errorf("%v requests, expected about %v", n, want)
	}
	if tr.Duration != 3600 || tr.Start.Unix() != 1000 {
		t.Errorf("duration %v start %v", tr.Duration, tr.Start.Unix())
	}
	s := tr.ComputeStats()
	// Output mean comes from the token-rate series (300), prompt from the default.
	if math.Abs(s.OutputTokens.Mean-300) > 20 || math.Abs(s.PromptTokens.Mean-500) > 30 {
		t.Errorf("token means: output %v prompt %v", s.OutputTokens.Mean, s.PromptTokens.Mean)
	}
	first, _ := tr.Slice(SliceOptions{To: 2400})
	last, _ := tr.Slice(SliceOptions{From: 2400})
	if r1, r2 := first.ComputeStats().MeanRate, last.ComputeStats().MeanRate; math.Abs(r1-2) > 0.15 || math.Abs(r2-6) > 0.3 {
		t.Errorf("bucket rates %v and %v, want about 2 and 6", r1, r2)
	}
	// The approximation label survives a round trip through CSV.
	var buf bytes.Buffer
	_ = tr.WriteCSV(&buf)
	if back := mustRead(t, buf.String(), FormatAuto, "x.csv"); !back.Approximate {
		t.Error("the approximation marker was lost")
	}
	if _, err := FromRateSeries(CoarseOptions{Rate: Series{T: []float64{1}, V: []float64{1}}, PromptMean: 1, OutputMean: 1}); err == nil {
		t.Error("a single sample must fail")
	}
	zero := Series{T: []float64{60, 120}, V: []float64{0, 0}}
	if _, err := FromRateSeries(CoarseOptions{Rate: zero, PromptMean: 1, OutputMean: 1}); err == nil {
		t.Error("an all-zero series must fail")
	}
}

func TestFetchPrometheusRange(t *testing.T) {
	var gotQuery, gotStep string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		gotQuery, gotStep = r.URL.Query().Get("query"), r.URL.Query().Get("step")
		_, _ = w.Write([]byte(promBody))
	}))
	defer srv.Close()
	start := time.Unix(1000, 0)
	s, n, err := FetchPrometheusRange(context.Background(), srv.Client(), srv.URL+"/", "sum(rate(x[1m]))", start, start.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(s.T) != 3 || gotQuery != "sum(rate(x[1m]))" || gotStep != "60" {
		t.Fatalf("n=%d series=%+v query=%q step=%q", n, s, gotQuery, gotStep)
	}
	if _, _, err := FetchPrometheusRange(context.Background(), srv.Client(), srv.URL+"/missing", "x", start, start, time.Minute); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("got %v", err)
	}
}
