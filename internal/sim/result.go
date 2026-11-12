package sim

import (
	"math"
	"sort"

	"github.com/cb-Sigma/scale-backtest/internal/trace"
)

// Percentiles summarises a latency distribution, in seconds.
type Percentiles struct {
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
}

// ScaleEvent is one change of the desired replica count.
type ScaleEvent struct {
	T    float64 `json:"t_seconds"`
	From int     `json:"from"`
	To   int     `json:"to"`
	// Source is "HPA" or "KEDA".
	Source string `json:"source"`
	// Reason is the reason the autoscaler gave. For an HPA it is the text
	// of its SuccessfulRescale event.
	Reason string `json:"reason"`
	// Detail lists the metric values behind the decision.
	Detail string `json:"detail,omitempty"`
}

// TimelinePoint is one sample of the cluster state.
type TimelinePoint struct {
	T              float64 `json:"t_seconds"`
	Desired        int     `json:"desired"`
	Pending        int     `json:"pending"`
	Starting       int     `json:"starting"`
	Ready          int     `json:"ready"`
	Terminating    int     `json:"terminating"`
	Running        int     `json:"requests_running"`
	Waiting        int     `json:"requests_waiting"`
	GatewayPending int     `json:"gateway_pending"`
	// ArrivalRate is the mean arrival rate since the previous sample.
	ArrivalRate float64 `json:"arrival_rate_rps"`
}

// ReplicaChange is the exact replica state after one state transition.
type ReplicaChange struct {
	T           float64 `json:"t_seconds"`
	Pending     int     `json:"pending"`
	Starting    int     `json:"starting"`
	Ready       int     `json:"ready"`
	Terminating int     `json:"terminating"`
}

// RequestRecord is the outcome of one request.
type RequestRecord struct {
	Arrival float64 `json:"arrival_seconds"`
	// Queue, TTFT and E2E are measured from arrival. They are 0 for a
	// request that was dropped before reaching that point.
	Queue   float64 `json:"queue_seconds"`
	TTFT    float64 `json:"ttft_seconds"`
	E2E     float64 `json:"e2e_seconds"`
	Dropped string  `json:"dropped,omitempty"`
	Replica int     `json:"replica"`
}

// ReplicaStats summarises the replica timeline.
type ReplicaStats struct {
	Initial    int     `json:"initial"`
	MaxDesired int     `json:"max_desired"`
	MaxReady   int     `json:"max_ready"`
	MinReady   int     `json:"min_ready"`
	MeanReady  float64 `json:"mean_ready"`
	ColdStarts int     `json:"cold_starts"`
}

// CostStats is the cost side of a run.
type CostStats struct {
	// GPUHours is every billed GPU-hour: replicas from creation to removal
	// plus nodes kept warm with nothing on them.
	GPUHours float64 `json:"gpu_hours"`
	// Cost is GPUHours x the price per GPU-hour.
	Cost            float64 `json:"cost"`
	GPUPricePerHour float64 `json:"gpu_price_per_hour"`
	// ReplicaSeconds is the sum of replica lifetimes (creation to removal).
	ReplicaSeconds float64 `json:"replica_seconds"`
	// WarmNodeSeconds is node time with no replica on it.
	WarmNodeSeconds float64 `json:"warm_node_seconds"`
	// StartingShare is the share of billed time spent waiting for a node or
	// starting up; IdleShare is the share spent Ready with nothing running
	// (plus empty warm nodes); BusyShare is the rest.
	StartingShare float64 `json:"starting_share"`
	IdleShare     float64 `json:"idle_share"`
	BusyShare     float64 `json:"busy_share"`
	// MeanUtilization is the mean of running requests / maxConcurrency over
	// the time replicas were serving.
	MeanUtilization float64 `json:"mean_utilization"`
	// PerMillionOutputTokens is Cost per million generated tokens.
	PerMillionOutputTokens float64 `json:"cost_per_million_output_tokens"`
}

// Result is the outcome of a run.
type Result struct {
	Policy      string `json:"policy"`
	PolicyKind  string `json:"policy_kind"`
	PolicyPath  string `json:"policy_path,omitempty"`
	Trace       string `json:"trace"`
	Replica     string `json:"replica_model"`
	Seed        uint64 `json:"seed"`
	LB          string `json:"load_balancing"`
	Approximate bool   `json:"approximate_trace,omitempty"`

	// DurationSeconds is the simulated time: the trace, plus however long
	// the last requests took to finish.
	DurationSeconds      float64 `json:"duration_seconds"`
	TraceDurationSeconds float64 `json:"trace_duration_seconds"`

	Requests     int            `json:"requests"`
	Completed    int            `json:"completed"`
	Dropped      int            `json:"dropped"`
	DropReasons  map[string]int `json:"drop_reasons,omitempty"`
	ColdHits     int            `json:"cold_hits"`
	OutputTokens float64        `json:"output_tokens"`

	// Latencies cover completed requests and include all waiting.
	TTFT  Percentiles `json:"ttft_seconds"`
	E2E   Percentiles `json:"e2e_seconds"`
	Queue Percentiles `json:"queue_seconds"`

	// Targets and attainment. Attainment is over all requests: a dropped
	// request misses every target.
	TTFTTargetSeconds float64  `json:"ttft_target_seconds,omitempty"`
	E2ETargetSeconds  float64  `json:"e2e_target_seconds,omitempty"`
	TTFTAttainment    *float64 `json:"ttft_attainment,omitempty"`
	E2EAttainment     *float64 `json:"e2e_attainment,omitempty"`
	// Attainment is the share of requests that completed and met every
	// target that was set (with no target: the share that completed).
	Attainment float64 `json:"attainment"`

	Replicas ReplicaStats `json:"replicas"`
	Cost     CostStats    `json:"cost"`

	Events   []ScaleEvent    `json:"scale_events"`
	Timeline []TimelinePoint `json:"timeline"`
	// Changes is the exact log of replica state transitions.
	Changes []ReplicaChange `json:"-"`
	// StateSeconds is the exact time-integral of replicas per state
	// (pending, starting, ready, terminating).
	StateSeconds [4]float64 `json:"-"`

	HPASyncs        int `json:"hpa_syncs"`
	HPAMetricErrors int `json:"hpa_syncs_with_metric_errors"`

	Warnings []string `json:"warnings,omitempty"`
	Notes    []string `json:"notes,omitempty"`

	RequestRecords []RequestRecord `json:"-"`
}

func newResult(cfg *Config) *Result {
	p := cfg.Policy
	return &Result{
		Policy:               p.Label(),
		PolicyKind:           string(p.Kind),
		PolicyPath:           p.Path,
		Trace:                cfg.Trace.Name,
		Replica:              cfg.Model.Name,
		Seed:                 cfg.Seed,
		LB:                   cfg.LB,
		Approximate:          cfg.Trace.Approximate,
		TraceDurationSeconds: cfg.Trace.Duration,
		TTFTTargetSeconds:    cfg.TTFTTargetSeconds,
		E2ETargetSeconds:     cfg.E2ETargetSeconds,
		Warnings:             append([]string(nil), p.Warnings...),
		Notes:                append([]string(nil), cfg.Trace.Notes...),
	}
}

func (e *engine) sample() {
	p := TimelinePoint{
		T: e.now, Desired: e.desired,
		Pending: e.counts[statePending], Starting: e.counts[stateStarting],
		Ready: e.counts[stateReady], Terminating: e.counts[stateTerminating],
		GatewayPending: e.gwLive,
	}
	for _, r := range e.reps {
		p.Running += len(r.running)
		p.Waiting += r.queued
	}
	if n := len(e.res.Timeline); n > 0 {
		if dt := e.now - e.res.Timeline[n-1].T; dt > 0 {
			p.ArrivalRate = float64(e.stepArrived) / dt
		}
	}
	e.stepArrived = 0
	e.res.Timeline = append(e.res.Timeline, p)
}

func summarise(vals []float64) Percentiles {
	if len(vals) == 0 {
		return Percentiles{}
	}
	sort.Float64s(vals)
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return Percentiles{
		Mean: sum / float64(len(vals)),
		P50:  trace.Quantile(vals, 0.50),
		P90:  trace.Quantile(vals, 0.90),
		P95:  trace.Quantile(vals, 0.95),
		P99:  trace.Quantile(vals, 0.99),
		Max:  vals[len(vals)-1],
	}
}

func (e *engine) finish() {
	res := e.res
	end := math.Max(e.cfg.Trace.Duration, e.lastFinish)
	e.advanceTo(end)
	if n := len(res.Timeline); n == 0 || res.Timeline[n-1].T < end {
		e.sample()
	}
	for _, r := range e.reps {
		e.advance(r)
		e.replicaSecs += end - r.created
		e.busySecs += r.busySeconds
		e.concSecs += r.concSeconds
	}
	res.DurationSeconds = end
	for s := statePending; s < stateGone; s++ {
		res.StateSeconds[s] = e.stateSeconds[s]
	}

	// Requests.
	res.Requests = len(e.reqs)
	var ttft, e2e, queue []float64
	okTTFT, okE2E, okAll := 0, 0, 0
	for i := range e.reqs {
		q := &e.reqs[i]
		rec := RequestRecord{Arrival: q.arrival, Replica: -1}
		if q.rep != nil {
			rec.Replica = q.rep.id
		}
		if q.phase == phaseDropped {
			res.Dropped++
			if res.DropReasons == nil {
				res.DropReasons = map[string]int{}
			}
			res.DropReasons[q.drop]++
			rec.Dropped = q.drop
		} else {
			res.Completed++
			rec.Queue, rec.TTFT, rec.E2E = q.started-q.arrival, q.first-q.arrival, q.end-q.arrival
			queue = append(queue, rec.Queue)
			ttft = append(ttft, rec.TTFT)
			e2e = append(e2e, rec.E2E)
			res.OutputTokens += float64(q.output)
			a := e.cfg.TTFTTargetSeconds <= 0 || rec.TTFT <= e.cfg.TTFTTargetSeconds
			b := e.cfg.E2ETargetSeconds <= 0 || rec.E2E <= e.cfg.E2ETargetSeconds
			if a {
				okTTFT++
			}
			if b {
				okE2E++
			}
			if a && b {
				okAll++
			}
		}
		if e.cfg.KeepRequests {
			res.RequestRecords = append(res.RequestRecords, rec)
		}
	}
	res.TTFT, res.E2E, res.Queue = summarise(ttft), summarise(e2e), summarise(queue)
	total := float64(res.Requests)
	res.Attainment = float64(okAll) / total
	if e.cfg.TTFTTargetSeconds > 0 {
		v := float64(okTTFT) / total
		res.TTFTAttainment = &v
	}
	if e.cfg.E2ETargetSeconds > 0 {
		v := float64(okE2E) / total
		res.E2EAttainment = &v
	}

	// Replicas.
	res.Replicas.Initial = 0
	if len(res.Changes) > 0 {
		res.Replicas.MinReady = math.MaxInt
	}
	// The initial replicas are created at time 0; the minimum is taken over
	// the state after time 0 settles.
	lastT := -1.0
	for i, c := range res.Changes {
		if c.T == 0 {
			res.Replicas.Initial = c.Ready
		}
		// Only the last change at a given instant is a state that lasted.
		if i+1 < len(res.Changes) && res.Changes[i+1].T == c.T {
			continue
		}
		if c.T >= end && lastT >= 0 {
			continue
		}
		lastT = c.T
		if c.Ready < res.Replicas.MinReady {
			res.Replicas.MinReady = c.Ready
		}
	}
	if res.Replicas.MinReady == math.MaxInt {
		res.Replicas.MinReady = 0
	}
	if len(res.Changes) == 0 || res.Changes[0].T > 0 {
		res.Replicas.MinReady = 0
	}
	if end > 0 {
		res.Replicas.MeanReady = e.stateSeconds[stateReady] / end
	}
	if res.Replicas.MaxDesired < res.Replicas.Initial {
		res.Replicas.MaxDesired = res.Replicas.Initial
	}

	// Cost.
	c := &res.Cost
	gpus := e.model.Cost.GPUsPerReplica
	c.ReplicaSeconds = e.replicaSecs
	c.WarmNodeSeconds = e.warmSeconds
	billed := e.replicaSecs + e.warmSeconds
	c.GPUHours = billed * gpus / 3600
	c.GPUPricePerHour = e.model.Cost.GPUPricePerHour
	c.Cost = c.GPUHours * c.GPUPricePerHour
	serving := e.stateSeconds[stateReady] + e.stateSeconds[stateTerminating]
	if billed > 0 {
		c.StartingShare = (e.stateSeconds[statePending] + e.stateSeconds[stateStarting]) / billed
		c.BusyShare = e.busySecs / billed
		c.IdleShare = (serving - e.busySecs + e.warmSeconds) / billed
	}
	if serving > 0 {
		c.MeanUtilization = e.concSecs / (serving * float64(e.model.MaxConcurrency))
	}
	if res.OutputTokens > 0 {
		c.PerMillionOutputTokens = c.Cost / res.OutputTokens * 1e6
	}
}
