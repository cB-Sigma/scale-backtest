package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/cb-Sigma/scale-backtest/internal/replica"
	"github.com/cb-Sigma/scale-backtest/internal/report"
	"github.com/cb-Sigma/scale-backtest/internal/trace"
)

const traceUsage = `Usage: scale-backtest trace <subcommand> [flags]

Subcommands:
  stats FILE                   burstiness, rates and token distributions
  scale FILE -o OUT            time compression and load multiplier
  slice FILE -o OUT            keep a time window, a model or a tenant
  convert FILE -o OUT          rewrite any supported format as native CSV
  synth -o OUT                 generate a synthetic trace from a rate profile
  from-prometheus -o OUT       synthesise an approximate trace from rate series
`

func cmdTrace(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, traceUsage)
		return errors.New("trace: a subcommand is required")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "stats":
		return traceStats(rest, stdout, stderr)
	case "scale":
		return traceScale(rest, stdout, stderr)
	case "slice", "convert":
		return traceSlice(sub, rest, stdout, stderr)
	case "synth":
		return traceSynth(rest, stdout, stderr)
	case "from-prometheus":
		return traceFromPrometheus(rest, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, traceUsage)
		return nil
	}
	fmt.Fprint(stderr, traceUsage)
	return fmt.Errorf("trace: unknown subcommand %q", sub)
}

func oneTrace(pos []string, format string) (*trace.Trace, error) {
	if len(pos) != 1 {
		return nil, errors.New("exactly one trace file is required")
	}
	return loadTrace(pos[0], format)
}

func writeTrace(t *trace.Trace, out string, stdout io.Writer) error {
	if out == "" || out == "-" {
		return t.WriteCSV(stdout)
	}
	if err := writeFile(out, t.WriteCSV); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s: %s requests over %s\n", out, report.Num(len(t.Requests)), report.Dur(t.Duration))
	return nil
}

func traceStats(args []string, stdout, stderr io.Writer) error {
	fset := newFlagSet("trace stats", stderr, "trace stats FILE [flags]")
	format := fset.String("format", "table", "output format: table or json")
	traceFormat := fset.String("trace-format", trace.FormatAuto, "trace format: auto, csv, jsonl, azure")
	width := fset.Int("width", 60, "width of the sparkline")
	pos, err := parseArgs(fset, args)
	if err != nil {
		return err
	}
	t, err := oneTrace(pos, *traceFormat)
	if err != nil {
		return err
	}
	s := t.ComputeStats()
	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	w := stdout
	fmt.Fprintf(w, "trace %s (%s)\n", t.Name, t.Format)
	if t.Approximate {
		fmt.Fprintf(w, "APPROXIMATE: %s\n", strings.Join(t.Notes, "; "))
	}
	if !t.Start.IsZero() {
		fmt.Fprintf(w, "  starts          %s\n", t.Start.Format(time.RFC3339))
	}
	fmt.Fprintf(w, "  requests        %s over %s\n", report.Num(s.Requests), report.Dur(s.DurationSeconds))
	fmt.Fprintf(w, "  arrival rate    mean %.2f req/s · peak %.2f (1s) / %.2f (10s) / %.2f (60s) · peak-to-mean %.2f (60s)\n",
		s.MeanRate, s.PeakRate1s, s.PeakRate10s, s.PeakRate60s, s.PeakToMean60s)
	fmt.Fprintf(w, "  burstiness      inter-arrival CV %.2f · index of dispersion %.2f (10s) / %.2f (60s)   [Poisson: 1 / 1 / 1]\n",
		s.InterarrivalCV, s.Dispersion10s, s.Dispersion60s)
	fmt.Fprintf(w, "  prompt tokens   mean %.0f · p50 %.0f · p90 %.0f · p99 %.0f · max %.0f   (%.0f tokens/s)\n",
		s.PromptTokens.Mean, s.PromptTokens.P50, s.PromptTokens.P90, s.PromptTokens.P99, s.PromptTokens.Max, s.PromptTokenRate)
	fmt.Fprintf(w, "  output tokens   mean %.0f · p50 %.0f · p90 %.0f · p99 %.0f · max %.0f   (%.0f tokens/s)\n",
		s.OutputTokens.Mean, s.OutputTokens.P50, s.OutputTokens.P90, s.OutputTokens.P99, s.OutputTokens.Max, s.OutputTokenRate)
	line, top := report.Sparkline(t.RateSeries(*width))
	fmt.Fprintf(w, "  rate            %s  0..%.1f req/s\n", line, top)
	for _, group := range []struct {
		name string
		m    map[string]int
	}{{"models", s.Models}, {"tenants", s.Tenants}} {
		if len(group.m) == 0 {
			continue
		}
		keys := make([]string, 0, len(group.m))
		for k := range group.m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if group.m[keys[i]] != group.m[keys[j]] {
				return group.m[keys[i]] > group.m[keys[j]]
			}
			return keys[i] < keys[j]
		})
		var parts []string
		for i, k := range keys {
			if i == 8 {
				parts = append(parts, fmt.Sprintf("... %d more", len(keys)-8))
				break
			}
			parts = append(parts, fmt.Sprintf("%s %s", k, report.Num(group.m[k])))
		}
		fmt.Fprintf(w, "  %-15s %s\n", group.name, strings.Join(parts, " · "))
	}
	return nil
}

func traceScale(args []string, stdout, stderr io.Writer) error {
	fset := newFlagSet("trace scale", stderr, "trace scale FILE -o OUT [flags]")
	out := fset.String("o", "", "output file (default: standard output)")
	traceFormat := fset.String("trace-format", trace.FormatAuto, "trace format: auto, csv, jsonl, azure")
	compress := fset.Float64("time-compress", 1, "divide every timestamp by this factor (2 = twice as fast)")
	load := fset.Float64("load", 1, "multiply the number of requests, keeping the duration")
	tokens := fset.Float64("tokens", 1, "multiply prompt and output token counts")
	var jitter seconds = 1
	fset.Var(&jitter, "jitter", "largest time shift of a replicated request")
	seed := fset.Uint64("seed", 1, "random seed")
	pos, err := parseArgs(fset, args)
	if err != nil {
		return err
	}
	t, err := oneTrace(pos, *traceFormat)
	if err != nil {
		return err
	}
	scaled, err := t.Scale(trace.ScaleOptions{TimeCompression: *compress, LoadMultiplier: *load, TokenMultiplier: *tokens, JitterSeconds: float64(jitter), Seed: *seed})
	if err != nil {
		return err
	}
	return writeTrace(scaled, *out, stdout)
}

func traceSlice(sub string, args []string, stdout, stderr io.Writer) error {
	fset := newFlagSet("trace "+sub, stderr, "trace "+sub+" FILE -o OUT [flags]")
	out := fset.String("o", "", "output file (default: standard output)")
	traceFormat := fset.String("trace-format", trace.FormatAuto, "trace format: auto, csv, jsonl, azure")
	var from, to seconds
	fset.Var(&from, "from", "start of the window, from the beginning of the trace (e.g. 10m)")
	fset.Var(&to, "to", "end of the window (0: the end of the trace)")
	model := fset.String("model", "", "keep only this model")
	tenant := fset.String("tenant", "", "keep only this tenant")
	pos, err := parseArgs(fset, args)
	if err != nil {
		return err
	}
	t, err := oneTrace(pos, *traceFormat)
	if err != nil {
		return err
	}
	sliced, err := t.Slice(trace.SliceOptions{From: float64(from), To: float64(to), Model: *model, Tenant: *tenant})
	if err != nil {
		return err
	}
	if len(sliced.Requests) == 0 {
		return errors.New("no requests left after slicing")
	}
	return writeTrace(sliced, *out, stdout)
}

func traceSynth(args []string, stdout, stderr io.Writer) error {
	fset := newFlagSet("trace synth", stderr, "trace synth -o OUT --profile 0:1,10m:1,12m:6,20m:6,22m:1 [flags]")
	out := fset.String("o", "", "output file (default: standard output)")
	profile := fset.String("profile", "", "arrival-rate profile as time:rate points, linearly interpolated; the last time is the trace length")
	promptMean := fset.Float64("prompt-mean", 600, "mean prompt tokens")
	outputMean := fset.Float64("output-mean", 250, "mean output tokens")
	promptCV := fset.Float64("prompt-cv", 1.0, "coefficient of variation of prompt tokens (log-normal)")
	outputCV := fset.Float64("output-cv", 0.8, "coefficient of variation of output tokens (log-normal)")
	maxPrompt := fset.Int("max-prompt", 8000, "cap on prompt tokens (0: none)")
	maxOutput := fset.Int("max-output", 2000, "cap on output tokens (0: none)")
	seed := fset.Uint64("seed", 1, "random seed")
	if _, err := parseArgs(fset, args); err != nil {
		return err
	}
	if *profile == "" {
		return errors.New("--profile is required")
	}
	pts, err := trace.ParseProfile(*profile, replica.ParseDuration)
	if err != nil {
		return err
	}
	t, err := trace.Synthesize(trace.SynthOptions{Profile: pts, PromptMean: *promptMean, OutputMean: *outputMean,
		PromptCV: *promptCV, OutputCV: *outputCV, MaxPrompt: *maxPrompt, MaxOutput: *maxOutput, Seed: *seed})
	if err != nil {
		return err
	}
	return writeTrace(t, *out, stdout)
}

func parseTime(s string) (time.Time, error) {
	if ts, err := time.Parse(time.RFC3339, s); err == nil {
		return ts, nil
	}
	var unix float64
	if _, err := fmt.Sscanf(s, "%g", &unix); err == nil && unix > 0 {
		sec := int64(unix)
		return time.Unix(sec, int64((unix-float64(sec))*1e9)), nil
	}
	return time.Time{}, fmt.Errorf("bad time %q (RFC 3339 or Unix seconds)", s)
}

func traceFromPrometheus(args []string, stdout, stderr io.Writer) error {
	fset := newFlagSet("trace from-prometheus", stderr, "trace from-prometheus -o OUT (--rate-json FILE | --url URL --rate-query Q --start T --end T) [flags]")
	out := fset.String("o", "", "output file (default: standard output)")
	rateJSON := fset.String("rate-json", "", "saved query_range response with the request rate in requests per second")
	promptJSON := fset.String("prompt-rate-json", "", "saved query_range response with the prompt-token rate in tokens per second")
	outputJSON := fset.String("output-rate-json", "", "saved query_range response with the output-token rate in tokens per second")
	url := fset.String("url", "", "Prometheus base URL to query instead of reading files")
	rateQuery := fset.String("rate-query", "", "PromQL for the request rate, e.g. sum(rate(vllm:request_success_total[1m]))")
	promptQuery := fset.String("prompt-rate-query", "", "PromQL for the prompt-token rate")
	outputQuery := fset.String("output-rate-query", "", "PromQL for the output-token rate")
	start := fset.String("start", "", "range start (RFC 3339 or Unix seconds)")
	end := fset.String("end", "", "range end (RFC 3339 or Unix seconds)")
	var step seconds = 60
	fset.Var(&step, "step", "query resolution step")
	promptMean := fset.Float64("prompt-mean", 600, "mean prompt tokens where there is no token-rate data")
	outputMean := fset.Float64("output-mean", 250, "mean output tokens where there is no token-rate data")
	tokenCV := fset.Float64("token-cv", 1.0, "coefficient of variation of token counts within a bucket")
	seed := fset.Uint64("seed", 1, "random seed")
	if _, err := parseArgs(fset, args); err != nil {
		return err
	}
	o := trace.CoarseOptions{PromptMean: *promptMean, OutputMean: *outputMean, TokenCV: *tokenCV, Seed: *seed}
	note := func(what string, n int) {
		if n > 1 {
			fmt.Fprintf(stderr, "note: the %s result has %d series; they were summed\n", what, n)
		}
	}
	switch {
	case *rateJSON != "":
		s, n, err := trace.LoadPrometheusMatrix(*rateJSON)
		if err != nil {
			return err
		}
		note("request-rate", n)
		o.Rate = s
		for _, opt := range []struct {
			path string
			dst  **trace.Series
			what string
		}{{*promptJSON, &o.PromptRate, "prompt-rate"}, {*outputJSON, &o.OutputRate, "output-rate"}} {
			if opt.path == "" {
				continue
			}
			s, n, err := trace.LoadPrometheusMatrix(opt.path)
			if err != nil {
				return err
			}
			note(opt.what, n)
			*opt.dst = &s
		}
	case *url != "":
		if *rateQuery == "" || *start == "" || *end == "" {
			return errors.New("--url needs --rate-query, --start and --end")
		}
		from, err := parseTime(*start)
		if err != nil {
			return err
		}
		to, err := parseTime(*end)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		client := &http.Client{Timeout: time.Minute}
		fetch := func(query, what string) (*trace.Series, error) {
			s, n, err := trace.FetchPrometheusRange(ctx, client, *url, query, from, to, time.Duration(float64(step)*float64(time.Second)))
			if err != nil {
				return nil, fmt.Errorf("%s query: %w", what, err)
			}
			note(what, n)
			return &s, nil
		}
		s, err := fetch(*rateQuery, "request-rate")
		if err != nil {
			return err
		}
		o.Rate = *s
		if *promptQuery != "" {
			if o.PromptRate, err = fetch(*promptQuery, "prompt-rate"); err != nil {
				return err
			}
		}
		if *outputQuery != "" {
			if o.OutputRate, err = fetch(*outputQuery, "output-rate"); err != nil {
				return err
			}
		}
	default:
		return errors.New("give --rate-json, or --url with --rate-query, --start and --end")
	}
	t, err := trace.FromRateSeries(o)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "note: %s\n", t.Notes[0])
	return writeTrace(t, *out, stdout)
}
