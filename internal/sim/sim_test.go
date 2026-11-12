package sim

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cb-Sigma/scale-backtest/internal/policy"
	"github.com/cb-Sigma/scale-backtest/internal/replica"
	"github.com/cb-Sigma/scale-backtest/internal/trace"
)

const examples = "../../examples"

func loadDemo(t *testing.T) (*trace.Trace, *replica.Model) {
	t.Helper()
	tr, err := trace.Load(filepath.Join(examples, "traces/demo.csv"), trace.FormatAuto)
	if err != nil {
		t.Fatal(err)
	}
	m, err := replica.Load(filepath.Join(examples, "replicas/demo-gpu.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return tr, m
}

func loadPolicy(t *testing.T, name string) *policy.Policy {
	t.Helper()
	p, err := policy.Load(filepath.Join(examples, "policies", name), nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func intp(v int) *int { return &v }

func mustRun(t *testing.T, cfg Config) *Result {
	t.Helper()
	res, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

var demoPolicies = []string{
	"hpa-queue-depth.yaml", "hpa-running-requests.yaml", "hpa-token-throughput.yaml",
	"hpa-scale-to-zero.yaml", "keda-prometheus.yaml", "keda-scale-to-zero.yaml",
}

// checkInvariants verifies the conservation laws that must hold for any run.
func checkInvariants(t *testing.T, res *Result, model *replica.Model) {
	t.Helper()
	// Every request is either served or dropped, exactly once.
	if res.Completed+res.Dropped != res.Requests {
		t.Errorf("completed %d + dropped %d != requests %d", res.Completed, res.Dropped, res.Requests)
	}
	reasons := 0
	for _, n := range res.DropReasons {
		reasons += n
	}
	if reasons != res.Dropped {
		t.Errorf("drop reasons sum to %d, dropped %d", reasons, res.Dropped)
	}
	if len(res.RequestRecords) == res.Requests {
		for i, r := range res.RequestRecords {
			if r.Dropped != "" {
				continue
			}
			if r.Queue < -1e-9 || r.TTFT < r.Queue-1e-9 || r.E2E < r.TTFT-1e-9 {
				t.Fatalf("request %d: queue %v <= ttft %v <= e2e %v violated", i, r.Queue, r.TTFT, r.E2E)
			}
		}
	}
	// Replica-seconds: the sum of replica lifetimes equals the integral of
	// the replica-count timeline, computed here from the change log and by
	// the engine's running integrals.
	var fromLog float64
	for i, c := range res.Changes {
		end := res.DurationSeconds
		if i+1 < len(res.Changes) {
			end = res.Changes[i+1].T
		}
		fromLog += float64(c.Pending+c.Starting+c.Ready+c.Terminating) * (end - c.T)
	}
	var fromStates float64
	for _, s := range res.StateSeconds {
		fromStates += s
	}
	tol := 1e-6 * math.Max(1, res.Cost.ReplicaSeconds)
	if math.Abs(fromLog-res.Cost.ReplicaSeconds) > tol || math.Abs(fromStates-res.Cost.ReplicaSeconds) > tol {
		t.Errorf("replica-seconds: lifetimes %.6f, timeline %.6f, state integrals %.6f", res.Cost.ReplicaSeconds, fromLog, fromStates)
	}
	wantHours := (res.Cost.ReplicaSeconds + res.Cost.WarmNodeSeconds) * model.Cost.GPUsPerReplica / 3600
	if math.Abs(res.Cost.GPUHours-wantHours) > 1e-9 || math.Abs(res.Cost.Cost-wantHours*model.Cost.GPUPricePerHour) > 1e-9 {
		t.Errorf("cost: %+v", res.Cost)
	}
	if share := res.Cost.BusyShare + res.Cost.IdleShare + res.Cost.StartingShare; math.Abs(share-1) > 1e-6 {
		t.Errorf("busy+idle+starting = %v, want 1", share)
	}
	if res.Cost.MeanUtilization < 0 || res.Cost.MeanUtilization > 1+1e-9 {
		t.Errorf("utilisation %v", res.Cost.MeanUtilization)
	}
	if res.Attainment < 0 || res.Attainment > 1 {
		t.Errorf("attainment %v", res.Attainment)
	}
	// The desired count only changes through recorded scale events.
	for i := 1; i < len(res.Events); i++ {
		if res.Events[i].From != res.Events[i-1].To || res.Events[i].T < res.Events[i-1].T {
			t.Errorf("scale events are not a chain: %+v then %+v", res.Events[i-1], res.Events[i])
		}
	}
	for _, c := range res.Changes {
		if c.Pending < 0 || c.Starting < 0 || c.Ready < 0 || c.Terminating < 0 {
			t.Errorf("negative replica count: %+v", c)
		}
	}
}

func TestInvariantsOnDemoPolicies(t *testing.T) {
	tr, m := loadDemo(t)
	for _, name := range demoPolicies {
		t.Run(name, func(t *testing.T) {
			p := loadPolicy(t, name)
			res := mustRun(t, Config{Trace: tr, Model: m, Policy: p, Seed: 1, GatewayQueue: -1, KeepRequests: true, TTFTTargetSeconds: 2, E2ETargetSeconds: 60})
			checkInvariants(t, res, m)
			if res.Dropped != 0 {
				t.Errorf("nothing should be dropped with unbounded queues: %v", res.DropReasons)
			}
			if res.Replicas.MaxReady > int(p.HPA.MaxReplicas) || res.Replicas.MaxDesired > int(p.HPA.MaxReplicas) {
				t.Errorf("exceeded maxReplicas: %+v", res.Replicas)
			}
			if len(res.Events) == 0 {
				t.Error("the demo trace has bursts; the policy should have scaled")
			}
		})
	}
}

func TestInvariantsWithDropsAndNodes(t *testing.T) {
	tr, _ := loadDemo(t)
	m, err := replica.Load(filepath.Join(examples, "replicas/cold-node-pool.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, lb := range []string{LBLeastRequests, LBRoundRobin} {
		res := mustRun(t, Config{Trace: tr, Model: m, Policy: loadPolicy(t, "hpa-running-requests.yaml"), Seed: 3, LB: lb,
			GatewayQueue: 20, QueueTimeoutSeconds: 30, KeepRequests: true})
		checkInvariants(t, res, m)
		if res.Dropped == 0 || res.DropReasons[DropQueueTimeout] == 0 {
			t.Errorf("%s: a 30 s queue timeout with 4-minute node provisioning must drop requests: %v", lb, res.DropReasons)
		}
		if res.Cost.WarmNodeSeconds <= 0 {
			t.Errorf("%s: nodes kept warm for 10 minutes must show up in the cost", lb)
		}
		if res.StateSeconds[0] <= 0 {
			t.Errorf("%s: replicas must have spent time Pending on node provisioning", lb)
		}
	}
}

func TestDeterminism(t *testing.T) {
	tr, m := loadDemo(t)
	run := func(seed uint64) string {
		res := mustRun(t, Config{Trace: tr, Model: m, Policy: loadPolicy(t, "keda-scale-to-zero.yaml"), Seed: seed, GatewayQueue: -1, TTFTTargetSeconds: 2})
		data, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	a, b := run(7), run(7)
	if a != b {
		t.Fatal("the same seed must give byte-identical results")
	}
	// The seed drives the cold-start draws, so another seed gives another run.
	if c := run(8); c == a {
		t.Fatal("a different seed should change the result")
	}
}

func simpleModel(t *testing.T, coldStart float64, mutate func(m *replica.Model)) *replica.Model {
	t.Helper()
	m := &replica.Model{Name: "simple", MaxConcurrency: 4,
		Decode:    replica.Decode{BaseSecondsPerToken: 0.01},
		ColdStart: replica.ConstantDist(coldStart)}
	if mutate != nil {
		mutate(m)
	}
	if err := m.Resolve(""); err != nil {
		t.Fatal(err)
	}
	return m
}

func steady(rate, duration float64, out int) *trace.Trace {
	tr := &trace.Trace{Name: "steady", Duration: duration}
	for at := 0.0; at < duration; at += 1 / rate {
		tr.Requests = append(tr.Requests, trace.Request{Arrival: at, PromptTokens: 0, OutputTokens: out})
	}
	return tr
}

func hpaPolicy(t *testing.T, body string) *policy.Policy {
	t.Helper()
	p, err := policy.Parse([]byte("apiVersion: autoscaling/v2\nkind: HorizontalPodAutoscaler\nmetadata: {name: test}\nspec:\n  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: x}\n"+body), nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A single request on an idle replica takes exactly what the model says:
// prefill, then output-1 decode steps.
func TestSingleRequestTiming(t *testing.T) {
	m := simpleModel(t, 0, func(m *replica.Model) {
		m.Prefill = replica.Prefill{BaseSeconds: 0.05, SecondsPerToken: 0.001}
	})
	tr := &trace.Trace{Duration: 10, Requests: []trace.Request{{Arrival: 1, PromptTokens: 200, OutputTokens: 101}}}
	res := mustRun(t, Config{Trace: tr, Model: m, Policy: policy.Static(1), KeepRequests: true})
	r := res.RequestRecords[0]
	if math.Abs(r.TTFT-0.25) > 1e-9 || math.Abs(r.E2E-1.25) > 1e-9 || r.Queue != 0 {
		t.Fatalf("ttft %v e2e %v queue %v; want 0.25, 1.25, 0", r.TTFT, r.E2E, r.Queue)
	}
	if res.OutputTokens != 101 || res.DurationSeconds != 10 {
		t.Fatalf("%+v", res)
	}
}

// Two requests that share the batch slow each other down as the curve says.
func TestBatchSharing(t *testing.T) {
	m := simpleModel(t, 0, func(m *replica.Model) {
		m.Decode = replica.Decode{BaseSecondsPerToken: 0.01, SecondsPerTokenPerRequest: 0.01}
	})
	tr := &trace.Trace{Duration: 10, Requests: []trace.Request{
		{Arrival: 0, OutputTokens: 101}, {Arrival: 0, OutputTokens: 51}}}
	res := mustRun(t, Config{Trace: tr, Model: m, Policy: policy.Static(1), KeepRequests: true})
	// Together: tpot(2) = 30 ms. The short one needs 50 steps: 1.5 s. The
	// long one then has 50 steps left alone at tpot(1) = 20 ms: 2.5 s.
	if got := res.RequestRecords[1].E2E; math.Abs(got-1.5) > 1e-9 {
		t.Errorf("short request: %v, want 1.5", got)
	}
	if got := res.RequestRecords[0].E2E; math.Abs(got-2.5) > 1e-9 {
		t.Errorf("long request: %v, want 2.5", got)
	}
}

// Requests beyond maxConcurrency wait inside the replica in FIFO order, and
// beyond maxQueue they are refused.
func TestReplicaQueueAndLimit(t *testing.T) {
	two := 2
	m := simpleModel(t, 0, func(m *replica.Model) { m.MaxConcurrency = 1; m.MaxQueue = &two })
	tr := &trace.Trace{Duration: 10}
	for range 5 {
		tr.Requests = append(tr.Requests, trace.Request{Arrival: 0, OutputTokens: 101}) // 1 s each
	}
	res := mustRun(t, Config{Trace: tr, Model: m, Policy: policy.Static(1), GatewayQueue: 0, KeepRequests: true})
	if res.Completed != 3 || res.DropReasons[DropQueueFull] != 2 {
		t.Fatalf("completed %d, drops %v", res.Completed, res.DropReasons)
	}
	for i, want := range []float64{0, 1, 2} {
		if got := res.RequestRecords[i].Queue; math.Abs(got-want) > 1e-9 {
			t.Errorf("request %d waited %v, want %v", i, got, want)
		}
	}
}

func TestLoadBalancing(t *testing.T) {
	m := simpleModel(t, 0, func(m *replica.Model) { m.MaxConcurrency = 8 })
	// Alternating long and short requests: round-robin sends all the long
	// ones to the same replica, least-requests evens the load out.
	tr := &trace.Trace{Duration: 60}
	for i := range 40 {
		out := 2001
		if i%2 == 1 {
			out = 11
		}
		tr.Requests = append(tr.Requests, trace.Request{Arrival: float64(i) * 0.5, OutputTokens: out})
	}
	count := func(lb string) map[int]int {
		res := mustRun(t, Config{Trace: tr, Model: m, Policy: policy.Static(2), LB: lb, KeepRequests: true})
		long := map[int]int{}
		for i, r := range res.RequestRecords {
			if i%2 == 0 {
				long[r.Replica]++
			}
		}
		return long
	}
	if rr := count(LBRoundRobin); rr[0] != 20 || rr[1] != 0 {
		t.Errorf("round-robin: long requests per replica %v, want all on replica 0", rr)
	}
	if lr := count(LBLeastRequests); lr[0] < 8 || lr[1] < 8 {
		t.Errorf("least-requests: long requests per replica %v, want them spread", lr)
	}
	if _, err := Run(Config{Trace: tr, Model: m, Policy: policy.Static(1), LB: "random"}); err == nil {
		t.Error("unknown load-balancing policy must fail")
	}
}

// The autoscaler sees load only through scraped metrics and a new replica
// serves only after its cold start: the first extra capacity arrives no
// earlier than scrape + sync + cold start after the load begins.
func TestScaleUpDelays(t *testing.T) {
	m := simpleModel(t, 120, nil) // 4 concurrent, 1 s per 101-token request
	tr := &trace.Trace{Duration: 600}
	for at := 100.0; at < 500; at += 0.125 { // 8 req/s from t=100: twice one replica's capacity
		tr.Requests = append(tr.Requests, trace.Request{Arrival: at, OutputTokens: 101})
	}
	p := hpaPolicy(t, "  minReplicas: 1\n  maxReplicas: 4\n  metrics:\n  - type: Pods\n    pods:\n      metric: {name: vllm:num_requests_waiting}\n      target: {type: AverageValue, averageValue: 2}\n")
	res := mustRun(t, Config{Trace: tr, Model: m, Policy: p, GatewayQueue: -1})
	if len(res.Events) == 0 {
		t.Fatal("expected a scale-up")
	}
	first := res.Events[0]
	// Load starts at t=100; the next scrape and sync are both at t=105.
	if first.T != 105 || first.To <= first.From || first.Source != "HPA" {
		t.Fatalf("first event %+v, want a scale-up at t=105", first)
	}
	if !strings.Contains(first.Reason, "pods metric vllm:num_requests_waiting above target") {
		t.Fatalf("reason %q", first.Reason)
	}
	var firstReady float64 = -1
	for _, c := range res.Changes {
		if c.Ready >= 2 {
			firstReady = c.T
			break
		}
	}
	if firstReady != 225 {
		t.Fatalf("second replica ready at t=%v, want 105 + 120 = 225", firstReady)
	}
	if res.Replicas.ColdStarts < 3 || res.Cost.StartingShare <= 0 {
		t.Fatalf("%+v %+v", res.Replicas, res.Cost)
	}
}

// With a long scrape interval the autoscaler reacts to stale numbers.
func TestScrapeStaleness(t *testing.T) {
	m := simpleModel(t, 0, nil)
	tr := &trace.Trace{Duration: 300}
	for at := 10.0; at < 250; at += 0.125 {
		tr.Requests = append(tr.Requests, trace.Request{Arrival: at, OutputTokens: 101})
	}
	p := hpaPolicy(t, "  minReplicas: 1\n  maxReplicas: 4\n  metrics:\n  - type: Pods\n    pods:\n      metric: {name: vllm:num_requests_waiting}\n      target: {type: AverageValue, averageValue: 2}\n")
	fast := mustRun(t, Config{Trace: tr, Model: m, Policy: p, GatewayQueue: -1, ScrapeIntervalSeconds: 15})
	slow := mustRun(t, Config{Trace: tr, Model: m, Policy: p, GatewayQueue: -1, ScrapeIntervalSeconds: 120})
	if fast.Events[0].T != 15 {
		t.Errorf("15 s scrapes: first scale-up at %v, want 15", fast.Events[0].T)
	}
	if slow.Events[0].T != 120 {
		t.Errorf("120 s scrapes: first scale-up at %v, want 120 (the first scrape that saw the queue)", slow.Events[0].T)
	}
	if slow.TTFT.P95 <= fast.TTFT.P95 {
		t.Errorf("stale metrics should hurt latency: p95 TTFT %v vs %v", slow.TTFT.P95, fast.TTFT.P95)
	}
}

// A replica removed by a scale-down finishes the requests it has accepted.
func TestGracefulScaleDown(t *testing.T) {
	m := simpleModel(t, 0, nil)
	// Two long requests (60 s each) on each of two replicas, then nothing.
	tr := &trace.Trace{Duration: 400}
	for i := range 4 {
		tr.Requests = append(tr.Requests, trace.Request{Arrival: float64(i) * 0.01, OutputTokens: 6001})
	}
	p := hpaPolicy(t, "  minReplicas: 1\n  maxReplicas: 2\n  metrics:\n  - type: Pods\n    pods:\n      metric: {name: vllm:num_requests_running}\n      target: {type: AverageValue, averageValue: 10}\n  behavior:\n    scaleDown: {stabilizationWindowSeconds: 0}\n")
	res := mustRun(t, Config{Trace: tr, Model: m, Policy: p, InitialReplicas: intp(2), KeepRequests: true})
	if res.Dropped != 0 || res.Completed != 4 {
		t.Fatalf("in-flight requests must finish: completed %d, dropped %v", res.Completed, res.DropReasons)
	}
	if len(res.Events) == 0 || res.Events[0].To != 1 || res.Events[0].Reason != "All metrics below target" {
		t.Fatalf("events %+v", res.Events)
	}
	sawTerminating := false
	for _, c := range res.Changes {
		if c.Terminating > 0 {
			sawTerminating = true
		}
	}
	if !sawTerminating {
		t.Fatal("the removed replica should have been Terminating while it drained")
	}
	for _, r := range res.RequestRecords {
		if math.Abs(r.E2E-60) > 1 {
			t.Fatalf("a drained request took %v, want about 60", r.E2E)
		}
	}

	// With a grace period shorter than the requests, they are cut off.
	short := replica.Duration(20)
	m2 := simpleModel(t, 0, func(m *replica.Model) { m.TerminationGracePeriod = &short })
	res = mustRun(t, Config{Trace: tr, Model: m2, Policy: p, InitialReplicas: intp(2)})
	if res.DropReasons[DropTerminated] != 2 || res.Completed != 2 {
		t.Fatalf("grace period 20 s: completed %d, drops %v", res.Completed, res.DropReasons)
	}
}

// KEDA scale to zero: requests that arrive at zero replicas wait at the
// gateway for the activation poll plus the cold start.
func TestKEDAScaleToZeroColdHit(t *testing.T) {
	m := simpleModel(t, 60, nil)
	tr := &trace.Trace{Duration: 1500, Requests: []trace.Request{
		{Arrival: 100, OutputTokens: 101}, {Arrival: 101, OutputTokens: 101},
		{Arrival: 1000, OutputTokens: 101},
	}}
	p := loadPolicy(t, "keda-scale-to-zero.yaml") // pollingInterval 15, cooldownPeriod 300
	res := mustRun(t, Config{Trace: tr, Model: m, Policy: p, GatewayQueue: -1, KeepRequests: true})
	if res.Replicas.Initial != 0 || res.ColdHits != 3 || res.Dropped != 0 {
		t.Fatalf("initial %d, cold hits %d, dropped %d", res.Replicas.Initial, res.ColdHits, res.Dropped)
	}
	// Arrival at 100; scraped at 105; KEDA polls at 105 and activates; the
	// replica is Ready at 165: the request waited 65 s.
	if got := res.RequestRecords[0].Queue; math.Abs(got-65) > 1e-9 {
		t.Fatalf("cold hit waited %v, want 65", got)
	}
	var kinds []string
	for _, ev := range res.Events {
		kinds = append(kinds, ev.Source+":"+strings.SplitN(ev.Reason, " ", 3)[1])
		if ev.Source != "KEDA" {
			t.Errorf("only KEDA acts between 0 and 1: %+v", ev)
		}
	}
	if len(res.Events) != 4 || res.Events[0].To != 1 || res.Events[1].To != 0 || res.Events[2].To != 1 || res.Events[3].To != 0 {
		t.Fatalf("events %v", kinds)
	}
	// Scale to zero happens cooldownPeriod after the last active poll. The
	// gateway still showed requests in flight at the 165 scrape (they finish
	// at 166), so the last active poll is at 165 and scale-in is at 465.
	if res.Events[1].T != 465 {
		t.Fatalf("scaled to zero at %v, want 465", res.Events[1].T)
	}
	if !strings.Contains(res.Events[1].Reason, "cooldownPeriod") || !strings.Contains(res.Events[0].Reason, "activated") {
		t.Fatalf("reasons: %q / %q", res.Events[0].Reason, res.Events[1].Reason)
	}
	if res.Replicas.MinReady != 0 || res.Cost.GPUHours >= 1500.0/3600 {
		t.Fatalf("scale to zero must save GPU time: %+v %+v", res.Replicas, res.Cost)
	}
}

// A ScaledObject that scales to zero on a metric the replicas export cannot
// come back: the policy loader warns, and the simulation shows it.
func TestScaleToZeroOnReplicaMetricNeverReturns(t *testing.T) {
	doc := "apiVersion: keda.sh/v1alpha1\nkind: ScaledObject\nmetadata: {name: stuck}\nspec:\n  scaleTargetRef: {name: x}\n  cooldownPeriod: 30\n  triggers:\n  - type: prometheus\n    metadata: {query: sum(vllm:num_requests_running), threshold: \"4\"}\n"
	p, err := policy.Parse([]byte(doc), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) == 0 {
		t.Fatal("expected a warning at load time")
	}
	m := simpleModel(t, 10, nil)
	tr := &trace.Trace{Duration: 600, Requests: []trace.Request{{Arrival: 300, OutputTokens: 11}}}
	res := mustRun(t, Config{Trace: tr, Model: m, Policy: p, InitialReplicas: intp(1), GatewayQueue: -1, MaxDrainSeconds: 120})
	if res.Completed != 0 || res.DropReasons[DropUnserved] != 1 {
		t.Fatalf("completed %d, drops %v", res.Completed, res.DropReasons)
	}
}

// Native HPA scale to zero and back, on a gateway metric.
func TestHPANativeScaleToZero(t *testing.T) {
	m := simpleModel(t, 30, nil)
	tr := &trace.Trace{Duration: 1200}
	for at := 600.0; at < 700; at += 0.5 {
		tr.Requests = append(tr.Requests, trace.Request{Arrival: at, OutputTokens: 101})
	}
	p := loadPolicy(t, "hpa-scale-to-zero.yaml")
	res := mustRun(t, Config{Trace: tr, Model: m, Policy: p, GatewayQueue: -1})
	if res.Dropped != 0 || res.ColdHits == 0 {
		t.Fatalf("dropped %d, cold hits %d", res.Dropped, res.ColdHits)
	}
	if res.Events[0].To != 0 || res.Events[0].T != 315 {
		t.Fatalf("first event %+v, want a scale to zero once the 300 s window has passed", res.Events[0])
	}
	if res.Events[1].From != 0 || res.Events[1].To < 1 || res.Events[1].T < 600 || res.Events[1].T > 630 {
		t.Fatalf("second event %+v, want a scale from zero shortly after t=600", res.Events[1])
	}
	if !strings.Contains(res.Events[1].Reason, "external metric gateway_requests_in_flight") {
		t.Fatalf("reason %q", res.Events[1].Reason)
	}

	// Started at zero by hand, the HPA leaves the target alone.
	res = mustRun(t, Config{Trace: tr, Model: m, Policy: p, InitialReplicas: intp(0), GatewayQueue: -1, MaxDrainSeconds: 60})
	if len(res.Events) != 0 || res.Completed != 0 || len(res.Warnings) == 0 {
		t.Fatalf("events %v completed %d warnings %v", res.Events, res.Completed, res.Warnings)
	}
}

func TestAttainmentCountsDrops(t *testing.T) {
	zero := 0
	m := simpleModel(t, 0, func(m *replica.Model) { m.MaxConcurrency = 1; m.MaxQueue = &zero })
	tr := &trace.Trace{Duration: 10, Requests: []trace.Request{
		{Arrival: 0, OutputTokens: 101}, {Arrival: 0.1, OutputTokens: 101}, // the second is dropped
		{Arrival: 5, OutputTokens: 801}, // 8 s: misses a 2 s end-to-end target
		{Arrival: 14, OutputTokens: 11},
	}}
	tr.Normalise()
	res := mustRun(t, Config{Trace: tr, Model: m, Policy: policy.Static(1), GatewayQueue: 0, TTFTTargetSeconds: 1, E2ETargetSeconds: 2})
	if res.Dropped != 1 || res.DropReasons[DropQueueFull] != 1 {
		t.Fatalf("%v", res.DropReasons)
	}
	if *res.TTFTAttainment != 0.75 || *res.E2EAttainment != 0.5 || res.Attainment != 0.5 {
		t.Fatalf("ttft %v e2e %v all %v; want 0.75, 0.5, 0.5", *res.TTFTAttainment, *res.E2EAttainment, res.Attainment)
	}
}

func TestStaticPolicyCost(t *testing.T) {
	m := simpleModel(t, 0, func(m *replica.Model) { m.Cost = replica.Cost{GPUPricePerHour: 3, GPUsPerReplica: 2} })
	res := mustRun(t, Config{Trace: steady(1, 3600, 11), Model: m, Policy: policy.Static(3)})
	if math.Abs(res.Cost.GPUHours-6) > 1e-6 || math.Abs(res.Cost.Cost-18) > 1e-5 || res.Replicas.MeanReady != 3 || len(res.Events) != 0 {
		t.Fatalf("%+v %+v", res.Cost, res.Replicas)
	}
	if res.Cost.StartingShare != 0 || res.Replicas.MinReady != 3 || res.Replicas.MaxReady != 3 {
		t.Fatalf("%+v %+v", res.Cost, res.Replicas)
	}
}

func TestConfigErrors(t *testing.T) {
	m := simpleModel(t, 0, nil)
	tr := steady(1, 10, 11)
	bad := []Config{
		{},
		{Trace: &trace.Trace{}, Model: m, Policy: policy.Static(1)},
		{Trace: tr, Model: m, Policy: policy.Static(1), QueueTimeoutSeconds: -1},
		{Trace: tr, Model: m, Policy: &policy.Policy{Kind: "Other"}},
	}
	for i, cfg := range bad {
		if _, err := Run(cfg); err == nil {
			t.Errorf("config %d: expected an error", i)
		}
	}
}
