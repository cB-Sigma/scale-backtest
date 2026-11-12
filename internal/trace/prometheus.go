package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Series is one aggregate time series: a value per timestamp.
type Series struct {
	// T are Unix timestamps in seconds, increasing.
	T []float64
	V []float64
}

type promResponse struct {
	Status string    `json:"status"`
	Error  string    `json:"error"`
	Data   *promData `json:"data"`
	// A file may also hold just the data object.
	ResultType string       `json:"resultType"`
	Result     []promMatrix `json:"result"`
}

type promData struct {
	ResultType string       `json:"resultType"`
	Result     []promMatrix `json:"result"`
}

type promMatrix struct {
	Metric map[string]string    `json:"metric"`
	Values [][2]json.RawMessage `json:"values"`
}

// ParsePrometheusMatrix parses the JSON body of a Prometheus
// /api/v1/query_range response (or just its "data" object). When the result
// holds several series they are summed per timestamp; nSeries reports how
// many there were.
func ParsePrometheusMatrix(data []byte) (s Series, nSeries int, err error) {
	var resp promResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return s, 0, fmt.Errorf("not a Prometheus query_range response: %w", err)
	}
	if resp.Status != "" && resp.Status != "success" {
		return s, 0, fmt.Errorf("the Prometheus server returned status %q: %s", resp.Status, resp.Error)
	}
	rt, result := resp.ResultType, resp.Result
	if resp.Data != nil {
		rt, result = resp.Data.ResultType, resp.Data.Result
	}
	if rt != "matrix" {
		return s, 0, fmt.Errorf("expected resultType \"matrix\" (a query_range response), got %q", rt)
	}
	if len(result) == 0 {
		return s, 0, errors.New("the query_range response contains no series")
	}
	sum := map[float64]float64{}
	for _, m := range result {
		for _, pair := range m.Values {
			var ts float64
			if err := json.Unmarshal(pair[0], &ts); err != nil {
				return s, 0, fmt.Errorf("bad sample timestamp %s", pair[0])
			}
			var text string
			if err := json.Unmarshal(pair[1], &text); err != nil {
				return s, 0, fmt.Errorf("bad sample value %s", pair[1])
			}
			v, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return s, 0, fmt.Errorf("bad sample value %q", text)
			}
			if math.IsNaN(v) || math.IsInf(v, 0) {
				v = 0
			}
			sum[ts] += v
		}
	}
	for ts := range sum {
		s.T = append(s.T, ts)
	}
	sort.Float64s(s.T)
	for _, ts := range s.T {
		s.V = append(s.V, sum[ts])
	}
	if len(s.T) == 0 {
		return s, 0, errors.New("the query_range response contains no samples")
	}
	return s, len(result), nil
}

// LoadPrometheusMatrix reads a query_range response from a file.
func LoadPrometheusMatrix(path string) (Series, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Series{}, 0, err
	}
	s, n, err := ParsePrometheusMatrix(data)
	if err != nil {
		return s, n, fmt.Errorf("%s: %w", path, err)
	}
	return s, n, nil
}

// FetchPrometheusRange runs a query_range against a Prometheus-compatible
// server.
func FetchPrometheusRange(ctx context.Context, client *http.Client, baseURL, query string, start, end time.Time, step time.Duration) (Series, int, error) {
	if client == nil {
		client = http.DefaultClient
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/") + "/api/v1/query_range")
	if err != nil {
		return Series{}, 0, err
	}
	q := u.Query()
	q.Set("query", query)
	q.Set("start", strconv.FormatFloat(float64(start.UnixNano())/1e9, 'f', 3, 64))
	q.Set("end", strconv.FormatFloat(float64(end.UnixNano())/1e9, 'f', 3, 64))
	q.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Series{}, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Series{}, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return Series{}, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return Series{}, 0, fmt.Errorf("query_range returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 300)])))
	}
	return ParsePrometheusMatrix(body)
}

// CoarseOptions controls FromRateSeries.
type CoarseOptions struct {
	// Rate is the request rate in requests per second. Required.
	Rate Series
	// PromptRate and OutputRate are token rates in tokens per second. When
	// present, the mean token counts of a bucket are the token rate divided
	// by the request rate.
	PromptRate, OutputRate *Series
	// PromptMean and OutputMean are used for buckets without token-rate
	// data.
	PromptMean, OutputMean float64
	// TokenCV is the coefficient of variation of the per-request token
	// counts inside a bucket (log-normal). History gives no information
	// about it; 1.0 is a common order of magnitude for chat traffic.
	TokenCV float64
	Seed    uint64
}

func medianStep(ts []float64) float64 {
	if len(ts) < 2 {
		return 0
	}
	d := make([]float64, 0, len(ts)-1)
	for i := 1; i < len(ts); i++ {
		d = append(d, ts[i]-ts[i-1])
	}
	sort.Float64s(d)
	return d[len(d)/2]
}

func (s *Series) at(ts float64) (float64, bool) {
	if s == nil {
		return 0, false
	}
	i := sort.SearchFloat64s(s.T, ts)
	if i < len(s.T) && s.T[i] == ts {
		return s.V[i], true
	}
	// Nearest earlier sample within one step.
	if i > 0 {
		return s.V[i-1], true
	}
	return 0, false
}

// FromRateSeries synthesises a per-request trace from aggregate rate series.
//
// Each sample of the rate series is taken to hold for one step (the bucket
// that ends at the sample's timestamp, as rate() looks backwards). Inside a
// bucket arrivals are a Poisson process at that rate, and token counts are
// log-normal around the bucket's mean. Everything below the bucket length -
// bursts within a bucket, correlation between prompt and output sizes, the
// real token distributions - is invented, so the result is marked
// Approximate.
func FromRateSeries(o CoarseOptions) (*Trace, error) {
	if len(o.Rate.T) < 2 {
		return nil, errors.New("the request-rate series needs at least two samples")
	}
	step := medianStep(o.Rate.T)
	if step <= 0 {
		return nil, errors.New("the request-rate series has no positive step")
	}
	if o.PromptMean <= 0 || o.OutputMean <= 0 {
		return nil, errors.New("default prompt and output token means must be positive")
	}
	if o.TokenCV < 0 {
		return nil, errors.New("token CV must not be negative")
	}
	rng := NewRand(o.Seed)
	origin := o.Rate.T[0] - step
	t := &Trace{Name: "prometheus", Format: "prometheus", Approximate: true}
	sec := int64(origin)
	t.Start = time.Unix(sec, int64((origin-float64(sec))*1e9)).UTC()
	for i, ts := range o.Rate.T {
		rate := o.Rate.V[i]
		if rate <= 0 {
			continue
		}
		width := step
		if i > 0 && ts-o.Rate.T[i-1] < step {
			width = ts - o.Rate.T[i-1]
		}
		bucketStart := ts - width - origin
		pm, om := o.PromptMean, o.OutputMean
		if v, ok := o.PromptRate.at(ts); ok && v > 0 {
			pm = v / rate
		}
		if v, ok := o.OutputRate.at(ts); ok && v > 0 {
			om = v / rate
		}
		at := 0.0
		for {
			at += rng.ExpFloat64() / rate
			if at >= width {
				break
			}
			t.Requests = append(t.Requests, Request{
				Arrival:      math.Round((bucketStart+at)*1000) / 1000,
				PromptTokens: drawTokens(rng, pm, o.TokenCV, 0),
				OutputTokens: drawTokens(rng, om, o.TokenCV, 0),
			})
		}
	}
	t.Duration = o.Rate.T[len(o.Rate.T)-1] - origin
	tokens := "fixed token means"
	if o.PromptRate != nil || o.OutputRate != nil {
		tokens = "per-bucket token means from token-rate series"
	}
	t.Notes = append(t.Notes, fmt.Sprintf("arrivals synthesised from a Prometheus rate series with %gs buckets (Poisson within a bucket, %s, log-normal token counts with CV %g, seed %d)", step, tokens, o.TokenCV, o.Seed))
	t.normalise()
	if len(t.Requests) == 0 {
		return nil, errors.New("the rate series is zero everywhere: no requests were synthesised")
	}
	return t, nil
}

func secondsToDuration(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}
