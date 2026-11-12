package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/cb-Sigma/scale-backtest/internal/sim"
)

// Formats.
const (
	FormatTable    = "table"
	FormatJSON     = "json"
	FormatMarkdown = "markdown"
)

// ValidFormat reports whether f is a known output format.
func ValidFormat(f string) bool {
	return f == FormatTable || f == FormatJSON || f == FormatMarkdown
}

// Run is one simulated policy with its SLO verdict.
type Run struct {
	Result *sim.Result `json:"result"`
	// Checks and Pass are set when an SLO was given.
	Checks []Check `json:"slo_checks,omitempty"`
	Pass   *bool   `json:"slo_pass,omitempty"`
	// Params holds the swept parameter values of this run, if any.
	Params map[string]float64 `json:"params,omitempty"`
	// Frontier marks runs on the cost-versus-attainment frontier of a sweep.
	Frontier bool `json:"on_frontier,omitempty"`
}

// NewRun evaluates the objectives (if any) against a result.
func NewRun(res *sim.Result, objs []Objective) *Run {
	r := &Run{Result: res}
	if len(objs) > 0 {
		checks, pass := Evaluate(objs, res)
		r.Checks, r.Pass = checks, &pass
	}
	return r
}

// Options tunes text rendering.
type Options struct {
	// Width is the width of sparklines (default 60).
	Width int
	// MaxEvents limits the scale events listed (0: all).
	MaxEvents int
	// Version is printed in the JSON envelope.
	Version string
}

func (o Options) width() int {
	if o.Width <= 0 {
		return 60
	}
	return o.Width
}

func verdict(r *Run) string {
	if r.Pass == nil {
		return ""
	}
	if *r.Pass {
		return "PASS"
	}
	return "FAIL"
}

func dropSummary(res *sim.Result) string {
	if res.Dropped == 0 {
		return "0 dropped"
	}
	parts := make([]string, 0, len(res.DropReasons))
	for _, k := range sortedKeys(res.DropReasons) {
		parts = append(parts, fmt.Sprintf("%s %s", k, Num(res.DropReasons[k])))
	}
	return fmt.Sprintf("%s dropped (%s)", Num(res.Dropped), strings.Join(parts, ", "))
}

func header(res *sim.Result) string {
	return fmt.Sprintf("%s · trace %s (%s requests over %s) · replica model %s",
		res.Policy, res.Trace, Num(res.Requests), Dur(res.TraceDurationSeconds), res.Replica)
}

func latencyRows(res *sim.Result) [][]string {
	row := func(name string, p sim.Percentiles) []string {
		return []string{name, Dur(p.Mean), Dur(p.P50), Dur(p.P90), Dur(p.P95), Dur(p.P99), Dur(p.Max)}
	}
	return [][]string{
		{"", "mean", "p50", "p90", "p95", "p99", "max"},
		row("TTFT", res.TTFT),
		row("End-to-end", res.E2E),
		row("Queue wait", res.Queue),
	}
}

func targetRows(res *sim.Result) [][]string {
	var rows [][]string
	over := func(share float64) string {
		n := res.Requests - int(float64(res.Requests)*share+0.5)
		return Num(n) + " missed"
	}
	if res.TTFTAttainment != nil {
		rows = append(rows, []string{"TTFT <= " + Dur(res.TTFTTargetSeconds), Pct(*res.TTFTAttainment), over(*res.TTFTAttainment)})
	}
	if res.E2EAttainment != nil {
		rows = append(rows, []string{"End-to-end <= " + Dur(res.E2ETargetSeconds), Pct(*res.E2EAttainment), over(*res.E2EAttainment)})
	}
	if len(rows) == 2 {
		rows = append(rows, []string{"Both", Pct(res.Attainment), over(res.Attainment)})
	}
	return rows
}

func timelineSeries(res *sim.Result, width int) (ready, arrivals, waiting []float64) {
	n := len(res.Timeline)
	ts := make([]float64, n)
	rv := make([]float64, n)
	av := make([]float64, n)
	wv := make([]float64, n)
	for i, p := range res.Timeline {
		ts[i] = p.T
		rv[i] = float64(p.Ready)
		av[i] = p.ArrivalRate
		wv[i] = float64(p.Waiting + p.GatewayPending)
	}
	width = min(width, max(n, 1))
	d := res.DurationSeconds
	return bucketise(ts, rv, d, width, "max"), bucketise(ts, av, d, width, "mean"), bucketise(ts, wv, d, width, "max")
}

func eventRows(res *sim.Result, limit int) (rows [][]string, shown int) {
	events := res.Events
	if limit > 0 && len(events) > limit {
		events = events[:limit]
	}
	for _, ev := range events {
		rows = append(rows, []string{Clock(ev.T), fmt.Sprintf("%d -> %d", ev.From, ev.To), ev.Source, ev.Reason})
		if ev.Detail != "" {
			rows = append(rows, []string{"", "", "", "  " + strings.TrimSuffix(strings.TrimSpace(ev.Detail), ";")})
		}
	}
	return rows, len(events)
}

// WriteRun renders one run.
func WriteRun(w io.Writer, r *Run, format string, o Options) error {
	switch format {
	case FormatJSON:
		return writeJSON(w, map[string]any{"version": o.Version, "runs": []*Run{r}})
	case FormatMarkdown:
		return writeRunMarkdown(w, r, o)
	}
	res := r.Result
	p := func(format string, args ...any) { fmt.Fprintf(w, format, args...) }

	p("%s\n", header(res))
	if res.Approximate {
		p("APPROXIMATE TRACE: arrivals were synthesised from aggregate series; treat tail latencies as rough.\n")
	}
	p("\n")
	if v := verdict(r); v != "" {
		p("Verdict   %s\n", v)
		var rows [][]string
		for _, c := range r.Checks {
			mark := "ok  "
			if !c.OK {
				mark = "FAIL"
			}
			rows = append(rows, []string{mark, c.Text, "actual " + c.FormatActual()})
		}
		p("%s\n", table(rows, nil, "  "))
	}

	p("Latency (completed requests, queueing included)\n")
	p("%s\n", table(latencyRows(res), map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true}, "  "))

	if rows := targetRows(res); len(rows) > 0 {
		p("Targets (all requests; a dropped request misses its targets)\n")
		p("%s\n", table(rows, map[int]bool{1: true, 2: true}, "  "))
	}

	p("Requests  %s completed · %s · %s arrived with no ready replica\n",
		Num(res.Completed), dropSummary(res), Num(res.ColdHits))
	rep := res.Replicas
	p("Replicas  start %d · ready min %d / mean %.2f / max %d · %d cold starts · %d scale events\n",
		rep.Initial, rep.MinReady, rep.MeanReady, rep.MaxReady, rep.ColdStarts, len(res.Events))
	ready, arrivals, waiting := timelineSeries(res, o.width())
	line, top := Sparkline(ready)
	p("  ready     %s  0..%.0f\n", line, top)
	line, top = Sparkline(arrivals)
	p("  arrivals  %s  0..%.1f req/s\n", line, top)
	line, top = Sparkline(waiting)
	p("  waiting   %s  0..%.0f requests\n", line, top)
	c := res.Cost
	p("Cost      %.2f GPU-hours · %s at $%.2f per GPU-hour · $%.2f per 1M output tokens\n",
		c.GPUHours, Money(c.Cost), c.GPUPricePerHour, c.PerMillionOutputTokens)
	p("          busy %s · idle %s · starting %s · mean batch fill %s\n\n",
		Pct(c.BusyShare), Pct(c.IdleShare), Pct(c.StartingShare), Pct(c.MeanUtilization))

	if len(res.Events) > 0 {
		rows, shown := eventRows(res, o.MaxEvents)
		if shown < len(res.Events) {
			p("Scale events (first %d of %d; --max-events 0 shows all)\n", shown, len(res.Events))
		} else {
			p("Scale events\n")
		}
		p("%s", table(rows, nil, "  "))
	} else {
		p("Scale events  none\n")
	}
	if len(res.Warnings)+len(res.Notes) > 0 {
		p("\n")
	}
	writeNotes(w, res)
	return nil
}

func writeNotes(w io.Writer, res *sim.Result) {
	for _, n := range res.Warnings {
		fmt.Fprintf(w, "warning: %s\n", n)
	}
	for _, n := range res.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
}

func writeRunMarkdown(w io.Writer, r *Run, o Options) error {
	res := r.Result
	p := func(format string, args ...any) { fmt.Fprintf(w, format, args...) }
	p("## %s\n\n", res.Policy)
	p("Trace `%s`: %s requests over %s. Replica model `%s`.\n\n", res.Trace, Num(res.Requests), Dur(res.TraceDurationSeconds), res.Replica)
	if res.Approximate {
		p("> **Approximate trace:** arrivals were synthesised from aggregate series; treat tail latencies as rough.\n\n")
	}
	if v := verdict(r); v != "" {
		p("**Verdict: %s**\n\n", v)
		rows := [][]string{{"objective", "actual", "result"}}
		for _, c := range r.Checks {
			res := "ok"
			if !c.OK {
				res = "**FAIL**"
			}
			rows = append(rows, []string{"`" + c.Text + "`", c.FormatActual(), res})
		}
		p("%s\n", mdTable(rows, map[int]bool{1: true}))
	}
	lat := latencyRows(res)
	lat[0][0] = "latency"
	p("%s\n", mdTable(lat, map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true}))
	if rows := targetRows(res); len(rows) > 0 {
		p("%s\n", mdTable(append([][]string{{"target", "attained", "missed"}}, rows...), map[int]bool{1: true, 2: true}))
	}
	c := res.Cost
	rows := [][]string{
		{"metric", "value"},
		{"completed", Num(res.Completed)},
		{"dropped", dropSummary(res)},
		{"arrived with no ready replica", Num(res.ColdHits)},
		{"ready replicas (min / mean / max)", fmt.Sprintf("%d / %.2f / %d", res.Replicas.MinReady, res.Replicas.MeanReady, res.Replicas.MaxReady)},
		{"cold starts", Num(res.Replicas.ColdStarts)},
		{"scale events", Num(len(res.Events))},
		{"GPU-hours", fmt.Sprintf("%.2f", c.GPUHours)},
		{"cost", fmt.Sprintf("%s at $%.2f per GPU-hour", Money(c.Cost), c.GPUPricePerHour)},
		{"cost per 1M output tokens", fmt.Sprintf("$%.2f", c.PerMillionOutputTokens)},
		{"busy / idle / starting", fmt.Sprintf("%s / %s / %s", Pct(c.BusyShare), Pct(c.IdleShare), Pct(c.StartingShare))},
	}
	p("%s\n", mdTable(rows, map[int]bool{1: true}))
	if len(res.Events) > 0 {
		evs := res.Events
		if o.MaxEvents > 0 && len(evs) > o.MaxEvents {
			evs = evs[:o.MaxEvents]
		}
		rows := [][]string{{"time", "change", "by", "reason"}}
		for _, ev := range evs {
			rows = append(rows, []string{Clock(ev.T), fmt.Sprintf("%d -> %d", ev.From, ev.To), ev.Source, ev.Reason})
		}
		p("%s\n", mdTable(rows, nil))
	}
	for _, n := range res.Warnings {
		p("- warning: %s\n", n)
	}
	for _, n := range res.Notes {
		p("- note: %s\n", n)
	}
	return nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func paramNames(runs []*Run) []string {
	seen := map[string]bool{}
	var names []string
	for _, r := range runs {
		for k := range r.Params {
			if !seen[k] {
				seen[k] = true
				names = append(names, k)
			}
		}
	}
	sort.Strings(names)
	return names
}

func summaryRows(runs []*Run, sweep bool) (rows [][]string, right map[int]bool) {
	params := paramNames(runs)
	anySLO := false
	for _, r := range runs {
		anySLO = anySLO || r.Pass != nil
	}
	var head []string
	if sweep {
		head = append(head, params...)
	} else {
		head = append(head, "policy")
	}
	first := len(head)
	head = append(head, "attained", "TTFT p50", "TTFT p95", "E2E p95", "dropped", "ready mean/max", "GPU-h", "cost", "idle")
	if anySLO {
		head = append(head, "SLO")
	}
	if sweep {
		head = append(head, "frontier")
	}
	rows = append(rows, head)
	right = map[int]bool{}
	for i := 0; i < len(head); i++ {
		if (sweep && i < first) || (i >= first && i < first+9) {
			right[i] = true
		}
	}
	for _, r := range runs {
		res := r.Result
		var row []string
		if sweep {
			for _, k := range params {
				row = append(row, strconv.FormatFloat(r.Params[k], 'g', 6, 64))
			}
		} else {
			row = append(row, res.Policy)
		}
		row = append(row,
			Pct(res.Attainment), Dur(res.TTFT.P50), Dur(res.TTFT.P95), Dur(res.E2E.P95),
			Num(res.Dropped), fmt.Sprintf("%.1f/%d", res.Replicas.MeanReady, res.Replicas.MaxReady),
			fmt.Sprintf("%.2f", res.Cost.GPUHours), Money(res.Cost.Cost), Pct(res.Cost.IdleShare))
		if anySLO {
			row = append(row, verdict(r))
		}
		if sweep {
			mark := ""
			if r.Frontier {
				mark = "*"
			}
			row = append(row, mark)
		}
		rows = append(rows, row)
	}
	return rows, right
}

func attainmentCaption(runs []*Run) string {
	if len(runs) == 0 {
		return ""
	}
	res := runs[0].Result
	var parts []string
	if res.TTFTTargetSeconds > 0 {
		parts = append(parts, "TTFT <= "+Dur(res.TTFTTargetSeconds))
	}
	if res.E2ETargetSeconds > 0 {
		parts = append(parts, "end-to-end <= "+Dur(res.E2ETargetSeconds))
	}
	if len(parts) == 0 {
		return "attained = share of requests completed (no latency target set)"
	}
	return "attained = share of all requests completed within " + strings.Join(parts, " and ")
}

// WriteCompare renders several policies side by side.
func WriteCompare(w io.Writer, runs []*Run, format string, o Options) error {
	if format == FormatJSON {
		return writeJSON(w, map[string]any{"version": o.Version, "runs": runs})
	}
	rows, right := summaryRows(runs, false)
	res := runs[0].Result
	if format == FormatMarkdown {
		fmt.Fprintf(w, "Trace `%s`: %s requests over %s. Replica model `%s`.\n\n", res.Trace, Num(res.Requests), Dur(res.TraceDurationSeconds), res.Replica)
		fmt.Fprint(w, mdTable(rows, right))
		fmt.Fprintf(w, "\n%s.\n", attainmentCaption(runs))
		return nil
	}
	fmt.Fprintf(w, "trace %s (%s requests over %s) · replica model %s\n", res.Trace, Num(res.Requests), Dur(res.TraceDurationSeconds), res.Replica)
	if res.Approximate {
		fmt.Fprintf(w, "APPROXIMATE TRACE: arrivals were synthesised from aggregate series; treat tail latencies as rough.\n")
	}
	fmt.Fprintf(w, "\n%s\n", table(rows, right, ""))
	fmt.Fprintf(w, "%s\n\n", attainmentCaption(runs))
	width := o.width()
	nameW := 0
	for _, r := range runs {
		nameW = max(nameW, len([]rune(r.Result.Policy)))
	}
	fmt.Fprintf(w, "ready replicas over time\n")
	for _, r := range runs {
		ready, _, _ := timelineSeries(r.Result, width)
		line, top := Sparkline(ready)
		fmt.Fprintf(w, "  %-*s  %s  0..%.0f\n", nameW, r.Result.Policy, line, top)
	}
	_, arrivals, _ := timelineSeries(runs[0].Result, width)
	line, top := Sparkline(arrivals)
	fmt.Fprintf(w, "  %-*s  %s  0..%.1f req/s\n", nameW, "arrivals", line, top)
	seen := map[string]bool{}
	for _, r := range runs {
		for _, n := range r.Result.Warnings {
			if key := r.Result.Policy + n; !seen[key] {
				seen[key] = true
				fmt.Fprintf(w, "warning: %s: %s\n", r.Result.Policy, n)
			}
		}
	}
	return nil
}

// MarkFrontier flags the runs that are not dominated: no other run has
// both lower-or-equal cost and higher-or-equal attainment with one of the
// two strictly better.
func MarkFrontier(runs []*Run) {
	for i, a := range runs {
		a.Frontier = true
		for j, b := range runs {
			if i == j {
				continue
			}
			ca, cb := a.Result.Cost.Cost, b.Result.Cost.Cost
			aa, ab := a.Result.Attainment, b.Result.Attainment
			if cb <= ca && ab >= aa && (cb < ca || ab > aa) {
				a.Frontier = false
				break
			}
		}
	}
}

// WriteSweep renders a parameter sweep, cheapest first, with the frontier
// marked.
func WriteSweep(w io.Writer, runs []*Run, format string, o Options) error {
	MarkFrontier(runs)
	sorted := append([]*Run(nil), runs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Result.Cost.Cost < sorted[j].Result.Cost.Cost })
	if format == FormatJSON {
		return writeJSON(w, map[string]any{"version": o.Version, "runs": sorted})
	}
	rows, right := summaryRows(sorted, true)
	res := runs[0].Result
	if format == FormatMarkdown {
		fmt.Fprintf(w, "Sweep of %s on trace `%s` (%s requests over %s), replica model `%s`.\n\n", res.Policy, res.Trace, Num(res.Requests), Dur(res.TraceDurationSeconds), res.Replica)
		fmt.Fprint(w, mdTable(rows, right))
		fmt.Fprintf(w, "\n`*` marks the cost-versus-attainment frontier. %s.\n", attainmentCaption(runs))
		return nil
	}
	fmt.Fprintf(w, "sweep of %s · trace %s (%s requests over %s) · replica model %s\n\n", res.Policy, res.Trace, Num(res.Requests), Dur(res.TraceDurationSeconds), res.Replica)
	fmt.Fprint(w, table(rows, right, ""))
	fmt.Fprintf(w, "\n* = on the cost-versus-attainment frontier (no other setting is both cheaper and better)\n%s\n", attainmentCaption(runs))
	var best *Run
	for _, r := range sorted {
		if r.Pass != nil && *r.Pass {
			best = r
			break
		}
	}
	if best != nil {
		var parts []string
		for _, k := range paramNames(runs) {
			parts = append(parts, fmt.Sprintf("%s=%g", k, best.Params[k]))
		}
		fmt.Fprintf(w, "cheapest setting that meets the SLO: %s (%s, %s attained)\n", strings.Join(parts, " "), Money(best.Result.Cost.Cost), Pct(best.Result.Attainment))
	} else if len(sorted) > 0 && sorted[0].Pass != nil {
		fmt.Fprintf(w, "no setting in the sweep meets the SLO\n")
	}
	return nil
}

// WriteTimelineCSV writes the sampled timeline.
func WriteTimelineCSV(w io.Writer, res *sim.Result) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"t_seconds", "desired", "pending", "starting", "ready", "terminating", "requests_running", "requests_waiting", "gateway_pending", "arrival_rate_rps"})
	for _, p := range res.Timeline {
		_ = cw.Write([]string{
			strconv.FormatFloat(p.T, 'f', 3, 64), strconv.Itoa(p.Desired), strconv.Itoa(p.Pending), strconv.Itoa(p.Starting),
			strconv.Itoa(p.Ready), strconv.Itoa(p.Terminating), strconv.Itoa(p.Running), strconv.Itoa(p.Waiting),
			strconv.Itoa(p.GatewayPending), strconv.FormatFloat(p.ArrivalRate, 'f', 4, 64),
		})
	}
	cw.Flush()
	return cw.Error()
}

// WriteEventsCSV writes the scale events.
func WriteEventsCSV(w io.Writer, res *sim.Result) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"t_seconds", "from", "to", "source", "reason", "detail"})
	for _, ev := range res.Events {
		_ = cw.Write([]string{strconv.FormatFloat(ev.T, 'f', 3, 64), strconv.Itoa(ev.From), strconv.Itoa(ev.To), ev.Source, ev.Reason, strings.TrimSpace(ev.Detail)})
	}
	cw.Flush()
	return cw.Error()
}

// WriteRequestsCSV writes one row per request.
func WriteRequestsCSV(w io.Writer, res *sim.Result) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"arrival_seconds", "queue_seconds", "ttft_seconds", "e2e_seconds", "replica", "dropped"})
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }
	for _, r := range res.RequestRecords {
		_ = cw.Write([]string{f(r.Arrival), f(r.Queue), f(r.TTFT), f(r.E2E), strconv.Itoa(r.Replica), r.Dropped})
	}
	cw.Flush()
	return cw.Error()
}
