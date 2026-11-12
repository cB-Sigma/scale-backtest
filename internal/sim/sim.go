// Package sim is the discrete-event simulation: requests arrive from a
// trace, a load balancer spreads them over the ready replicas, each replica
// runs a continuous-batching model, metrics are scraped at intervals, and the
// autoscaler (HPA, or KEDA plus its HPA) changes the number of replicas with
// the delays a real cluster has. A run is a pure function of its Config: the
// same seed gives the same Result.
package sim

import (
	"container/heap"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"

	"github.com/cb-Sigma/scale-backtest/internal/hpa"
	"github.com/cb-Sigma/scale-backtest/internal/keda"
	"github.com/cb-Sigma/scale-backtest/internal/metrics"
	"github.com/cb-Sigma/scale-backtest/internal/policy"
	"github.com/cb-Sigma/scale-backtest/internal/replica"
	"github.com/cb-Sigma/scale-backtest/internal/trace"
)

// Load-balancing policies.
const (
	LBRoundRobin    = "round-robin"
	LBLeastRequests = "least-requests"
)

// Drop reasons.
const (
	DropQueueFull    = "queue_full"      // every replica queue and the gateway queue were full
	DropNoReplica    = "no_replica"      // no ready replica and no gateway queue to wait in
	DropQueueTimeout = "queue_timeout"   // waited longer than the queue timeout before starting
	DropTerminated   = "terminated"      // still running when a removed replica's grace period ended
	DropUnserved     = "unserved_at_end" // never served before the drain limit after the trace
	dropNone         = ""
)

// Config is everything a run depends on.
type Config struct {
	Trace  *trace.Trace
	Model  *replica.Model
	Policy *policy.Policy
	// Registry resolves metric names; nil means the built-in names.
	Registry *metrics.Registry

	Seed uint64
	// LB is the load-balancing policy (default least-requests).
	LB string
	// InitialReplicas is the number of replicas that are Ready at time 0.
	// nil means the policy's default: minReplicas (at least 1) for an HPA,
	// minReplicaCount for KEDA (0 when it has idleReplicaCount).
	InitialReplicas *int

	// ScrapeIntervalSeconds is how often replica and gateway metrics are
	// scraped (default 15). Autoscalers only ever see scraped values.
	ScrapeIntervalSeconds float64
	// RateWindowSeconds is the window of *_per_second metrics (default 60).
	RateWindowSeconds float64
	// HPA holds the kube-controller-manager settings.
	HPA hpa.Options

	// GatewayQueue is how many requests may wait in front of the replicas
	// when none can take them. Negative: unbounded. 0: such requests are
	// dropped.
	GatewayQueue int
	// QueueTimeoutSeconds drops a request that has not started running
	// this long after it arrived. 0 disables it.
	QueueTimeoutSeconds float64

	// TTFTTargetSeconds and E2ETargetSeconds are the latency targets used
	// for attainment. 0 means no target.
	TTFTTargetSeconds float64
	E2ETargetSeconds  float64

	// TimelineStepSeconds is the sampling step of the timeline (default 15).
	TimelineStepSeconds float64
	// MaxDrainSeconds bounds how long the run continues after the trace
	// ends to finish outstanding requests (default 3600).
	MaxDrainSeconds float64
	// KeepRequests keeps the per-request records in the result.
	KeepRequests bool
}

func (c *Config) defaults() error {
	if c.Trace == nil || c.Model == nil || c.Policy == nil {
		return errors.New("sim: trace, replica model and policy are required")
	}
	if len(c.Trace.Requests) == 0 {
		return errors.New("sim: the trace is empty")
	}
	if c.Registry == nil {
		c.Registry = metrics.NewRegistry()
	}
	if c.LB == "" {
		c.LB = LBLeastRequests
	}
	if c.LB != LBLeastRequests && c.LB != LBRoundRobin {
		return fmt.Errorf("sim: unknown load-balancing policy %q (%s, %s)", c.LB, LBLeastRequests, LBRoundRobin)
	}
	if c.ScrapeIntervalSeconds == 0 {
		c.ScrapeIntervalSeconds = 15
	}
	if c.RateWindowSeconds == 0 {
		c.RateWindowSeconds = 60
	}
	if c.HPA == (hpa.Options{}) {
		c.HPA = hpa.DefaultOptions()
	}
	if c.TimelineStepSeconds == 0 {
		c.TimelineStepSeconds = 15
	}
	if c.MaxDrainSeconds == 0 {
		c.MaxDrainSeconds = 3600
	}
	if c.ScrapeIntervalSeconds < 0 || c.RateWindowSeconds < 0 || c.TimelineStepSeconds < 0 ||
		c.MaxDrainSeconds < 0 || c.QueueTimeoutSeconds < 0 || c.TTFTTargetSeconds < 0 || c.E2ETargetSeconds < 0 {
		return errors.New("sim: intervals, timeouts and targets must not be negative")
	}
	return nil
}

type repState int

const (
	statePending  repState = iota // waiting for a node
	stateStarting                 // on a node, not Ready yet
	stateReady
	stateTerminating // removed by a scale-down, finishing accepted requests
	stateGone
	numStates
)

type reqPhase int

const (
	phaseNew reqPhase = iota
	phaseGateway
	phaseQueued
	phasePrefill
	phaseDecode
	phaseDone
	phaseDropped
)

type request struct {
	idx       int
	arrival   float64
	prompt    int
	output    int
	phase     reqPhase
	remaining float64 // prefill: solo-seconds left; decode: tokens left
	started   float64
	first     float64
	end       float64
	rep       *rep
	drop      string
	cold      bool
}

type rep struct {
	id      int
	state   repState
	created float64
	readyAt float64
	goneAt  float64
	hasNode bool
	spare   bool // the node is one of the permanently spare nodes

	running []*request
	queue   []*request
	queued  int // live entries in queue

	last    float64 // time up to which progress is accounted
	version int     // invalidates scheduled replica events
	next    *request

	genTokens    float64
	promptTokens float64
	completed    float64
	busySeconds  float64
	concSeconds  float64
}

type eventKind int

// Event kinds, in the order they are processed at equal times.
const (
	evNodeReady eventKind = iota
	evPodReady
	evReplica
	evKill
	evTimeout
	evNodeRelease
	evScrape
	evKEDA
	evHPA
	evSample
)

type event struct {
	t       float64
	kind    eventKind
	seq     uint64
	rep     *rep
	version int
	req     *request
	node    int
}

type eventHeap []*event

func (h eventHeap) Len() int { return len(h) }
func (h eventHeap) Less(i, j int) bool {
	if h[i].t != h[j].t {
		return h[i].t < h[j].t
	}
	if h[i].kind != h[j].kind {
		return h[i].kind < h[j].kind
	}
	return h[i].seq < h[j].seq
}
func (h eventHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *eventHeap) Push(x any)   { *h = append(*h, x.(*event)) }
func (h *eventHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return e
}

type engine struct {
	cfg   Config
	model *replica.Model
	rng   *rand.Rand
	now   float64
	seq   uint64
	heap  eventHeap

	reqs        []request
	outstanding int
	lastFinish  float64

	reps     []*rep // alive (not gone), in creation order
	nextID   int
	desired  int
	rrCursor int

	gateway  []*request
	gwLive   int
	inFlight int
	arrived  float64

	store *metrics.Store

	hpaCtl   *hpa.Controller
	kedaOp   *keda.Operator
	kedaLast []keda.TriggerReading

	// node pool
	spareFree   int
	warm        map[int]float64 // node id -> expiry
	nextNode    int
	warmSeconds float64

	// accounting
	acctT        float64
	stateSeconds [numStates]float64
	counts       [numStates]int
	replicaSecs  float64 // sum of finished replica lifetimes
	busySecs     float64
	concSecs     float64

	res            *Result
	stepArrived    int
	peakThroughput float64
}

// Run executes the simulation.
func Run(cfg Config) (*Result, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	e := &engine{
		cfg:   cfg,
		model: cfg.Model,
		rng:   trace.NewRand(cfg.Seed),
		warm:  map[int]float64{},
		store: metrics.NewStore(math.Max(600, 2*cfg.RateWindowSeconds)),
	}
	e.store.DefaultRateWindow = cfg.RateWindowSeconds
	e.peakThroughput = cfg.Model.PeakThroughput()
	e.res = newResult(&cfg)
	if err := e.setup(); err != nil {
		return nil, err
	}
	e.loop()
	e.finish()
	return e.res, nil
}

func (e *engine) push(ev *event) {
	e.seq++
	ev.seq = e.seq
	heap.Push(&e.heap, ev)
}

func (e *engine) setup() error {
	p := e.cfg.Policy
	initial := -1
	if e.cfg.InitialReplicas != nil {
		initial = max(*e.cfg.InitialReplicas, 0)
	}
	switch p.Kind {
	case policy.KindStatic:
		initial = int(p.StaticReplicas)
	case policy.KindHPA:
		ctl, err := hpa.NewController(p.HPA, e.cfg.HPA)
		if err != nil {
			return err
		}
		e.hpaCtl = ctl
		if initial < 0 {
			initial = int(max(p.HPA.MinReplicas, 1))
		}
		if initial == 0 {
			e.res.Warnings = append(e.res.Warnings, "the run starts at 0 replicas, which an HPA treats as a manual scale to zero: it will not scale the target until something else does")
		}
	case policy.KindKEDA:
		ctl, err := hpa.NewController(p.HPA, e.cfg.HPA)
		if err != nil {
			return err
		}
		e.hpaCtl = ctl
		op, err := keda.NewOperator(*p.KEDA, 0)
		if err != nil {
			return err
		}
		e.kedaOp = op
		if initial < 0 {
			initial = int(p.KEDA.MinReplicaCount)
			if p.KEDA.IdleReplicaCount != nil {
				initial = int(*p.KEDA.IdleReplicaCount)
			}
		}
	default:
		return fmt.Errorf("sim: unknown policy kind %q", p.Kind)
	}
	if initial < 0 {
		initial = 1
	}
	e.spareFree = e.model.Node.SpareNodes
	e.desired = initial
	for range initial {
		r := e.newRep()
		r.hasNode = true
		e.setState(r, stateReady)
		r.readyAt = 0
	}

	tr := e.cfg.Trace
	e.reqs = make([]request, len(tr.Requests))
	for i, r := range tr.Requests {
		e.reqs[i] = request{idx: i, arrival: r.Arrival, prompt: r.PromptTokens, output: max(r.OutputTokens, 1)}
	}
	e.outstanding = len(e.reqs)

	e.push(&event{t: 0, kind: evScrape})
	e.push(&event{t: 0, kind: evSample})
	if e.kedaOp != nil {
		e.push(&event{t: 0, kind: evKEDA})
	}
	if e.hpaCtl != nil {
		e.push(&event{t: e.cfg.HPA.SyncPeriodSeconds, kind: evHPA})
	}
	return nil
}

func (e *engine) loop() {
	tr := e.cfg.Trace
	next := 0
	limit := tr.Duration + e.cfg.MaxDrainSeconds
	for {
		arrT := math.Inf(1)
		if next < len(e.reqs) {
			arrT = e.reqs[next].arrival
		}
		evT := math.Inf(1)
		if len(e.heap) > 0 {
			evT = e.heap[0].t
		}
		t := math.Min(arrT, evT)
		if e.outstanding == 0 && t > tr.Duration {
			return
		}
		if t > limit {
			e.advanceTo(limit)
			e.dropAll(DropUnserved)
			return
		}
		e.advanceTo(t)
		if arrT <= evT {
			e.arrive(&e.reqs[next])
			next++
			continue
		}
		ev := heap.Pop(&e.heap).(*event)
		e.handle(ev)
	}
}

// advanceTo moves the clock and the exact accounting integrals.
func (e *engine) advanceTo(t float64) {
	if t <= e.now {
		return
	}
	dt := t - e.acctT
	for s := statePending; s < stateGone; s++ {
		e.stateSeconds[s] += float64(e.counts[s]) * dt
	}
	e.warmSeconds += float64(e.spareFree+len(e.warm)) * dt
	e.acctT = t
	e.now = t
}

func (e *engine) handle(ev *event) {
	switch ev.kind {
	case evNodeReady:
		if ev.rep.state == statePending {
			ev.rep.hasNode = true
			e.startContainer(ev.rep)
		}
	case evPodReady:
		if ev.rep.state == stateStarting {
			e.setState(ev.rep, stateReady)
			ev.rep.readyAt = e.now
			ev.rep.last = e.now
			e.drainGateway()
		}
	case evReplica:
		if ev.rep.version == ev.version && ev.rep.state != stateGone {
			e.replicaEvent(ev.rep)
		}
	case evKill:
		if ev.rep.state == stateTerminating {
			e.kill(ev.rep)
		}
	case evTimeout:
		r := ev.req
		if r.phase == phaseGateway || r.phase == phaseQueued {
			e.drop(r, DropQueueTimeout)
		}
	case evNodeRelease:
		if exp, ok := e.warm[ev.node]; ok && exp <= e.now {
			delete(e.warm, ev.node)
		}
	case evScrape:
		e.scrape()
		e.push(&event{t: e.now + e.cfg.ScrapeIntervalSeconds, kind: evScrape})
	case evKEDA:
		e.kedaPoll()
		e.push(&event{t: e.now + e.kedaOp.Spec().PollingInterval, kind: evKEDA})
	case evHPA:
		e.hpaSync()
		e.push(&event{t: e.now + e.cfg.HPA.SyncPeriodSeconds, kind: evHPA})
	case evSample:
		e.sample()
		e.push(&event{t: e.now + e.cfg.TimelineStepSeconds, kind: evSample})
	}
}

// ---- replicas ----

func (e *engine) newRep() *rep {
	r := &rep{id: e.nextID, created: e.now, last: e.now, state: stateGone}
	e.nextID++
	e.reps = append(e.reps, r)
	return r
}

func (e *engine) setState(r *rep, s repState) {
	if r.state != stateGone {
		e.counts[r.state]--
	}
	r.state = s
	if s != stateGone {
		e.counts[s]++
	}
	e.res.Changes = append(e.res.Changes, ReplicaChange{
		T: e.now, Pending: e.counts[statePending], Starting: e.counts[stateStarting],
		Ready: e.counts[stateReady], Terminating: e.counts[stateTerminating],
	})
	if e.counts[stateReady] > e.res.Replicas.MaxReady {
		e.res.Replicas.MaxReady = e.counts[stateReady]
	}
}

func (e *engine) createReplica() {
	r := e.newRep()
	// A node: a permanently spare one, a recently freed warm one, or a new
	// one after the provisioning delay.
	switch {
	case e.spareFree > 0:
		e.spareFree--
		r.hasNode, r.spare = true, true
	case len(e.warm) > 0:
		// Take the warm node that would expire last (deterministic choice).
		best, bestExp := -1, math.Inf(-1)
		for id, exp := range e.warm {
			if exp > bestExp || (exp == bestExp && id < best) {
				best, bestExp = id, exp
			}
		}
		delete(e.warm, best)
		r.hasNode = true
	}
	if r.hasNode {
		e.startContainer(r)
		return
	}
	delay := e.model.Node.ProvisionDelay.Sample(e.rng)
	if delay <= 0 {
		r.hasNode = true
		e.startContainer(r)
		return
	}
	e.setState(r, statePending)
	e.push(&event{t: e.now + delay, kind: evNodeReady, rep: r})
}

func (e *engine) startContainer(r *rep) {
	e.setState(r, stateStarting)
	e.res.Replicas.ColdStarts++
	cold := e.model.ColdStart.Sample(e.rng)
	e.push(&event{t: e.now + cold, kind: evPodReady, rep: r})
}

// removeReplica starts the removal of a replica chosen by a scale-down.
func (e *engine) removeReplica(r *rep) {
	switch r.state {
	case statePending, stateStarting:
		e.gone(r)
	case stateReady:
		e.advance(r)
		e.setState(r, stateTerminating)
		if len(r.running) == 0 && r.queued == 0 {
			e.gone(r)
			return
		}
		e.push(&event{t: e.now + e.model.TerminationGrace(), kind: evKill, rep: r})
	}
}

func (e *engine) kill(r *rep) {
	e.advance(r)
	for _, q := range append(append([]*request(nil), r.running...), r.queue...) {
		if q.phase == phaseQueued || q.phase == phasePrefill || q.phase == phaseDecode {
			e.drop(q, DropTerminated)
		}
	}
	r.running, r.queue, r.queued = nil, nil, 0
	e.gone(r)
}

func (e *engine) gone(r *rep) {
	e.advance(r)
	hadNode := r.hasNode
	e.setState(r, stateGone)
	r.goneAt = e.now
	r.version++
	e.replicaSecs += e.now - r.created
	e.busySecs += r.busySeconds
	e.concSecs += r.concSeconds
	e.store.Forget(r.id)
	for i, x := range e.reps {
		if x == r {
			e.reps = append(e.reps[:i], e.reps[i+1:]...)
			if e.rrCursor > i {
				e.rrCursor--
			}
			break
		}
	}
	if r.spare {
		e.spareFree++
	} else if hadNode {
		if keep := e.model.Node.KeepWarm.Seconds(); keep > 0 {
			id := e.nextNode
			e.nextNode++
			e.warm[id] = e.now + keep
			e.push(&event{t: e.now + keep, kind: evNodeRelease, node: id})
		}
	}
}

// scaleTo applies a new desired replica count.
func (e *engine) scaleTo(n int, source, reason, detail string) {
	from := e.desired
	e.desired = n
	e.res.Events = append(e.res.Events, ScaleEvent{T: e.now, From: from, To: n, Source: source, Reason: reason, Detail: detail})
	if n > e.res.Replicas.MaxDesired {
		e.res.Replicas.MaxDesired = n
	}
	live := e.liveReplicas()
	for len(live) < n {
		e.createReplica()
		live = e.liveReplicas()
	}
	if len(live) > n {
		// Removal order follows the ReplicaSet controller's preference:
		// pods that are not running yet first, then the ones that became
		// ready most recently.
		sort.SliceStable(live, func(i, j int) bool {
			a, b := live[i], live[j]
			if a.state != b.state {
				return a.state < b.state
			}
			if a.state == stateReady && a.readyAt != b.readyAt {
				return a.readyAt > b.readyAt
			}
			return a.id > b.id
		})
		for _, r := range live[:len(live)-n] {
			e.removeReplica(r)
		}
		e.drainGateway()
	}
}

// liveReplicas returns the replicas that count towards the desired count.
func (e *engine) liveReplicas() []*rep {
	var out []*rep
	for _, r := range e.reps {
		if r.state == statePending || r.state == stateStarting || r.state == stateReady {
			out = append(out, r)
		}
	}
	return out
}

// ---- requests ----

func (e *engine) hasRoom(r *rep) bool {
	if len(r.running) < e.model.MaxConcurrency {
		return true
	}
	return e.model.MaxQueue == nil || r.queued < *e.model.MaxQueue
}

func (e *engine) pick() *rep {
	var best *rep
	switch e.cfg.LB {
	case LBRoundRobin:
		n := len(e.reps)
		for i := range n {
			r := e.reps[(e.rrCursor+i)%n]
			if r.state == stateReady && e.hasRoom(r) {
				e.rrCursor = (e.rrCursor + i + 1) % n
				return r
			}
		}
	default:
		bestLoad := math.MaxInt
		for _, r := range e.reps {
			if r.state != stateReady || !e.hasRoom(r) {
				continue
			}
			if load := len(r.running) + r.queued; load < bestLoad {
				best, bestLoad = r, load
			}
		}
	}
	return best
}

func (e *engine) arrive(q *request) {
	e.arrived++
	e.stepArrived++
	e.inFlight++
	if e.counts[stateReady] == 0 {
		q.cold = true
		e.res.ColdHits++
	}
	if e.cfg.QueueTimeoutSeconds > 0 {
		e.push(&event{t: e.now + e.cfg.QueueTimeoutSeconds, kind: evTimeout, req: q})
	}
	if r := e.pick(); r != nil {
		e.admit(r, q)
		return
	}
	if e.cfg.GatewayQueue < 0 || e.gwLive < e.cfg.GatewayQueue {
		q.phase = phaseGateway
		e.gateway = append(e.gateway, q)
		e.gwLive++
		return
	}
	if e.counts[stateReady] == 0 {
		e.drop(q, DropNoReplica)
	} else {
		e.drop(q, DropQueueFull)
	}
}

// admit hands a request to a replica that has room.
func (e *engine) admit(r *rep, q *request) {
	e.advance(r)
	q.rep = r
	if len(r.running) < e.model.MaxConcurrency && r.queued == 0 {
		e.start(r, q)
	} else {
		q.phase = phaseQueued
		r.queue = append(r.queue, q)
		r.queued++
	}
	e.reschedule(r)
}

func (e *engine) start(r *rep, q *request) {
	q.phase = phasePrefill
	q.started = e.now
	q.remaining = e.model.PrefillSeconds(q.prompt)
	r.running = append(r.running, q)
}

// advance accounts the progress of a replica's batch up to now.
func (e *engine) advance(r *rep) {
	dt := e.now - r.last
	r.last = e.now
	n := len(r.running)
	if dt <= 0 || n == 0 {
		return
	}
	prefill := dt / e.model.PrefillSlowdown(n)
	decode := dt * e.model.DecodeRate(n)
	for _, q := range r.running {
		if q.phase == phasePrefill {
			q.remaining -= prefill
		} else {
			r.genTokens += math.Min(decode, math.Max(q.remaining, 0))
			q.remaining -= decode
		}
	}
	r.busySeconds += dt
	r.concSeconds += dt * float64(n)
}

// reschedule plans the next internal event of a replica: the earliest end of
// a prefill or decode phase at the current batch size.
func (e *engine) reschedule(r *rep) {
	r.version++
	r.next = nil
	n := len(r.running)
	if n == 0 {
		return
	}
	slow := e.model.PrefillSlowdown(n)
	rate := e.model.DecodeRate(n)
	best := math.Inf(1)
	for _, q := range r.running {
		var dt float64
		if q.phase == phasePrefill {
			dt = q.remaining * slow
		} else {
			dt = q.remaining / rate
		}
		if dt < best {
			best, r.next = dt, q
		}
	}
	if best < 0 {
		best = 0
	}
	e.push(&event{t: e.now + best, kind: evReplica, rep: r, version: r.version})
}

const progressEps = 1e-9

func (e *engine) replicaEvent(r *rep) {
	e.advance(r)
	if r.next != nil {
		r.next.remaining = 0 // the request this event was scheduled for
	}
	kept := r.running[:0]
	for _, q := range r.running {
		if q.remaining > progressEps {
			kept = append(kept, q)
			continue
		}
		if q.phase == phasePrefill {
			// The prefill pass produces the first token.
			q.first = e.now
			r.promptTokens += float64(q.prompt)
			r.genTokens++
			q.phase = phaseDecode
			q.remaining = float64(q.output - 1)
			if q.remaining > progressEps {
				kept = append(kept, q)
				continue
			}
		}
		e.complete(r, q)
	}
	for i := len(kept); i < len(r.running); i++ {
		r.running[i] = nil
	}
	r.running = kept
	e.fill(r)
	if r.state == stateTerminating && len(r.running) == 0 && r.queued == 0 {
		e.gone(r)
		e.drainGateway()
		return
	}
	e.reschedule(r)
	e.drainGateway()
}

// fill moves queued requests into the batch while there is room.
func (e *engine) fill(r *rep) {
	for len(r.running) < e.model.MaxConcurrency && len(r.queue) > 0 {
		q := r.queue[0]
		r.queue[0] = nil
		r.queue = r.queue[1:]
		if q.phase != phaseQueued {
			continue // dropped while waiting
		}
		r.queued--
		e.start(r, q)
	}
	if len(r.queue) == 0 {
		r.queue = nil
	}
}

func (e *engine) complete(r *rep, q *request) {
	q.phase = phaseDone
	q.end = e.now
	r.completed++
	e.finishRequest()
}

func (e *engine) finishRequest() {
	e.outstanding--
	e.inFlight--
	e.lastFinish = e.now
}

func (e *engine) drop(q *request, reason string) {
	switch q.phase {
	case phaseGateway:
		e.gwLive--
	case phaseQueued:
		q.rep.queued--
	case phaseDone, phaseDropped:
		return
	}
	q.phase = phaseDropped
	q.drop = reason
	q.end = e.now
	e.finishRequest()
}

func (e *engine) dropAll(reason string) {
	for i := range e.reqs {
		q := &e.reqs[i]
		if q.phase != phaseDone && q.phase != phaseDropped {
			if q.phase == phaseNew {
				e.inFlight++ // never arrived inside the horizon
			}
			e.drop(q, reason)
		}
	}
}

// drainGateway hands waiting requests to replicas that now have room.
func (e *engine) drainGateway() {
	for e.gwLive > 0 {
		q := e.gateway[0]
		if q.phase != phaseGateway {
			e.gateway[0] = nil
			e.gateway = e.gateway[1:]
			continue
		}
		r := e.pick()
		if r == nil {
			return
		}
		e.gateway[0] = nil
		e.gateway = e.gateway[1:]
		e.gwLive--
		e.admit(r, q)
	}
	if e.gwLive == 0 {
		e.gateway = nil
	}
}

// ---- metrics and autoscalers ----

func (e *engine) values(r *rep) metrics.Values {
	var v metrics.Values
	n := len(r.running)
	v[metrics.RequestsRunning] = float64(n)
	v[metrics.RequestsWaiting] = float64(r.queued)
	v[metrics.KVCacheUsage] = float64(n) / float64(e.model.MaxConcurrency)
	if e.peakThroughput > 0 {
		v[metrics.GPUUtilization] = 100 * e.model.Throughput(n) / e.peakThroughput
	}
	v[metrics.GenerationTokens] = r.genTokens
	v[metrics.PromptTokens] = r.promptTokens
	v[metrics.RequestsCompleted] = r.completed
	return v
}

func (e *engine) scrape() {
	for _, r := range e.reps {
		if r.state == stateReady || r.state == stateTerminating {
			e.advance(r)
			e.store.Scrape(e.now, r.id, e.values(r))
		}
	}
	var g metrics.Values
	g[metrics.GatewayPending] = float64(e.gwLive)
	g[metrics.GatewayInFlight] = float64(e.inFlight)
	g[metrics.GatewayRequests] = e.arrived
	e.store.ScrapeGateway(e.now, g)
}

// readTrigger evaluates a KEDA trigger's query against the scraped metrics.
func (e *engine) readTrigger(i int) keda.TriggerReading {
	src := e.cfg.Policy.Sources[i]
	v, empty, err := e.store.Query(e.now, src.Query, src.Ref)
	if err != nil {
		return keda.TriggerReading{Err: err}
	}
	if empty && !e.cfg.Policy.KEDA.Triggers[i].IgnoreNullValues {
		return keda.TriggerReading{Err: errors.New("the query returned no data and ignoreNullValues is false")}
	}
	return keda.TriggerReading{Value: v}
}

func (e *engine) kedaPoll() {
	spec := e.kedaOp.Spec()
	readings := make([]keda.TriggerReading, len(spec.Triggers))
	for i := range spec.Triggers {
		readings[i] = e.readTrigger(i)
	}
	e.kedaLast = readings
	act := e.kedaOp.Poll(e.now, int32(e.desired), readings)
	if act.Scale && int(act.Replicas) != e.desired {
		detail := ""
		for i, t := range spec.Triggers {
			if readings[i].Err == nil {
				detail += fmt.Sprintf("%s=%.4g (activation %g) ", t.Name, readings[i].Value, t.ActivationThreshold)
			}
		}
		e.scaleTo(int(act.Replicas), "KEDA", act.Reason, detail)
	}
}

func (e *engine) observe() hpa.Observation {
	p := e.cfg.Policy
	obs := hpa.Observation{SpecReplicas: int32(e.desired)}
	var pending, starting, ready []*rep
	for _, r := range e.reps {
		switch r.state {
		case statePending:
			pending = append(pending, r)
		case stateStarting:
			starting = append(starting, r)
		case stateReady:
			ready = append(ready, r)
		}
	}
	obs.StatusReplicas = int32(len(pending) + len(starting) + len(ready))
	obs.TotalPods = len(e.reps)
	obs.ReadyPods = len(ready) + e.counts[stateTerminating]
	obs.Metrics = make([]hpa.MetricObservation, len(p.HPA.Metrics))
	for i, m := range p.HPA.Metrics {
		src := p.Sources[i]
		mo := &obs.Metrics[i]
		switch {
		case m.Kind == hpa.PodsAverage:
			// Pending pods are "not yet ready"; pods whose container is
			// still starting are running but export nothing yet, as are
			// ready pods that have not been scraped: "missing metrics".
			mo.Pods.Unready = len(pending)
			mo.Pods.Missing = len(starting)
			for _, r := range ready {
				if v, ok := e.store.PodValue(e.now, r.id, src.Ref); ok {
					mo.Pods.Values = append(mo.Pods.Values, v)
				} else {
					mo.Pods.Missing++
				}
			}
		case src.Query != nil:
			var reading keda.TriggerReading
			if p.KEDA.Triggers[i].UseCachedMetrics && i < len(e.kedaLast) {
				reading = e.kedaLast[i]
			} else {
				reading = e.readTrigger(i)
			}
			mo.Total, mo.Err = reading.Value, reading.Err
		default:
			total, empty := e.store.Total(e.now, src.Ref)
			if empty {
				mo.Err = fmt.Errorf("no series for external metric %s", m.Name)
			}
			mo.Total = total
		}
	}
	return obs
}

func (e *engine) hpaSync() {
	obs := e.observe()
	d := e.hpaCtl.Reconcile(e.now, obs)
	e.res.HPASyncs++
	if d.Err != nil {
		e.res.HPAMetricErrors++
	}
	if !d.Rescale {
		return
	}
	detail := ""
	for _, m := range d.Metrics {
		if m.Err != nil {
			detail += fmt.Sprintf("%s: error; ", m.Name)
			continue
		}
		detail += fmt.Sprintf("%s=%.4g/%g -> %d; ", m.Name, m.Usage, m.Target, m.Proposal)
	}
	if d.ScalingLimitedActive {
		detail += "limited: " + d.ScalingLimited
	} else if d.AbleToScale != "" && d.AbleToScale != "ReadyForNewScale" {
		detail += d.AbleToScale
	}
	e.scaleTo(int(d.DesiredReplicas), "HPA", d.Reason, detail)
}
