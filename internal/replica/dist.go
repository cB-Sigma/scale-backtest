package replica

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Duration is a number of seconds that can be written in YAML either as a
// number (seconds) or as a Go duration string such as "150s" or "2m30s".
type Duration float64

// Seconds returns the duration in seconds.
func (d Duration) Seconds() float64 { return float64(d) }

// UnmarshalJSON accepts a number of seconds or a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var f float64
	if err := json.Unmarshal(b, &f); err == nil {
		*d = Duration(f)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a number of seconds or a string such as \"90s\"")
	}
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes the duration as a string such as "150s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(FormatDuration(float64(d)))
}

// ParseDuration parses "90s", "2m", "1h30m" or a plain number of seconds.
func ParseDuration(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("bad duration %q (use for example 90s, 2m30s or a number of seconds)", s)
	}
	return d.Seconds(), nil
}

// FormatDuration renders seconds as a compact string such as "150s".
func FormatDuration(s float64) string {
	return strconv.FormatFloat(s, 'g', 6, 64) + "s"
}

// Dist is a distribution of durations. Exactly one field must be set.
type Dist struct {
	// Constant is a fixed duration.
	Constant *Duration `json:"constant,omitempty"`
	// Percentiles maps percentile names (p0..p100, or min/max) to
	// durations. Samples follow the piecewise-linear inverse CDF through
	// the given points; see docs/model.md for the tails.
	Percentiles map[string]Duration `json:"percentiles,omitempty"`
	// Samples is an empirical distribution: each draw picks one value.
	Samples []Duration `json:"samples,omitempty"`
	// Doriki lists doriki JSON reports (paths relative to the model file)
	// whose time-to-ready totals become the samples.
	Doriki []string `json:"doriki,omitempty"`

	points []cdfPoint // resolved percentiles
}

type cdfPoint struct{ q, v float64 }

// ConstantDist returns a constant distribution.
func ConstantDist(seconds float64) Dist {
	d := Duration(seconds)
	return Dist{Constant: &d}
}

// IsZero reports whether no field is set.
func (d *Dist) IsZero() bool {
	return d.Constant == nil && len(d.Percentiles) == 0 && len(d.Samples) == 0 && len(d.Doriki) == 0
}

func parsePercentileKey(k string) (float64, error) {
	switch strings.ToLower(k) {
	case "min":
		return 0, nil
	case "max":
		return 1, nil
	}
	rest, ok := strings.CutPrefix(strings.ToLower(k), "p")
	if !ok {
		return 0, fmt.Errorf("percentile key %q: expected p50, p99, min or max", k)
	}
	f, err := strconv.ParseFloat(rest, 64)
	if err != nil || f < 0 || f > 100 {
		return 0, fmt.Errorf("percentile key %q: expected a percentile between p0 and p100", k)
	}
	return f / 100, nil
}

// resolve validates the distribution and prepares it for sampling. baseDir
// is used to locate doriki reports.
func (d *Dist) resolve(what, baseDir string) error {
	set := 0
	if d.Constant != nil {
		set++
	}
	if len(d.Percentiles) > 0 {
		set++
	}
	if len(d.Samples) > 0 {
		set++
	}
	if len(d.Doriki) > 0 {
		set++
	}
	if set != 1 {
		return fmt.Errorf("%s: set exactly one of constant, percentiles, samples, doriki", what)
	}
	switch {
	case d.Constant != nil:
		if *d.Constant < 0 {
			return fmt.Errorf("%s: constant must not be negative", what)
		}
	case len(d.Samples) > 0:
		for _, s := range d.Samples {
			if s < 0 {
				return fmt.Errorf("%s: samples must not be negative", what)
			}
		}
	case len(d.Doriki) > 0:
		for _, p := range d.Doriki {
			rep, err := LoadDorikiReport(resolvePath(baseDir, p))
			if err != nil {
				return fmt.Errorf("%s: %w", what, err)
			}
			d.Samples = append(d.Samples, Duration(rep.StartupSeconds))
		}
	default:
		pts := make([]cdfPoint, 0, len(d.Percentiles)+2)
		for k, v := range d.Percentiles {
			q, err := parsePercentileKey(k)
			if err != nil {
				return fmt.Errorf("%s: %w", what, err)
			}
			if v < 0 {
				return fmt.Errorf("%s: %s must not be negative", what, k)
			}
			pts = append(pts, cdfPoint{q, float64(v)})
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i].q < pts[j].q })
		for i := 1; i < len(pts); i++ {
			if pts[i].q == pts[i-1].q {
				return fmt.Errorf("%s: percentile given twice", what)
			}
			if pts[i].v < pts[i-1].v {
				return fmt.Errorf("%s: percentile values must not decrease", what)
			}
		}
		// Tails: without an explicit minimum the lower tail mirrors the
		// distance to the next point (never below half the lowest value);
		// without an explicit maximum the highest given value is the cap.
		if pts[0].q > 0 {
			low := pts[0].v
			if len(pts) > 1 {
				low = pts[0].v - (pts[1].v - pts[0].v)
			}
			if low < pts[0].v/2 {
				low = pts[0].v / 2
			}
			pts = append([]cdfPoint{{0, low}}, pts...)
		}
		if last := pts[len(pts)-1]; last.q < 1 {
			pts = append(pts, cdfPoint{1, last.v})
		}
		d.points = pts
	}
	return nil
}

// Sample draws one duration in seconds.
func (d *Dist) Sample(rng *rand.Rand) float64 {
	switch {
	case d.Constant != nil:
		return float64(*d.Constant)
	case len(d.Samples) > 0:
		return float64(d.Samples[rng.IntN(len(d.Samples))])
	case len(d.points) > 0:
		u := rng.Float64()
		for i := 1; i < len(d.points); i++ {
			if u <= d.points[i].q {
				a, b := d.points[i-1], d.points[i]
				if b.q == a.q {
					return b.v
				}
				return a.v + (b.v-a.v)*(u-a.q)/(b.q-a.q)
			}
		}
		return d.points[len(d.points)-1].v
	}
	return 0
}

// Mean returns the mean of the distribution in seconds.
func (d *Dist) Mean() float64 {
	switch {
	case d.Constant != nil:
		return float64(*d.Constant)
	case len(d.Samples) > 0:
		sum := 0.0
		for _, s := range d.Samples {
			sum += float64(s)
		}
		return sum / float64(len(d.Samples))
	case len(d.points) > 0:
		mean := 0.0
		for i := 1; i < len(d.points); i++ {
			a, b := d.points[i-1], d.points[i]
			mean += (b.q - a.q) * (a.v + b.v) / 2
		}
		return mean
	}
	return 0
}

// Describe returns a one-line description.
func (d *Dist) Describe() string {
	switch {
	case d.Constant != nil:
		return "constant " + FormatDuration(float64(*d.Constant))
	case len(d.Doriki) > 0:
		return fmt.Sprintf("%d doriki report(s), mean %s", len(d.Doriki), FormatDuration(d.Mean()))
	case len(d.Samples) > 0:
		return fmt.Sprintf("%d samples, mean %s", len(d.Samples), FormatDuration(d.Mean()))
	case len(d.Percentiles) > 0:
		keys := make([]string, 0, len(d.Percentiles))
		for k := range d.Percentiles {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			a, _ := parsePercentileKey(keys[i])
			b, _ := parsePercentileKey(keys[j])
			return a < b
		})
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+FormatDuration(float64(d.Percentiles[k])))
		}
		return "percentiles " + strings.Join(parts, " ") + ", mean " + FormatDuration(d.Mean())
	}
	return "unset"
}
