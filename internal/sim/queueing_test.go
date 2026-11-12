package sim

import (
	"math"
	"testing"

	"github.com/cb-Sigma/scale-backtest/internal/policy"
	"github.com/cb-Sigma/scale-backtest/internal/replica"
	"github.com/cb-Sigma/scale-backtest/internal/trace"
)

// These tests compare the simulator with closed-form results from queueing
// theory. They exercise the event loop, the queues, the load balancer and the
// continuous-batching progress accounting; they say nothing about whether the
// replica model matches a real inference engine.

const meanTokens = 2000 // large, so that rounding to whole tokens is negligible

// poissonTrace builds n requests with Poisson arrivals at rate lambda and
// (approximately) exponential service demand with the given mean number of
// output tokens. The first token is produced by prefill, so a request with
// k output tokens needs k-1 decode steps.
func poissonTrace(n int, lambda float64, seed uint64) *trace.Trace {
	rng := trace.NewRand(seed)
	tr := &trace.Trace{Name: "poisson"}
	now := 0.0
	for range n {
		now += rng.ExpFloat64() / lambda
		tokens := 1 + int(math.Round(rng.ExpFloat64()*meanTokens))
		tr.Requests = append(tr.Requests, trace.Request{Arrival: now, PromptTokens: 0, OutputTokens: tokens})
	}
	tr.Duration = now
	return tr
}

// serverModel returns a replica that serves one request at a time at rate mu
// requests per second, with no internal queue (waiting happens in the shared
// gateway queue, as in M/M/c).
func serverModel(t *testing.T, mu float64) *replica.Model {
	t.Helper()
	zero := 0
	m := &replica.Model{
		Name:           "mmc-server",
		MaxConcurrency: 1,
		MaxQueue:       &zero,
		Decode:         replica.Decode{BaseSecondsPerToken: 1 / (mu * meanTokens)},
	}
	if err := m.Resolve(""); err != nil {
		t.Fatal(err)
	}
	return m
}

// erlangC returns the probability that an arrival has to wait in M/M/c.
func erlangC(c int, a float64) float64 {
	// a = lambda/mu (offered load), rho = a/c.
	sum := 0.0
	term := 1.0
	for k := 0; k < c; k++ {
		if k > 0 {
			term *= a / float64(k)
		}
		sum += term
	}
	term *= a / float64(c) // a^c / c!
	rho := a / float64(c)
	last := term / (1 - rho)
	return last / (sum + last)
}

func runQueue(t *testing.T, tr *trace.Trace, m *replica.Model, servers int) *Result {
	t.Helper()
	res, err := Run(Config{
		Trace: tr, Model: m, Policy: policy.Static(int32(servers)),
		Seed: 1, GatewayQueue: -1, KeepRequests: true, MaxDrainSeconds: 1e9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Dropped != 0 || res.Completed != len(tr.Requests) {
		t.Fatalf("completed %d dropped %d of %d", res.Completed, res.Dropped, len(tr.Requests))
	}
	return res
}

func relErr(got, want float64) float64 { return math.Abs(got-want) / want }

// TestMMcWaitingTime checks the mean waiting time and the probability of
// waiting against the Erlang C formulas:
//
//	P(wait) = C(c, a),  Wq = C(c, a) / (c*mu - lambda),  W = Wq + 1/mu
//
// (for c = 1 these reduce to the M/M/1 results Wq = rho/(mu - lambda)).
func TestMMcWaitingTime(t *testing.T) {
	n := 200000
	if testing.Short() {
		n = 40000
	}
	const mu = 1.0
	cases := []struct {
		name    string
		servers int
		rho     float64
		tol     float64
	}{
		{"M/M/1 rho=0.5", 1, 0.5, 0.04},
		{"M/M/1 rho=0.8", 1, 0.8, 0.06},
		{"M/M/4 rho=0.7", 4, 0.7, 0.05},
		{"M/M/8 rho=0.85", 8, 0.85, 0.07},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lambda := tc.rho * float64(tc.servers) * mu
			tr := poissonTrace(n, lambda, uint64(100+i))
			res := runQueue(t, tr, serverModel(t, mu), tc.servers)

			pWait := erlangC(tc.servers, lambda/mu)
			wantWq := pWait / (float64(tc.servers)*mu - lambda)
			wantW := wantWq + 1/mu

			waited := 0
			for _, r := range res.RequestRecords {
				if r.Queue > 1e-9 {
					waited++
				}
			}
			gotP := float64(waited) / float64(len(res.RequestRecords))
			t.Logf("%s: n=%d  Wq sim %.4f theory %.4f (%.1f%%)  W sim %.4f theory %.4f (%.1f%%)  P(wait) sim %.4f theory %.4f",
				tc.name, n, res.Queue.Mean, wantWq, 100*relErr(res.Queue.Mean, wantWq),
				res.E2E.Mean, wantW, 100*relErr(res.E2E.Mean, wantW), gotP, pWait)
			tol := tc.tol
			if testing.Short() {
				tol *= 2.5
			}
			if relErr(res.Queue.Mean, wantWq) > tol {
				t.Errorf("mean wait %.4f, theory %.4f: off by more than %.0f%%", res.Queue.Mean, wantWq, 100*tol)
			}
			if relErr(res.E2E.Mean, wantW) > tol {
				t.Errorf("mean sojourn %.4f, theory %.4f: off by more than %.0f%%", res.E2E.Mean, wantW, 100*tol)
			}
			if math.Abs(gotP-pWait) > tol/2 {
				t.Errorf("P(wait) %.4f, theory %.4f", gotP, pWait)
			}
		})
	}
}

// TestMM1WaitQuantile checks a tail, not just the mean: in M/M/1 the waiting
// time satisfies P(Wq > t) = rho * exp(-(mu - lambda) t), so the 95th
// percentile is ln(rho/0.05) / (mu - lambda).
func TestMM1WaitQuantile(t *testing.T) {
	n := 200000
	if testing.Short() {
		n = 40000
	}
	const mu, rho = 1.0, 0.7
	lambda := rho * mu
	res := runQueue(t, poissonTrace(n, lambda, 7), serverModel(t, mu), 1)
	want := math.Log(rho/0.05) / (mu - lambda)
	t.Logf("M/M/1 rho=%.1f: p95 wait sim %.3f theory %.3f (%.1f%%)", rho, res.Queue.P95, want, 100*relErr(res.Queue.P95, want))
	tol := 0.06
	if testing.Short() {
		tol = 0.15
	}
	if relErr(res.Queue.P95, want) > tol {
		t.Errorf("p95 wait %.3f, theory %.3f", res.Queue.P95, want)
	}
}

// TestProcessorSharing validates the continuous-batching mechanics. With
// tpot(n) = b*n the batch shares a fixed capacity equally, which is the
// egalitarian processor-sharing queue. For M/G/1-PS the mean sojourn time is
// E[S]/(1-rho) whatever the service distribution, and the mean sojourn time
// of a job of size x is x/(1-rho).
func TestProcessorSharing(t *testing.T) {
	n := 200000
	if testing.Short() {
		n = 40000
	}
	const mu, rho = 1.0, 0.75
	lambda := rho * mu
	m := &replica.Model{
		Name:           "ps-server",
		MaxConcurrency: 1 << 20, // never binding
		Decode:         replica.Decode{SecondsPerTokenPerRequest: 1 / (mu * meanTokens)},
	}
	if err := m.Resolve(""); err != nil {
		t.Fatal(err)
	}
	tr := poissonTrace(n, lambda, 11)
	res := runQueue(t, tr, m, 1)
	want := (1 / mu) / (1 - rho)
	t.Logf("M/M/1-PS rho=%.2f: mean sojourn sim %.4f theory %.4f (%.1f%%)", rho, res.E2E.Mean, want, 100*relErr(res.E2E.Mean, want))
	tol := 0.05
	if testing.Short() {
		tol = 0.12
	}
	if relErr(res.E2E.Mean, want) > tol {
		t.Errorf("mean sojourn %.4f, theory %.4f", res.E2E.Mean, want)
	}
	if res.Queue.Max > 1e-9 {
		t.Errorf("processor sharing must not queue, max queue wait %g", res.Queue.Max)
	}
	// Slowdown is the same for every job size: compare the mean sojourn of
	// short and long jobs with x/(1-rho).
	var shortSum, shortSize, longSum, longSize float64
	for i, r := range res.RequestRecords {
		size := float64(tr.Requests[i].OutputTokens-1) / (mu * meanTokens)
		if tr.Requests[i].OutputTokens < meanTokens/2 {
			shortSum += r.E2E
			shortSize += size
		} else if tr.Requests[i].OutputTokens > 2*meanTokens {
			longSum += r.E2E
			longSize += size
		}
	}
	for name, ratio := range map[string]float64{"short": shortSum / shortSize, "long": longSum / longSize} {
		t.Logf("M/M/1-PS %s jobs: slowdown sim %.3f theory %.3f", name, ratio, 1/(1-rho))
		if relErr(ratio, 1/(1-rho)) > 2*tol {
			t.Errorf("%s jobs: slowdown %.3f, theory %.3f", name, ratio, 1/(1-rho))
		}
	}
}

// TestLossSystem checks the Erlang B blocking probability of M/M/c/c: with
// no queue anywhere, an arrival that finds all c servers busy is dropped.
func TestLossSystem(t *testing.T) {
	n := 200000
	if testing.Short() {
		n = 40000
	}
	const mu, servers = 1.0, 5
	lambda := 4.0
	tr := poissonTrace(n, lambda, 21)
	res, err := Run(Config{Trace: tr, Model: serverModel(t, mu), Policy: policy.Static(servers), Seed: 1, GatewayQueue: 0})
	if err != nil {
		t.Fatal(err)
	}
	// Erlang B by the standard recursion.
	b := 1.0
	for k := 1; k <= servers; k++ {
		b = (lambda / mu * b) / (float64(k) + lambda/mu*b)
	}
	got := float64(res.Dropped) / float64(res.Requests)
	t.Logf("M/M/5/5 a=4: blocking sim %.4f theory (Erlang B) %.4f", got, b)
	if math.Abs(got-b) > 0.01 {
		t.Errorf("blocking %.4f, Erlang B %.4f", got, b)
	}
	if res.DropReasons[DropQueueFull] != res.Dropped {
		t.Errorf("drop reasons: %v", res.DropReasons)
	}
}
