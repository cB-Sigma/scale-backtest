package cli

import (
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/cb-Sigma/scale-backtest/internal/hpa"
	"github.com/cb-Sigma/scale-backtest/internal/metrics"
	"github.com/cb-Sigma/scale-backtest/internal/policy"
	"github.com/cb-Sigma/scale-backtest/internal/replica"
	"github.com/cb-Sigma/scale-backtest/internal/report"
	"github.com/cb-Sigma/scale-backtest/internal/sim"
)

func cmdRun(args []string, stdout, stderr io.Writer) (int, error) {
	fset := newFlagSet("run", stderr, "run --policy POLICY --trace TRACE --replica MODEL [flags]")
	var f simFlags
	f.register(fset)
	policyPath := fset.String("policy", "", "HPA or KEDA ScaledObject manifest, or static:N")
	timelineOut := fset.String("timeline-out", "", "write the replica timeline to this CSV file")
	eventsOut := fset.String("events-out", "", "write the scale events to this CSV file")
	requestsOut := fset.String("requests-out", "", "write per-request latencies to this CSV file")
	if pos, err := parseArgs(fset, args); err != nil {
		return ExitError, err
	} else if len(pos) > 0 {
		return ExitError, fmt.Errorf("unexpected argument %q", pos[0])
	}
	if f.demo && *policyPath == "" {
		*policyPath = demoPolicy
	}
	if *policyPath == "" {
		return ExitError, errors.New("--policy is required (or use --demo)")
	}
	in, err := f.prepare()
	if err != nil {
		return ExitError, err
	}
	p, err := loadPolicy(*policyPath, in.registry)
	if err != nil {
		return ExitError, err
	}
	cfg := in.config(p)
	cfg.KeepRequests = *requestsOut != ""
	res, err := sim.Run(cfg)
	if err != nil {
		return ExitError, err
	}
	run := report.NewRun(res, in.slo)
	if err := report.WriteRun(stdout, run, f.format, in.options()); err != nil {
		return ExitError, err
	}
	for path, write := range map[string]func(io.Writer, *sim.Result) error{
		*timelineOut: report.WriteTimelineCSV,
		*eventsOut:   report.WriteEventsCSV,
		*requestsOut: report.WriteRequestsCSV,
	} {
		if path == "" {
			continue
		}
		if err := writeFile(path, func(w io.Writer) error { return write(w, res) }); err != nil {
			return ExitError, err
		}
	}
	if run.Pass != nil && !*run.Pass {
		return ExitSLOFail, nil
	}
	return ExitOK, nil
}

func cmdCompare(args []string, stdout, stderr io.Writer) (int, error) {
	fset := newFlagSet("compare", stderr, "compare --policy A --policy B [...] --trace TRACE --replica MODEL [flags]")
	var f simFlags
	f.register(fset)
	var policies multi
	fset.Var(&policies, "policy", "policy to compare: a manifest or static:N (repeatable)")
	if pos, err := parseArgs(fset, args); err != nil {
		return ExitError, err
	} else if len(pos) > 0 {
		policies = append(policies, pos...)
	}
	if f.demo && len(policies) == 0 {
		policies = demoComparePolicies
	}
	if len(policies) < 1 {
		return ExitError, errors.New("give at least one --policy (or use --demo)")
	}
	in, err := f.prepare()
	if err != nil {
		return ExitError, err
	}
	var runs []*report.Run
	allPass := true
	for _, spec := range policies {
		p, err := loadPolicy(spec, in.registry)
		if err != nil {
			return ExitError, err
		}
		res, err := sim.Run(in.config(p))
		if err != nil {
			return ExitError, fmt.Errorf("%s: %w", spec, err)
		}
		run := report.NewRun(res, in.slo)
		if run.Pass != nil && !*run.Pass {
			allPass = false
		}
		runs = append(runs, run)
	}
	if err := report.WriteCompare(stdout, runs, f.format, in.options()); err != nil {
		return ExitError, err
	}
	if !allPass {
		return ExitSLOFail, nil
	}
	return ExitOK, nil
}

type sweepParam struct {
	name   string
	values []float64
}

func parseSweepParam(s string) (sweepParam, error) {
	name, spec, ok := strings.Cut(s, "=")
	if !ok || name == "" || spec == "" {
		return sweepParam{}, fmt.Errorf("--param %q: expected name=start:end:step or a comma-separated list name=v1,v2", s)
	}
	p := sweepParam{name: strings.TrimSpace(name)}
	if strings.Contains(spec, ":") {
		parts := strings.Split(spec, ":")
		if len(parts) != 3 {
			return p, fmt.Errorf("--param %q: a range is start:end:step", s)
		}
		var v [3]float64
		for i, part := range parts {
			f, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
			if err != nil {
				return p, fmt.Errorf("--param %q: bad number %q", s, part)
			}
			v[i] = f
		}
		if v[2] <= 0 || v[1] < v[0] {
			return p, fmt.Errorf("--param %q: need start <= end and a positive step", s)
		}
		n := int(math.Floor((v[1]-v[0])/v[2]+1e-9)) + 1
		if n > 200 {
			return p, fmt.Errorf("--param %q: %d values is more than the limit of 200", s, n)
		}
		for i := range n {
			p.values = append(p.values, math.Round((v[0]+float64(i)*v[2])*1e9)/1e9)
		}
		return p, nil
	}
	for _, part := range strings.Split(spec, ",") {
		f, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return p, fmt.Errorf("--param %q: bad number %q", s, part)
		}
		p.values = append(p.values, f)
	}
	return p, nil
}

func cmdSweep(args []string, stdout, stderr io.Writer) (int, error) {
	fset := newFlagSet("sweep", stderr, "sweep --policy POLICY --param name=start:end:step [--param ...] --trace TRACE --replica MODEL [flags]")
	var f simFlags
	f.register(fset)
	policyPath := fset.String("policy", "", "policy to vary: a manifest or static:N")
	var params multi
	fset.Var(&params, "param", "parameter range, name=start:end:step or name=v1,v2,... (repeatable; the grid is swept). Names: "+strings.Join(policy.Params, ", "))
	if pos, err := parseArgs(fset, args); err != nil {
		return ExitError, err
	} else if len(pos) > 0 {
		return ExitError, fmt.Errorf("unexpected argument %q", pos[0])
	}
	if f.demo {
		if *policyPath == "" {
			*policyPath = demoSweepPolicy
		}
		if len(params) == 0 {
			params = multi{"target=4:16:4", "minReplicas=1,2,3"}
		}
	}
	if *policyPath == "" || len(params) == 0 {
		return ExitError, errors.New("--policy and at least one --param are required (or use --demo)")
	}
	var grid []sweepParam
	total := 1
	for _, s := range params {
		sp, err := parseSweepParam(s)
		if err != nil {
			return ExitError, err
		}
		grid = append(grid, sp)
		total *= len(sp.values)
	}
	if total > 2000 {
		return ExitError, fmt.Errorf("the sweep has %d combinations; the limit is 2000", total)
	}
	in, err := f.prepare()
	if err != nil {
		return ExitError, err
	}
	base, err := loadPolicy(*policyPath, in.registry)
	if err != nil {
		return ExitError, err
	}

	var runs []*report.Run
	idx := make([]int, len(grid))
	for {
		p := base.Clone()
		vals := map[string]float64{}
		var applyErr error
		for i, sp := range grid {
			v := sp.values[idx[i]]
			vals[sp.name] = v
			if err := p.Set(sp.name, v); err != nil {
				applyErr = err
				break
			}
		}
		if applyErr != nil {
			// An invalid combination (for example min above max) is skipped,
			// not fatal, unless it is the parameter name that is wrong.
			if strings.Contains(applyErr.Error(), "unknown parameter") || strings.Contains(applyErr.Error(), "only exists") {
				return ExitError, applyErr
			}
			fmt.Fprintf(stderr, "skipping %v: %v\n", vals, applyErr)
		} else {
			res, err := sim.Run(in.config(p))
			if err != nil {
				return ExitError, err
			}
			run := report.NewRun(res, in.slo)
			run.Params = vals
			runs = append(runs, run)
		}
		// Next grid point.
		i := len(idx) - 1
		for ; i >= 0; i-- {
			idx[i]++
			if idx[i] < len(grid[i].values) {
				break
			}
			idx[i] = 0
		}
		if i < 0 {
			break
		}
	}
	if len(runs) == 0 {
		return ExitError, errors.New("no valid combination in the sweep")
	}
	if err := report.WriteSweep(stdout, runs, f.format, in.options()); err != nil {
		return ExitError, err
	}
	if in.slo != nil {
		for _, r := range runs {
			if r.Pass != nil && *r.Pass {
				return ExitOK, nil
			}
		}
		return ExitSLOFail, nil
	}
	return ExitOK, nil
}

func describeRules(w io.Writer, name string, r *hpa.ScalingRules) {
	window := "controller default (--hpa-downscale-stabilization)"
	if r.StabilizationWindowSeconds != nil {
		window = fmt.Sprintf("%ds", *r.StabilizationWindowSeconds)
	}
	fmt.Fprintf(w, "    %s: stabilizationWindow %s, selectPolicy %s", name, window, r.SelectPolicy)
	if r.Tolerance != nil {
		fmt.Fprintf(w, ", tolerance %g", *r.Tolerance)
	}
	fmt.Fprintln(w)
	for _, pol := range r.Policies {
		fmt.Fprintf(w, "      - %s %d per %ds\n", pol.Type, pol.Value, pol.PeriodSeconds)
	}
}

func cmdExplain(args []string, stdout, stderr io.Writer) error {
	fset := newFlagSet("explain", stderr, "explain --policy POLICY")
	policyPath := fset.String("policy", "", "HPA or KEDA ScaledObject manifest")
	var aliases multi
	fset.Var(&aliases, "metric-alias", "map a metric name to a known one (repeatable)")
	pos, err := parseArgs(fset, args)
	if err != nil {
		return err
	}
	if *policyPath == "" && len(pos) == 1 {
		*policyPath = pos[0]
	}
	if *policyPath == "" {
		return errors.New("--policy is required")
	}
	reg := metrics.NewRegistry()
	for _, a := range aliases {
		name, target, ok := strings.Cut(a, "=")
		if !ok {
			return fmt.Errorf("--metric-alias %q: expected name=known-name", a)
		}
		if err := reg.Alias(name, target); err != nil {
			return err
		}
	}
	p, err := loadPolicy(*policyPath, reg)
	if err != nil {
		return err
	}
	w := stdout
	fmt.Fprintf(w, "%s\n", p.Label())
	if p.Kind == policy.KindStatic {
		return nil
	}
	if k := p.KEDA; k != nil {
		fmt.Fprintf(w, "\nKEDA operator loop (0 <-> %d)\n", k.ActiveFloor())
		fmt.Fprintf(w, "  pollingInterval %gs, cooldownPeriod %gs, initialCooldownPeriod %gs\n", k.PollingInterval, k.CooldownPeriod, k.InitialCooldownPeriod)
		fmt.Fprintf(w, "  minReplicaCount %d, maxReplicaCount %d", k.MinReplicaCount, k.MaxReplicaCount)
		if k.IdleReplicaCount != nil {
			fmt.Fprintf(w, ", idleReplicaCount %d", *k.IdleReplicaCount)
		}
		fmt.Fprintln(w)
		if !k.ScalesToZero() {
			fmt.Fprintf(w, "  minReplicaCount >= 1: the scaler is always active and activation thresholds are ignored\n")
		}
		for i, t := range k.Triggers {
			fmt.Fprintf(w, "  trigger %s: %s\n", t.Name, p.Sources[i].Query.Raw)
			fmt.Fprintf(w, "    evaluated as %s over simulated metric %s\n", p.Sources[i].Query.String(), p.Sources[i].Ref.Series)
			fmt.Fprintf(w, "    threshold %g (%s), activationThreshold %g", t.Threshold, t.MetricType, t.ActivationThreshold)
			if t.UseCachedMetrics {
				fmt.Fprintf(w, ", useCachedMetrics")
			}
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "\nGenerated HPA %s (1 <-> N)\n", p.HPA.Name)
	} else {
		fmt.Fprintf(w, "\nHPA control loop\n")
	}
	fmt.Fprintf(w, "  minReplicas %d, maxReplicas %d\n", p.HPA.MinReplicas, p.HPA.MaxReplicas)
	for i, m := range p.HPA.Metrics {
		src := p.Sources[i]
		what := src.Ref.Series.String()
		if src.Ref.Rate {
			what = "rate of " + what
		}
		fmt.Fprintf(w, "  metric %s (%s): target %g <- simulated %s\n", m.Name, m.Kind, m.Target, what)
	}
	ctl, err := hpa.NewController(p.HPA, hpa.DefaultOptions())
	if err != nil {
		return err
	}
	if b := ctl.Behavior(); b != nil {
		fmt.Fprintf(w, "  behavior (after API defaulting)\n")
		describeRules(w, "scaleUp", b.ScaleUp)
		describeRules(w, "scaleDown", b.ScaleDown)
	} else {
		fmt.Fprintf(w, "  behavior: not set; the controller's built-in rules apply:\n")
		fmt.Fprintf(w, "    scale up to at most max(2 x current, 4) replicas per sync\n")
		fmt.Fprintf(w, "    scale down to the highest recommendation of the last 300s (--hpa-downscale-stabilization)\n")
	}
	tol := ctl.Tolerances()
	fmt.Fprintf(w, "  tolerance: scale up %g, scale down %g; sync period 15s (controller defaults unless overridden by flags)\n", tol.ScaleUp, tol.ScaleDown)
	if len(p.Warnings) > 0 {
		fmt.Fprintln(w)
		for _, wn := range p.Warnings {
			fmt.Fprintf(w, "warning: %s\n", wn)
		}
	}
	return nil
}

func cmdMetrics(stdout io.Writer) {
	fmt.Fprintf(stdout, "Simulated series\n")
	var rows [][2]string
	for s := metrics.Series(0); s < metrics.NumSeries; s++ {
		d := s.Describe()
		kind := "gauge"
		if d.Counter {
			kind = "counter"
		}
		scope := "per replica"
		if d.Gateway {
			scope = "gateway"
		}
		rows = append(rows, [2]string{d.Canonical, fmt.Sprintf("%s, %s: %s", kind, scope, d.Description)})
	}
	for _, r := range rows {
		fmt.Fprintf(stdout, "  %-28s %s\n", r[0], r[1])
	}
	fmt.Fprintf(stdout, "\nAccepted names\n")
	aliases := metrics.Builtin()
	sort.SliceStable(aliases, func(i, j int) bool { return aliases[i].Ref.Series < aliases[j].Ref.Series })
	for _, a := range aliases {
		target := a.Ref.Series.String()
		if a.Ref.Rate {
			target = "rate(" + target + ")"
		}
		note := ""
		if a.Note != "" {
			note = "  (" + a.Note + ")"
		}
		fmt.Fprintf(stdout, "  %-36s -> %s%s\n", a.Name, target, note)
	}
}

func cmdCalibrate(args []string, stdout, stderr io.Writer) error {
	fset := newFlagSet("calibrate", stderr, "calibrate --bench FILE [--base MODEL] [--doriki REPORT ...] [-o MODEL]")
	bench := fset.String("bench", "", "benchmark result: CSV (concurrency,ttft_ms,tpot_ms,throughput_tokens_per_s) or GuideLLM benchmarks.json")
	benchFormat := fset.String("bench-format", "auto", "benchmark format: auto, csv, guidellm")
	base := fset.String("base", "", "replica model to take everything else from (cold start, cost, queue)")
	maxConc := fset.Int("max-concurrency", 0, "batch limit of the engine (default: from --base, else the highest benchmarked concurrency)")
	promptTokens := fset.Float64("prompt-tokens", 0, "mean prompt length of the benchmark, if the file does not carry it")
	var prefillBase seconds
	fset.Var(&prefillBase, "prefill-base", "fixed part of time to first token (cannot be fitted from a single prompt length)")
	name := fset.String("name", "", "name of the resulting model")
	var doriki multi
	fset.Var(&doriki, "doriki", "doriki JSON report whose time to Ready becomes a cold-start sample (repeatable)")
	out := fset.String("o", "", "write the resulting replica model to this file (default: print it)")
	fset.StringVar(out, "output", "", "same as -o")
	if _, err := parseArgs(fset, args); err != nil {
		return err
	}
	if *bench == "" && len(doriki) == 0 {
		return errors.New("give --bench and/or --doriki")
	}
	var model *replica.Model
	if *base != "" {
		m, err := loadReplica(*base)
		if err != nil {
			return err
		}
		model = m
	}
	w := stdout
	if *bench != "" {
		pts, format, err := replica.LoadBenchmark(*bench, *benchFormat)
		if err != nil {
			return err
		}
		fit, err := replica.Calibrate(pts, replica.FitOptions{
			Base: model, MaxConcurrency: *maxConc, PromptTokens: *promptTokens,
			PrefillBaseSeconds: float64(prefillBase), Name: *name,
		})
		if err != nil {
			return err
		}
		model = fit.Model
		fmt.Fprintf(w, "Fitted %d points from %s (%s)\n\n", len(pts), *bench, format)
		fmt.Fprintf(w, "decode   tpot(n) = %.3f ms + %.4f ms x n   (peak throughput %.0f tokens/s at n=%d)\n",
			model.Decode.BaseSecondsPerToken*1000, model.Decode.SecondsPerTokenPerRequest*1000, model.PeakThroughput(), model.MaxConcurrency)
		fmt.Fprintf(w, "prefill  %.1f ms + %.4f ms per prompt token, x (1 + %.4f x (n-1))\n\n",
			model.Prefill.BaseSeconds*1000, model.Prefill.SecondsPerToken*1000, model.Prefill.Interference)
		fmt.Fprintf(w, "%11s  %10s %10s  %10s %10s  %10s %10s\n", "concurrency", "TPOT ms", "fitted", "TTFT ms", "fitted", "tokens/s", "fitted")
		cell := func(v float64, scale float64, prec int) string {
			if v <= 0 {
				return "-"
			}
			return strconv.FormatFloat(v*scale, 'f', prec, 64)
		}
		for _, r := range fit.Rows {
			fmt.Fprintf(w, "%11g  %10s %10s  %10s %10s  %10s %10s\n", r.Concurrency,
				cell(r.TPOT, 1000, 2), cell(r.FittedTPOT, 1000, 2), cell(r.TTFT, 1000, 1), cell(r.FittedTTFT, 1000, 1),
				cell(r.Throughput, 1, 0), cell(r.FittedThroughpt, 1, 0))
		}
		fmt.Fprintf(w, "\nGoodness of fit\n")
		q := func(name string, f replica.FitQuality, unit string, scale float64) {
			if f.N == 0 {
				return
			}
			fmt.Fprintf(w, "  %-11s R2 %.4f · RMSE %.3g %s · mean error %s · worst error %s (%d points)\n",
				name, f.R2, f.RMSE*scale, unit, report.Pct(f.MAPE), report.Pct(f.MaxAPE), f.N)
		}
		q("TPOT", fit.TPOT, "ms", 1000)
		q("TTFT", fit.TTFT, "ms", 1000)
		q("throughput", fit.Throughput, "tokens/s", 1)
		for _, n := range fit.Notes {
			fmt.Fprintf(w, "note: %s\n", n)
		}
		fmt.Fprintln(w)
	}
	if len(doriki) > 0 {
		if model == nil {
			return errors.New("--doriki needs --bench or --base to attach the cold start to")
		}
		var samples []replica.Duration
		fmt.Fprintf(w, "Cold start from %d doriki report(s)\n", len(doriki))
		for _, path := range doriki {
			rep, err := replica.LoadDorikiReport(path)
			if err != nil {
				return err
			}
			samples = append(samples, replica.Duration(rep.StartupSeconds))
			var phases []string
			for _, ph := range rep.Phases {
				if ph.Seconds >= 0.5 {
					phases = append(phases, fmt.Sprintf("%s %s", ph.Name, report.Dur(ph.Seconds)))
				}
			}
			fmt.Fprintf(w, "  %s: %s to Ready", path, report.Dur(rep.PendingToReadySeconds))
			if rep.NodeProvisionSeconds > 0 {
				fmt.Fprintf(w, " (%s of it node provisioning, left to node.provisionDelay)", report.Dur(rep.NodeProvisionSeconds))
			}
			fmt.Fprintf(w, " [%s]\n", strings.Join(phases, ", "))
		}
		model.ColdStart = replica.Dist{Samples: samples}
		if err := model.Resolve(""); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}
	data, err := model.Marshal()
	if err != nil {
		return err
	}
	if *out != "" {
		if err := writeFile(*out, func(w io.Writer) error { _, err := w.Write(data); return err }); err != nil {
			return err
		}
		fmt.Fprintf(w, "wrote %s\n", *out)
		return nil
	}
	fmt.Fprintf(w, "---\n%s", data)
	return nil
}
