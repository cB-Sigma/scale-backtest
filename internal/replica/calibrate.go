package replica

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// BenchPoint is one measurement of a load test at a fixed concurrency.
type BenchPoint struct {
	// Concurrency is the number of requests in flight.
	Concurrency float64
	// TTFT is the time to first token in seconds (0: not measured).
	TTFT float64
	// TPOT is the time per output token in seconds, ideally the
	// inter-token latency (0: not measured).
	TPOT float64
	// Throughput is the output tokens per second of the whole server
	// (0: not measured).
	Throughput float64
	// PromptTokens and OutputTokens are the mean sizes of the requests
	// (0: unknown).
	PromptTokens, OutputTokens float64
}

// Benchmark formats.
const (
	BenchCSV      = "csv"
	BenchGuideLLM = "guidellm"
)

// LoadBenchmark reads benchmark points from a file. format is "csv",
// "guidellm" or "auto".
func LoadBenchmark(path, format string) ([]BenchPoint, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	if format == "" || format == "auto" {
		if strings.HasPrefix(strings.TrimSpace(string(data[:min(len(data), 64)])), "{") {
			format = BenchGuideLLM
		} else {
			format = BenchCSV
		}
	}
	var pts []BenchPoint
	switch format {
	case BenchCSV:
		pts, err = ParseBenchCSV(data)
	case BenchGuideLLM:
		pts, err = ParseGuideLLM(data)
	default:
		return nil, "", fmt.Errorf("unknown benchmark format %q (csv, guidellm)", format)
	}
	if err != nil {
		return nil, format, fmt.Errorf("%s: %w", path, err)
	}
	return pts, format, nil
}

func benchKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(s, "\xef\xbb\xbf")))
	return strings.NewReplacer("-", "_", " ", "_").Replace(s)
}

// ParseBenchCSV parses the simple benchmark CSV: a header and one row per
// concurrency level. Recognised columns (units are part of the name):
//
//	concurrency
//	ttft_ms | ttft_s
//	tpot_ms | tpot_s | itl_ms | itl_s
//	throughput_tokens_per_s (also output_tokens_per_s, tokens_per_s)
//	prompt_tokens, output_tokens          mean request sizes, optional
func ParseBenchCSV(data []byte) ([]BenchPoint, error) {
	var clean bytes.Buffer
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		clean.WriteString(line + "\n")
	}
	rd := csv.NewReader(&clean)
	rd.TrimLeadingSpace = true
	rd.FieldsPerRecord = -1
	header, err := rd.Read()
	if err != nil {
		return nil, fmt.Errorf("reading the header: %w", err)
	}
	type colSpec struct {
		set   func(p *BenchPoint, v float64)
		scale float64
	}
	known := map[string]colSpec{
		"concurrency":             {func(p *BenchPoint, v float64) { p.Concurrency = v }, 1},
		"ttft_ms":                 {func(p *BenchPoint, v float64) { p.TTFT = v }, 0.001},
		"ttft_s":                  {func(p *BenchPoint, v float64) { p.TTFT = v }, 1},
		"tpot_ms":                 {func(p *BenchPoint, v float64) { p.TPOT = v }, 0.001},
		"tpot_s":                  {func(p *BenchPoint, v float64) { p.TPOT = v }, 1},
		"itl_ms":                  {func(p *BenchPoint, v float64) { p.TPOT = v }, 0.001},
		"itl_s":                   {func(p *BenchPoint, v float64) { p.TPOT = v }, 1},
		"throughput_tokens_per_s": {func(p *BenchPoint, v float64) { p.Throughput = v }, 1},
		"output_tokens_per_s":     {func(p *BenchPoint, v float64) { p.Throughput = v }, 1},
		"tokens_per_s":            {func(p *BenchPoint, v float64) { p.Throughput = v }, 1},
		"prompt_tokens":           {func(p *BenchPoint, v float64) { p.PromptTokens = v }, 1},
		"output_tokens":           {func(p *BenchPoint, v float64) { p.OutputTokens = v }, 1},
	}
	cols := make([]*colSpec, len(header))
	haveConc := false
	for i, h := range header {
		k := benchKey(h)
		if k == "ttft" || k == "tpot" || k == "itl" {
			return nil, fmt.Errorf("column %q has no unit: name it %s_ms or %s_s", h, k, k)
		}
		if spec, ok := known[k]; ok {
			cols[i] = &spec
			haveConc = haveConc || k == "concurrency"
		}
	}
	if !haveConc {
		return nil, fmt.Errorf("no concurrency column in header %v", header)
	}
	var pts []BenchPoint
	row := 1
	for {
		rec, err := rd.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		row++
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", row, err)
		}
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue
		}
		var p BenchPoint
		for i, cell := range rec {
			if i >= len(cols) || cols[i] == nil || strings.TrimSpace(cell) == "" {
				continue
			}
			v, err := strconv.ParseFloat(strings.TrimSpace(cell), 64)
			if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, fmt.Errorf("row %d, column %s: bad value %q", row, header[i], cell)
			}
			cols[i].set(&p, v*cols[i].scale)
		}
		if p.Concurrency < 1 {
			return nil, fmt.Errorf("row %d: concurrency must be at least 1", row)
		}
		pts = append(pts, p)
	}
	return pts, nil
}

type guideDist struct {
	Mean   float64 `json:"mean"`
	Median float64 `json:"median"`
	Count  int     `json:"count"`
}

type guideStatus struct {
	Successful *guideDist `json:"successful"`
}

type guideReport struct {
	Benchmarks []struct {
		Metrics map[string]json.RawMessage `json:"metrics"`
	} `json:"benchmarks"`
}

// ParseGuideLLM extracts one point per benchmark from a GuideLLM
// benchmarks.json report (GenerativeBenchmarksReport). It reads, under
// benchmarks[].metrics, the "successful" distribution of:
//
//	request_concurrency.mean          -> Concurrency
//	time_to_first_token_ms.median     -> TTFT
//	inter_token_latency_ms.mean       -> TPOT (falls back to time_per_output_token_ms.mean)
//	output_tokens_per_second.mean     -> Throughput
//	prompt_token_count.mean, output_token_count.mean
func ParseGuideLLM(data []byte) ([]BenchPoint, error) {
	var rep guideReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, fmt.Errorf("not a GuideLLM benchmarks JSON report: %w", err)
	}
	if len(rep.Benchmarks) == 0 {
		return nil, errors.New("no benchmarks[] in the report: is this a GuideLLM benchmarks.json?")
	}
	get := func(m map[string]json.RawMessage, key string) *guideDist {
		raw, ok := m[key]
		if !ok {
			return nil
		}
		var st guideStatus
		if err := json.Unmarshal(raw, &st); err != nil {
			return nil
		}
		return st.Successful
	}
	var pts []BenchPoint
	for i, b := range rep.Benchmarks {
		conc := get(b.Metrics, "request_concurrency")
		if conc == nil {
			return nil, fmt.Errorf("benchmarks[%d]: no metrics.request_concurrency.successful", i)
		}
		p := BenchPoint{Concurrency: conc.Mean}
		if d := get(b.Metrics, "time_to_first_token_ms"); d != nil {
			p.TTFT = d.Median / 1000
		}
		if d := get(b.Metrics, "inter_token_latency_ms"); d != nil && d.Mean > 0 {
			p.TPOT = d.Mean / 1000
		} else if d := get(b.Metrics, "time_per_output_token_ms"); d != nil {
			p.TPOT = d.Mean / 1000
		}
		if d := get(b.Metrics, "output_tokens_per_second"); d != nil {
			p.Throughput = d.Mean
		}
		if d := get(b.Metrics, "prompt_token_count"); d != nil {
			p.PromptTokens = d.Mean
		}
		if d := get(b.Metrics, "output_token_count"); d != nil {
			p.OutputTokens = d.Mean
		}
		if p.Concurrency < 0.5 {
			continue // an idle or failed run
		}
		pts = append(pts, p)
	}
	if len(pts) == 0 {
		return nil, errors.New("no benchmark in the report has a usable concurrency")
	}
	return pts, nil
}

// FitQuality is the goodness of fit of one fitted relation.
type FitQuality struct {
	// N is the number of points used.
	N int `json:"n"`
	// R2 is the coefficient of determination (1 is a perfect fit). It is
	// NaN-free: with fewer than three points or no variance it is reported
	// as 1 when the residuals are zero and 0 otherwise.
	R2 float64 `json:"r2"`
	// RMSE is the root mean squared error in the unit of the quantity.
	RMSE float64 `json:"rmse"`
	// MAPE is the mean absolute percentage error, 0..1.
	MAPE float64 `json:"mape"`
	// MaxAPE is the largest absolute percentage error, 0..1.
	MaxAPE float64 `json:"max_ape"`
}

// FitRow compares one benchmark point with the fitted model.
type FitRow struct {
	Concurrency                 float64
	TPOT, FittedTPOT            float64
	TTFT, FittedTTFT            float64
	Throughput, FittedThroughpt float64
}

// Fit is the result of a calibration.
type Fit struct {
	Model *Model
	// TPOT, TTFT and Throughput describe how well the fitted model
	// reproduces the benchmark. Throughput is a cross-check: it is not
	// fitted (unless TPOT was missing and derived from it).
	TPOT, TTFT, Throughput FitQuality
	Rows                   []FitRow
	Notes                  []string
}

// FitOptions controls Calibrate.
type FitOptions struct {
	// Base supplies everything the benchmark cannot: cold start, cost,
	// queue limits. May be nil.
	Base *Model
	// MaxConcurrency overrides the batch limit (0: from Base, else the
	// highest benchmarked concurrency).
	MaxConcurrency int
	// PromptTokens is the mean prompt length of the benchmark, used when
	// the file does not carry it.
	PromptTokens float64
	// PrefillBaseSeconds is the fixed part of TTFT. A benchmark with a
	// single prompt length cannot separate it from the per-token part.
	PrefillBaseSeconds float64
	Name               string
}

// roundSig rounds to six significant digits, which is far more than a
// benchmark supports and keeps the written model readable.
func roundSig(v float64) float64 {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	mag := math.Pow(10, 5-math.Floor(math.Log10(math.Abs(v))))
	return math.Round(v*mag) / mag
}

// linear fits y = a + b*x by least squares.
func linear(xs, ys []float64) (a, b float64) {
	n := float64(len(xs))
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return sy / n, 0
	}
	b = (n*sxy - sx*sy) / den
	a = (sy - b*sx) / n
	return a, b
}

// nonNegLinear fits y = a + b*x with a >= 0 and b >= 0.
func nonNegLinear(xs, ys []float64) (a, b float64) {
	a, b = linear(xs, ys)
	if a >= 0 && b >= 0 {
		return a, b
	}
	if b < 0 {
		// Flat: the mean.
		sum := 0.0
		for _, y := range ys {
			sum += y
		}
		return sum / float64(len(ys)), 0
	}
	// a < 0: through the origin.
	var sxx, sxy float64
	for i := range xs {
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	return 0, sxy / sxx
}

func quality(measured, fitted []float64) FitQuality {
	q := FitQuality{N: len(measured)}
	if q.N == 0 {
		return q
	}
	mean := 0.0
	for _, m := range measured {
		mean += m
	}
	mean /= float64(q.N)
	var ssRes, ssTot float64
	for i, m := range measured {
		res := m - fitted[i]
		ssRes += res * res
		ssTot += (m - mean) * (m - mean)
		if m != 0 {
			ape := math.Abs(res / m)
			q.MAPE += ape
			q.MaxAPE = math.Max(q.MaxAPE, ape)
		}
	}
	q.MAPE /= float64(q.N)
	q.RMSE = math.Sqrt(ssRes / float64(q.N))
	switch {
	case ssTot > 0:
		q.R2 = 1 - ssRes/ssTot
	case ssRes < 1e-18:
		q.R2 = 1
	}
	return q
}

// Calibrate fits the replica model's decode curve and prefill cost to
// benchmark points and reports the goodness of fit.
func Calibrate(points []BenchPoint, o FitOptions) (*Fit, error) {
	if len(points) < 2 {
		return nil, errors.New("calibration needs at least two concurrency levels")
	}
	pts := append([]BenchPoint(nil), points...)
	sort.Slice(pts, func(i, j int) bool { return pts[i].Concurrency < pts[j].Concurrency })
	fit := &Fit{}

	m := &Model{Kind: Kind, Name: o.Name}
	if o.Base != nil {
		c := *o.Base
		m = &c
		m.Decode = Decode{}
		m.curve = nil
		if o.Name != "" {
			m.Name = o.Name
		}
	}
	if m.Name == "" {
		m.Name = "calibrated"
	}

	// Decode curve: tpot(n) = a + b*n.
	var xs, ys []float64
	derived := 0
	for i := range pts {
		p := &pts[i]
		if p.TPOT <= 0 && p.Throughput > 0 {
			p.TPOT = p.Concurrency / p.Throughput
			derived++
		}
		if p.TPOT > 0 {
			xs = append(xs, p.Concurrency)
			ys = append(ys, p.TPOT)
		}
	}
	if len(xs) < 2 {
		return nil, errors.New("calibration needs TPOT (tpot_ms / itl_ms) or throughput for at least two concurrency levels")
	}
	if derived > 0 {
		fit.Notes = append(fit.Notes, fmt.Sprintf("TPOT was derived from throughput (concurrency / tokens per second) for %d point(s); that folds prefill time into the decode curve", derived))
	}
	a, b := nonNegLinear(xs, ys)
	if ua, ub := linear(xs, ys); ua < 0 || ub < 0 {
		fit.Notes = append(fit.Notes, "the unconstrained fit had a negative coefficient; it was refitted with both coefficients non-negative")
	}
	a, b = roundSig(a), roundSig(b)
	m.Decode.BaseSecondsPerToken, m.Decode.SecondsPerTokenPerRequest = a, b

	// Prefill: ttft(n) = T1 * (1 + c*(n-1)), with T1 = base + perToken * prompt.
	var tx, ty []float64
	promptSum, promptN := 0.0, 0
	for _, p := range pts {
		if p.TTFT > 0 {
			tx = append(tx, p.Concurrency)
			ty = append(ty, p.TTFT)
		}
		if p.PromptTokens > 0 {
			promptSum += p.PromptTokens
			promptN++
		}
	}
	prompt := o.PromptTokens
	if prompt <= 0 && promptN > 0 {
		prompt = promptSum / float64(promptN)
	}
	haveTTFT := len(tx) >= 2
	if haveTTFT {
		if prompt <= 0 {
			return nil, errors.New("the benchmark has TTFT but no prompt length: pass --prompt-tokens (the mean prompt length of the benchmark) so that TTFT can be turned into a per-token prefill cost")
		}
		alpha, beta := nonNegLinear(tx, ty)
		t1 := alpha + beta // TTFT of a request that runs alone
		if t1 <= 0 {
			return nil, errors.New("the fitted single-request TTFT is not positive")
		}
		base := o.PrefillBaseSeconds
		if base >= t1 {
			return nil, fmt.Errorf("--prefill-base %gs is not below the fitted single-request TTFT %.4gs", base, t1)
		}
		m.Prefill = Prefill{BaseSeconds: base, SecondsPerToken: roundSig((t1 - base) / prompt), Interference: roundSig(beta / t1)}
		fit.Notes = append(fit.Notes, fmt.Sprintf("prefill fitted for a mean prompt of %.0f tokens; one prompt length cannot separate the fixed part of TTFT from the per-token part, so prefill.baseSeconds is taken as given (%gs)", prompt, base))
	} else {
		fit.Notes = append(fit.Notes, "the benchmark has no TTFT for at least two concurrency levels: prefill was left as in the base model")
	}

	maxConc := o.MaxConcurrency
	if maxConc == 0 && o.Base != nil {
		maxConc = o.Base.MaxConcurrency
	}
	if maxConc == 0 {
		maxConc = int(math.Round(pts[len(pts)-1].Concurrency))
		fit.Notes = append(fit.Notes, fmt.Sprintf("maxConcurrency set to the highest benchmarked concurrency (%d); set it to the engine's batch limit (vLLM: --max-num-seqs)", maxConc))
	}
	m.MaxConcurrency = maxConc
	if top := pts[len(pts)-1].Concurrency; float64(maxConc) > top {
		fit.Notes = append(fit.Notes, fmt.Sprintf("maxConcurrency %d is beyond the highest benchmarked concurrency (%.0f): the curve is extrapolated there", maxConc, top))
	}
	if err := m.Resolve(""); err != nil {
		return nil, err
	}
	fit.Model = m

	var mT, fT, mF, fF, mX, fX []float64
	for _, p := range pts {
		row := FitRow{Concurrency: p.Concurrency, TPOT: p.TPOT, TTFT: p.TTFT, Throughput: p.Throughput}
		row.FittedTPOT = a + b*p.Concurrency
		row.FittedThroughpt = p.Concurrency / row.FittedTPOT
		if haveTTFT {
			row.FittedTTFT = m.PrefillSeconds(int(math.Round(prompt))) * (1 + m.Prefill.Interference*(p.Concurrency-1))
		}
		if p.TPOT > 0 {
			mT, fT = append(mT, p.TPOT), append(fT, row.FittedTPOT)
		}
		if p.TTFT > 0 && haveTTFT {
			mF, fF = append(mF, p.TTFT), append(fF, row.FittedTTFT)
		}
		if p.Throughput > 0 {
			mX, fX = append(mX, p.Throughput), append(fX, row.FittedThroughpt)
		}
		fit.Rows = append(fit.Rows, row)
	}
	fit.TPOT, fit.TTFT, fit.Throughput = quality(mT, fT), quality(mF, fF), quality(mX, fX)
	if len(xs) < 4 {
		fit.Notes = append(fit.Notes, fmt.Sprintf("only %d concurrency levels: a two-parameter fit to so few points says little about how well the curve holds elsewhere", len(xs)))
	}
	return fit, nil
}
