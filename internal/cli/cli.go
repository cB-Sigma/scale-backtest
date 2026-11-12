// Package cli implements the scale-backtest command line.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"

	scalebacktest "github.com/cb-Sigma/scale-backtest"
	"github.com/cb-Sigma/scale-backtest/internal/hpa"
	"github.com/cb-Sigma/scale-backtest/internal/metrics"
	"github.com/cb-Sigma/scale-backtest/internal/policy"
	"github.com/cb-Sigma/scale-backtest/internal/replica"
	"github.com/cb-Sigma/scale-backtest/internal/report"
	"github.com/cb-Sigma/scale-backtest/internal/sim"
	"github.com/cb-Sigma/scale-backtest/internal/trace"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitError   = 1
	ExitSLOFail = 2
)

// Version is set at build time with -ldflags "-X ...cli.Version=v1.2.3".
var Version = ""

func version() string {
	if Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

const demoPrefix = "demo:"

// Bundled demo inputs.
const (
	demoTrace   = demoPrefix + "traces/demo.csv"
	demoReplica = demoPrefix + "replicas/demo-gpu.yaml"
	demoPolicy  = demoPrefix + "policies/hpa-queue-depth.yaml"

	demoSweepPolicy = demoPrefix + "policies/hpa-running-requests.yaml"
)

var demoComparePolicies = []string{
	"static:6",
	demoPrefix + "policies/hpa-queue-depth.yaml",
	demoPrefix + "policies/hpa-running-requests.yaml",
	demoPrefix + "policies/keda-prometheus.yaml",
	demoPrefix + "policies/keda-scale-to-zero.yaml",
}

const usage = `scale-backtest replays a request trace through a Kubernetes HPA or KEDA
autoscaling policy in simulation and reports whether latency targets would
have held and what the GPUs would have cost.

Usage:
  scale-backtest run      --policy hpa.yaml --trace requests.csv --replica replica.yaml
  scale-backtest compare  --policy a.yaml --policy b.yaml --trace ... --replica ...
  scale-backtest sweep    --policy hpa.yaml --param target=2:20:2 --trace ... --replica ...
  scale-backtest explain  --policy hpa.yaml
  scale-backtest trace    stats|scale|slice|convert|synth|from-prometheus ...
  scale-backtest calibrate --bench bench.csv [--base replica.yaml] [-o replica.yaml]
  scale-backtest metrics
  scale-backtest version

Every simulating command accepts --demo to use the bundled synthetic trace,
replica model and policies. Run "scale-backtest <command> -h" for the flags.

Exit codes: 0 success, 1 error, 2 an --slo objective was not met.
`

// Main runs the command line and returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return ExitError
	}
	cmd, rest := args[0], args[1:]
	var err error
	code := ExitOK
	switch cmd {
	case "run":
		code, err = cmdRun(rest, stdout, stderr)
	case "compare":
		code, err = cmdCompare(rest, stdout, stderr)
	case "sweep":
		code, err = cmdSweep(rest, stdout, stderr)
	case "explain":
		err = cmdExplain(rest, stdout, stderr)
	case "trace":
		err = cmdTrace(rest, stdout, stderr)
	case "calibrate":
		err = cmdCalibrate(rest, stdout, stderr)
	case "metrics":
		cmdMetrics(stdout)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "scale-backtest %s\n", version())
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "scale-backtest: unknown command %q\n\n%s", cmd, usage)
		return ExitError
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		fmt.Fprintf(stderr, "scale-backtest: %v\n", err)
		return ExitError
	}
	return code
}

// readInput reads a file, or a bundled example for paths that start with
// "demo:".
func readInput(path string) ([]byte, error) {
	if rest, ok := strings.CutPrefix(path, demoPrefix); ok {
		data, err := fs.ReadFile(scalebacktest.Examples, "examples/"+rest)
		if err != nil {
			return nil, fmt.Errorf("bundled example %q: %w", rest, err)
		}
		return data, nil
	}
	return os.ReadFile(path)
}

func inputName(path string) string {
	base := filepath.Base(strings.TrimPrefix(path, demoPrefix))
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// seconds is a flag holding a duration in seconds, written as 2s, 500ms,
// 1m30s or a plain number of seconds.
type seconds float64

func (s *seconds) String() string { return strconv.FormatFloat(float64(*s), 'g', -1, 64) + "s" }

func (s *seconds) Set(v string) error {
	f, err := replica.ParseDuration(v)
	if err != nil {
		return err
	}
	if f < 0 {
		return errors.New("must not be negative")
	}
	*s = seconds(f)
	return nil
}

type multi []string

func (m *multi) String() string { return strings.Join(*m, ",") }

func (m *multi) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// parseArgs parses flags that may be mixed with positional arguments and
// returns the positional ones.
func parseArgs(fset *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fset.Parse(args); err != nil {
			return nil, err
		}
		rest := fset.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func newFlagSet(name string, stderr io.Writer, synopsis string) *flag.FlagSet {
	fset := flag.NewFlagSet(name, flag.ContinueOnError)
	fset.SetOutput(stderr)
	fset.Usage = func() {
		fmt.Fprintf(stderr, "Usage: scale-backtest %s\n\nFlags:\n", synopsis)
		fset.PrintDefaults()
	}
	return fset
}

// simFlags are the flags shared by run, compare and sweep.
type simFlags struct {
	demo        bool
	tracePath   string
	traceFormat string
	replicaPath string

	seed         uint64
	lb           string
	initial      int
	scrape       seconds
	rateWindow   seconds
	hpaSync      seconds
	hpaDownscale seconds
	hpaTolerance float64
	gatewayQueue int
	queueTimeout seconds
	ttft         seconds
	e2e          seconds
	timelineStep seconds
	maxDrain     seconds

	timeCompress float64
	load         float64

	slo       string
	format    string
	aliases   multi
	width     int
	maxEvents int
}

func (f *simFlags) register(fset *flag.FlagSet) {
	fset.BoolVar(&f.demo, "demo", false, "use the bundled synthetic trace, replica model and policies")
	fset.StringVar(&f.tracePath, "trace", "", "request trace (CSV, JSONL or an Azure LLM inference trace)")
	fset.StringVar(&f.traceFormat, "trace-format", trace.FormatAuto, "trace format: auto, csv, jsonl, azure")
	fset.StringVar(&f.replicaPath, "replica", "", "replica model (YAML)")
	fset.Uint64Var(&f.seed, "seed", 1, "random seed; the same seed gives the same result")
	fset.StringVar(&f.lb, "lb", sim.LBLeastRequests, "load balancing across ready replicas: least-requests or round-robin")
	fset.IntVar(&f.initial, "initial-replicas", -1, "replicas that are Ready at the start (-1: the policy's minimum)")
	f.scrape = 15
	fset.Var(&f.scrape, "scrape-interval", "metrics scrape interval; autoscalers only see scraped values")
	f.rateWindow = 60
	fset.Var(&f.rateWindow, "rate-window", "window of *_per_second metrics")
	f.hpaSync = 15
	fset.Var(&f.hpaSync, "hpa-sync-period", "kube-controller-manager --horizontal-pod-autoscaler-sync-period")
	f.hpaDownscale = 300
	fset.Var(&f.hpaDownscale, "hpa-downscale-stabilization", "kube-controller-manager --horizontal-pod-autoscaler-downscale-stabilization")
	fset.Float64Var(&f.hpaTolerance, "hpa-tolerance", 0.1, "kube-controller-manager --horizontal-pod-autoscaler-tolerance")
	fset.IntVar(&f.gatewayQueue, "gateway-queue", -1, "requests that may wait in front of the replicas when none can take them (-1: unbounded, 0: drop)")
	fset.Var(&f.queueTimeout, "queue-timeout", "drop a request that has not started this long after arriving (0: never)")
	fset.Var(&f.ttft, "ttft-target", "time-to-first-token target, e.g. 2s (0: none)")
	fset.Var(&f.e2e, "e2e-target", "end-to-end latency target, e.g. 60s (0: none)")
	f.timelineStep = 15
	fset.Var(&f.timelineStep, "timeline-step", "sampling step of the replica timeline")
	f.maxDrain = 3600
	fset.Var(&f.maxDrain, "max-drain", "how long to keep simulating after the trace ends to finish outstanding requests")
	fset.Float64Var(&f.timeCompress, "time-compress", 1, "replay the trace this many times faster")
	fset.Float64Var(&f.load, "load", 1, "multiply the number of requests (replicating or thinning at random)")
	fset.StringVar(&f.slo, "slo", "", "objectives, e.g. \"attainment>=99%,ttft_p95<=2s,drop_rate<=0.1%\"; exit code 2 if not met")
	fset.StringVar(&f.format, "format", report.FormatTable, "output format: table, json, markdown")
	fset.Var(&f.aliases, "metric-alias", "map a metric name to a known one, e.g. my_queue=vllm:num_requests_waiting (repeatable)")
	fset.IntVar(&f.width, "width", 60, "width of the sparklines")
	fset.IntVar(&f.maxEvents, "max-events", 12, "scale events to list in table and markdown output (0: all)")
}

// inputs are the loaded, shared inputs of a simulating command.
type inputs struct {
	trace    *trace.Trace
	model    *replica.Model
	registry *metrics.Registry
	slo      []report.Objective
	flags    *simFlags
}

func (f *simFlags) prepare() (*inputs, error) {
	if !report.ValidFormat(f.format) {
		return nil, fmt.Errorf("unknown --format %q (table, json, markdown)", f.format)
	}
	if f.demo {
		if f.tracePath == "" {
			f.tracePath = demoTrace
		}
		if f.replicaPath == "" {
			f.replicaPath = demoReplica
		}
		if f.ttft == 0 && f.e2e == 0 {
			f.ttft, f.e2e = 2, 60
		}
	}
	if f.tracePath == "" || f.replicaPath == "" {
		return nil, errors.New("--trace and --replica are required (or use --demo)")
	}
	in := &inputs{flags: f, registry: metrics.NewRegistry()}
	for _, a := range f.aliases {
		name, target, ok := strings.Cut(a, "=")
		if !ok {
			return nil, fmt.Errorf("--metric-alias %q: expected name=known-name", a)
		}
		if err := in.registry.Alias(strings.TrimSpace(name), strings.TrimSpace(target)); err != nil {
			return nil, err
		}
	}
	tr, err := loadTrace(f.tracePath, f.traceFormat)
	if err != nil {
		return nil, err
	}
	if f.timeCompress != 1 || f.load != 1 {
		if f.timeCompress <= 0 || f.load <= 0 {
			return nil, errors.New("--time-compress and --load must be greater than zero")
		}
		tr, err = tr.Scale(trace.ScaleOptions{TimeCompression: f.timeCompress, LoadMultiplier: f.load, JitterSeconds: 1, Seed: f.seed})
		if err != nil {
			return nil, err
		}
	}
	in.trace = tr
	in.model, err = loadReplica(f.replicaPath)
	if err != nil {
		return nil, err
	}
	if f.slo != "" {
		if in.slo, err = report.ParseSLO(f.slo); err != nil {
			return nil, err
		}
	}
	return in, nil
}

func loadTrace(path, format string) (*trace.Trace, error) {
	data, err := readInput(path)
	if err != nil {
		return nil, fmt.Errorf("trace: %w", err)
	}
	tr, err := trace.Read(strings.NewReader(string(data)), format, path)
	if err != nil {
		return nil, fmt.Errorf("trace %s: %w", path, err)
	}
	tr.Name = inputName(path)
	return tr, nil
}

func loadReplica(path string) (*replica.Model, error) {
	data, err := readInput(path)
	if err != nil {
		return nil, fmt.Errorf("replica model: %w", err)
	}
	dir := ""
	if !strings.HasPrefix(path, demoPrefix) {
		dir = filepath.Dir(path)
	}
	m, err := replica.Parse(data, dir)
	if err != nil {
		return nil, fmt.Errorf("replica model %s: %w", path, err)
	}
	if m.Name == "" {
		m.Name = inputName(path)
	}
	return m, nil
}

func loadPolicy(spec string, reg *metrics.Registry) (*policy.Policy, error) {
	if strings.HasPrefix(spec, "static:") {
		return policy.Load(spec, reg)
	}
	data, err := readInput(spec)
	if err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	p, err := policy.Parse(data, reg)
	if err != nil {
		return nil, fmt.Errorf("policy %s: %w", spec, err)
	}
	p.Path = spec
	return p, nil
}

func (in *inputs) config(p *policy.Policy) sim.Config {
	f := in.flags
	var initial *int
	if f.initial >= 0 {
		initial = &f.initial
	}
	return sim.Config{
		Trace: in.trace, Model: in.model, Policy: p, Registry: in.registry,
		Seed: f.seed, LB: f.lb, InitialReplicas: initial,
		ScrapeIntervalSeconds: float64(f.scrape), RateWindowSeconds: float64(f.rateWindow),
		HPA: hpa.Options{
			SyncPeriodSeconds:             float64(f.hpaSync),
			Tolerance:                     f.hpaTolerance,
			DownscaleStabilizationSeconds: float64(f.hpaDownscale),
		},
		GatewayQueue: f.gatewayQueue, QueueTimeoutSeconds: float64(f.queueTimeout),
		TTFTTargetSeconds: float64(f.ttft), E2ETargetSeconds: float64(f.e2e),
		TimelineStepSeconds: float64(f.timelineStep), MaxDrainSeconds: float64(f.maxDrain),
	}
}

func (in *inputs) options() report.Options {
	return report.Options{Width: in.flags.width, MaxEvents: in.flags.maxEvents, Version: version()}
}

func writeFile(path string, write func(w io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := write(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
