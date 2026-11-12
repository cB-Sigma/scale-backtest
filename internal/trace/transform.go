package trace

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
)

// NewRand returns the deterministic generator used everywhere in the
// project: the same seed always gives the same stream.
func NewRand(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
}

// ScaleOptions controls Scale.
type ScaleOptions struct {
	// TimeCompression divides every timestamp: 2 replays the trace in half
	// the time (twice the rate). Values below 1 stretch it. 0 means 1.
	TimeCompression float64
	// LoadMultiplier changes the number of requests without changing the
	// duration: above 1 requests are replicated with jitter, below 1 they
	// are thinned at random. 0 means 1.
	LoadMultiplier float64
	// JitterSeconds is the largest shift applied to a replicated request.
	JitterSeconds float64
	// TokenMultiplier scales prompt and output token counts. 0 means 1.
	TokenMultiplier float64
	Seed            uint64
}

// Scale returns a transformed copy of the trace.
func (t *Trace) Scale(o ScaleOptions) (*Trace, error) {
	if o.TimeCompression == 0 {
		o.TimeCompression = 1
	}
	if o.LoadMultiplier == 0 {
		o.LoadMultiplier = 1
	}
	if o.TokenMultiplier == 0 {
		o.TokenMultiplier = 1
	}
	if o.TimeCompression < 0 || o.LoadMultiplier < 0 || o.TokenMultiplier < 0 || o.JitterSeconds < 0 {
		return nil, errors.New("scale factors must not be negative")
	}
	rng := NewRand(o.Seed)
	out := t.Clone()
	out.Requests = out.Requests[:0]
	out.Duration = t.Duration / o.TimeCompression
	whole := int(math.Floor(o.LoadMultiplier))
	frac := o.LoadMultiplier - float64(whole)
	for _, r := range t.Requests {
		copies := whole
		if frac > 0 && rng.Float64() < frac {
			copies++
		}
		for c := range copies {
			n := r
			n.Arrival = r.Arrival / o.TimeCompression
			if c > 0 && o.JitterSeconds > 0 {
				n.Arrival += (rng.Float64()*2 - 1) * o.JitterSeconds
				n.Arrival = math.Min(math.Max(n.Arrival, 0), out.Duration)
			}
			if o.TokenMultiplier != 1 {
				n.PromptTokens = int(math.Round(float64(r.PromptTokens) * o.TokenMultiplier))
				n.OutputTokens = max(1, int(math.Round(float64(r.OutputTokens)*o.TokenMultiplier)))
			}
			out.Requests = append(out.Requests, n)
		}
	}
	out.Notes = append(out.Notes, fmt.Sprintf("scaled: time compression %g, load multiplier %g, token multiplier %g, seed %d",
		o.TimeCompression, o.LoadMultiplier, o.TokenMultiplier, o.Seed))
	out.normalise()
	return out, nil
}

// SliceOptions controls Slice.
type SliceOptions struct {
	// From and To bound the kept interval in seconds from the trace start.
	// To <= 0 means the end of the trace.
	From, To float64
	// Model and Tenant, when set, keep only matching requests.
	Model, Tenant string
}

// Slice returns the part of the trace inside the interval, re-based so that
// From becomes time 0.
func (t *Trace) Slice(o SliceOptions) (*Trace, error) {
	to := o.To
	if to <= 0 || to > t.Duration {
		to = t.Duration
	}
	if o.From < 0 || o.From >= to {
		return nil, fmt.Errorf("slice: empty interval [%g, %g) in a trace of %g seconds", o.From, to, t.Duration)
	}
	out := t.Clone()
	out.Requests = out.Requests[:0]
	for _, r := range t.Requests {
		if r.Arrival < o.From || r.Arrival > to {
			continue
		}
		if o.Model != "" && r.Model != o.Model {
			continue
		}
		if o.Tenant != "" && r.Tenant != o.Tenant {
			continue
		}
		r.Arrival -= o.From
		out.Requests = append(out.Requests, r)
	}
	out.Duration = to - o.From
	if !out.Start.IsZero() {
		out.Start = out.Start.Add(secondsToDuration(o.From))
	}
	out.normalise()
	return out, nil
}

// RatePoint is one point of a piecewise-linear arrival-rate profile.
type RatePoint struct {
	T    float64 // seconds
	Rate float64 // requests per second
}

// ParseProfile parses "0:1,10m:1,12m:6" into rate points. Times accept Go
// durations or plain seconds.
func ParseProfile(s string, parseDur func(string) (float64, error)) ([]RatePoint, error) {
	var pts []RatePoint
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		at, rate, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("profile point %q: expected time:rate", part)
		}
		ts, err := parseDur(strings.TrimSpace(at))
		if err != nil {
			return nil, fmt.Errorf("profile point %q: %w", part, err)
		}
		r, err := strconv.ParseFloat(strings.TrimSpace(rate), 64)
		if err != nil || r < 0 {
			return nil, fmt.Errorf("profile point %q: bad rate", part)
		}
		if len(pts) > 0 && ts <= pts[len(pts)-1].T {
			return nil, fmt.Errorf("profile point %q: times must increase", part)
		}
		pts = append(pts, RatePoint{T: ts, Rate: r})
	}
	if len(pts) < 2 {
		return nil, errors.New("a profile needs at least two time:rate points")
	}
	return pts, nil
}

func rateAt(pts []RatePoint, t float64) float64 {
	if t <= pts[0].T {
		return pts[0].Rate
	}
	for i := 1; i < len(pts); i++ {
		if t <= pts[i].T {
			a, b := pts[i-1], pts[i]
			return a.Rate + (b.Rate-a.Rate)*(t-a.T)/(b.T-a.T)
		}
	}
	return pts[len(pts)-1].Rate
}

// SynthOptions controls Synthesize.
type SynthOptions struct {
	// Profile is the arrival-rate profile; the trace ends at the last point.
	Profile []RatePoint
	// PromptMean and OutputMean are the mean token counts; PromptCV and
	// OutputCV are the coefficients of variation of the log-normal token
	// distributions.
	PromptMean, OutputMean float64
	PromptCV, OutputCV     float64
	// MaxPrompt and MaxOutput cap the token counts (0: no cap).
	MaxPrompt, MaxOutput int
	Seed                 uint64
}

// LogNormal draws from a log-normal distribution with the given mean and
// coefficient of variation.
func LogNormal(rng *rand.Rand, mean, cv float64) float64 {
	if cv <= 0 {
		return mean
	}
	sigma2 := math.Log(1 + cv*cv)
	mu := math.Log(mean) - sigma2/2
	return math.Exp(mu + math.Sqrt(sigma2)*rng.NormFloat64())
}

func drawTokens(rng *rand.Rand, mean, cv float64, limit int) int {
	n := int(math.Round(LogNormal(rng, mean, cv)))
	if n < 1 {
		n = 1
	}
	if limit > 0 && n > limit {
		n = limit
	}
	return n
}

// Synthesize generates a synthetic trace: a non-homogeneous Poisson process
// following the rate profile, with log-normal token counts.
func Synthesize(o SynthOptions) (*Trace, error) {
	if len(o.Profile) < 2 {
		return nil, errors.New("synth: a profile with at least two points is required")
	}
	if o.PromptMean <= 0 || o.OutputMean <= 0 {
		return nil, errors.New("synth: token means must be positive")
	}
	rng := NewRand(o.Seed)
	end := o.Profile[len(o.Profile)-1].T
	peak := 0.0
	for _, p := range o.Profile {
		peak = math.Max(peak, p.Rate)
	}
	t := &Trace{Name: "synthetic", Format: "synthetic", Duration: end}
	if peak <= 0 {
		return nil, errors.New("synth: the profile has no positive rate")
	}
	// Thinning (Lewis and Shedler): candidates at the peak rate, each kept
	// with probability rate(t)/peak.
	now := 0.0
	for {
		now += rng.ExpFloat64() / peak
		if now >= end {
			break
		}
		if rng.Float64()*peak > rateAt(o.Profile, now) {
			continue
		}
		t.Requests = append(t.Requests, Request{
			Arrival:      math.Round(now*1000) / 1000,
			PromptTokens: drawTokens(rng, o.PromptMean, o.PromptCV, o.MaxPrompt),
			OutputTokens: drawTokens(rng, o.OutputMean, o.OutputCV, o.MaxOutput),
		})
	}
	t.Notes = append(t.Notes, fmt.Sprintf("synthetic trace, seed %d", o.Seed))
	t.normalise()
	t.Duration = end
	return t, nil
}
