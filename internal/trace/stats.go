package trace

import (
	"math"
	"sort"
)

// Dist summarises a distribution of non-negative values.
type Dist struct {
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
}

// Stats summarises a trace.
type Stats struct {
	Requests        int     `json:"requests"`
	DurationSeconds float64 `json:"duration_seconds"`
	MeanRate        float64 `json:"mean_rate_rps"`
	// PeakRate1s, PeakRate10s and PeakRate60s are the highest arrival rates
	// seen over any aligned window of that length.
	PeakRate1s  float64 `json:"peak_rate_1s_rps"`
	PeakRate10s float64 `json:"peak_rate_10s_rps"`
	PeakRate60s float64 `json:"peak_rate_60s_rps"`
	// PeakToMean60s is PeakRate60s / MeanRate.
	PeakToMean60s float64 `json:"peak_to_mean_60s"`
	// InterarrivalCV is the coefficient of variation of inter-arrival times
	// (1 for a Poisson process, higher for bursty traffic).
	InterarrivalCV float64 `json:"interarrival_cv"`
	// Dispersion10s and Dispersion60s are the index of dispersion
	// (variance / mean) of arrival counts in windows of that length (1 for a
	// Poisson process).
	Dispersion10s float64 `json:"dispersion_10s"`
	Dispersion60s float64 `json:"dispersion_60s"`

	PromptTokens Dist `json:"prompt_tokens"`
	OutputTokens Dist `json:"output_tokens"`
	// TokenRate is the mean output-token arrival rate in tokens per second.
	OutputTokenRate float64 `json:"output_token_rate_tps"`
	PromptTokenRate float64 `json:"prompt_token_rate_tps"`

	Models  map[string]int `json:"models,omitempty"`
	Tenants map[string]int `json:"tenants,omitempty"`

	Approximate bool `json:"approximate,omitempty"`
}

// Quantile returns the q-quantile (0..1) of sorted values using linear
// interpolation between closest ranks.
func Quantile(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[n-1]
	}
	pos := q * float64(n-1)
	lo := int(math.Floor(pos))
	frac := pos - float64(lo)
	if lo+1 >= n {
		return sorted[n-1]
	}
	return sorted[lo]*(1-frac) + sorted[lo+1]*frac
}

func distOf(vals []float64) Dist {
	if len(vals) == 0 {
		return Dist{}
	}
	sort.Float64s(vals)
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return Dist{
		Mean: sum / float64(len(vals)),
		P50:  Quantile(vals, 0.50),
		P90:  Quantile(vals, 0.90),
		P99:  Quantile(vals, 0.99),
		Max:  vals[len(vals)-1],
	}
}

// windowCounts counts arrivals in aligned windows of the given width.
func (t *Trace) windowCounts(width float64) []float64 {
	if t.Duration <= 0 || width <= 0 {
		return []float64{float64(len(t.Requests))}
	}
	n := int(math.Ceil(t.Duration / width))
	if n < 1 {
		n = 1
	}
	counts := make([]float64, n)
	for _, r := range t.Requests {
		i := int(r.Arrival / width)
		if i >= n {
			i = n - 1
		}
		counts[i]++
	}
	return counts
}

func peakAndDispersion(counts []float64, width float64) (peak, dispersion float64) {
	mean := 0.0
	for _, c := range counts {
		mean += c
		if c > peak {
			peak = c
		}
	}
	mean /= float64(len(counts))
	variance := 0.0
	for _, c := range counts {
		variance += (c - mean) * (c - mean)
	}
	if len(counts) > 1 {
		variance /= float64(len(counts) - 1)
	}
	if mean > 0 {
		dispersion = variance / mean
	}
	return peak / width, dispersion
}

// RateSeries returns the arrival rate (requests per second) in n equal
// buckets across the trace.
func (t *Trace) RateSeries(n int) []float64 {
	if n < 1 || t.Duration <= 0 {
		return nil
	}
	width := t.Duration / float64(n)
	out := make([]float64, n)
	for _, r := range t.Requests {
		i := int(r.Arrival / width)
		if i >= n {
			i = n - 1
		}
		out[i]++
	}
	for i := range out {
		out[i] /= width
	}
	return out
}

// ComputeStats summarises the trace.
func (t *Trace) ComputeStats() Stats {
	s := Stats{Requests: len(t.Requests), DurationSeconds: t.Duration, Approximate: t.Approximate}
	if len(t.Requests) == 0 {
		return s
	}
	if t.Duration > 0 {
		s.MeanRate = float64(len(t.Requests)) / t.Duration
	}
	s.PeakRate1s, _ = peakAndDispersion(t.windowCounts(1), 1)
	s.PeakRate10s, s.Dispersion10s = peakAndDispersion(t.windowCounts(10), 10)
	s.PeakRate60s, s.Dispersion60s = peakAndDispersion(t.windowCounts(60), 60)
	if t.Duration < 60 {
		s.PeakRate60s = s.MeanRate
	}
	if t.Duration < 10 {
		s.PeakRate10s = s.MeanRate
	}
	if s.MeanRate > 0 {
		s.PeakToMean60s = s.PeakRate60s / s.MeanRate
	}

	if n := len(t.Requests); n > 2 {
		gaps := make([]float64, 0, n-1)
		sum := 0.0
		for i := 1; i < n; i++ {
			g := t.Requests[i].Arrival - t.Requests[i-1].Arrival
			gaps = append(gaps, g)
			sum += g
		}
		mean := sum / float64(len(gaps))
		variance := 0.0
		for _, g := range gaps {
			variance += (g - mean) * (g - mean)
		}
		variance /= float64(len(gaps) - 1)
		if mean > 0 {
			s.InterarrivalCV = math.Sqrt(variance) / mean
		}
	}

	prompts := make([]float64, len(t.Requests))
	outputs := make([]float64, len(t.Requests))
	var pSum, oSum float64
	for i, r := range t.Requests {
		prompts[i], outputs[i] = float64(r.PromptTokens), float64(r.OutputTokens)
		pSum += prompts[i]
		oSum += outputs[i]
		if r.Model != "" {
			if s.Models == nil {
				s.Models = map[string]int{}
			}
			s.Models[r.Model]++
		}
		if r.Tenant != "" {
			if s.Tenants == nil {
				s.Tenants = map[string]int{}
			}
			s.Tenants[r.Tenant]++
		}
	}
	s.PromptTokens, s.OutputTokens = distOf(prompts), distOf(outputs)
	if t.Duration > 0 {
		s.PromptTokenRate, s.OutputTokenRate = pSum/t.Duration, oSum/t.Duration
	}
	return s
}
