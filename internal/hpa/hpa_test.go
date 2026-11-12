package hpa

import (
	"errors"
	"testing"
)

// The tests in this file are tied to the Kubernetes documentation. Each one
// names the section it comes from on
// https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/
// (called "HPA doc" below) or the walkthrough at
// https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale-walkthrough/.

var defTol = Tolerances{ScaleDown: 0.1, ScaleUp: 0.1}

func repeat(v float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func i32(v int32) *int32 { return &v }

// HPA doc, "Algorithm details": "if the current metric value is 200m, and the
// desired value is 100m, the number of replicas will be doubled [...] If the
// current value is instead 50m, you'll halve the number of replicas".
func TestDocRatioDoublesAndHalves(t *testing.T) {
	got, usage, err := PodsMetricReplicas(4, 0.100, PodsInput{Values: repeat(0.200, 4)}, defTol)
	if err != nil || got != 8 {
		t.Fatalf("200m vs 100m with 4 replicas: got %d, %v; want 8", got, err)
	}
	if usage != 0.2 {
		t.Fatalf("usage = %v, want 0.2", usage)
	}
	got, _, err = PodsMetricReplicas(4, 0.100, PodsInput{Values: repeat(0.050, 4)}, defTol)
	if err != nil || got != 2 {
		t.Fatalf("50m vs 100m with 4 replicas: got %d, %v; want 2", got, err)
	}
}

// HPA doc, "Algorithm details": "The control plane skips any scaling action
// if the ratio is sufficiently close to 1.0 (within a configurable tolerance,
// 0.1 by default)."
func TestDocDefaultTolerance(t *testing.T) {
	for _, tc := range []struct {
		value float64
		want  int32
	}{
		{109, 10}, // ratio 1.09: inside the band
		{110, 10}, // ratio 1.10: the band is inclusive
		{111, 12}, // ratio 1.11: ceil(1.11*10)
		{91, 10},  // ratio 0.91: inside
		{90, 10},  // ratio 0.90: inclusive
		{89, 9},   // ratio 0.89: ceil(0.89*10)
	} {
		got, _, err := PodsMetricReplicas(10, 100, PodsInput{Values: repeat(tc.value, 10)}, defTol)
		if err != nil || got != tc.want {
			t.Errorf("value %v: got %d, %v; want %d", tc.value, got, err, tc.want)
		}
	}
}

// HPA doc, "Tolerance": "a target memory consumption of 100MiB and a
// scale-up tolerance of 5% [...] the HPA algorithm will only consider scaling
// up if the memory consumption is higher than 105MiB".
func TestDocConfigurableTolerance(t *testing.T) {
	tol := 0.05
	c, err := NewController(Spec{
		MinReplicas: 1, MaxReplicas: 100,
		Metrics:  []MetricSpec{{Kind: PodsAverage, Name: "memory_mib", Target: 100}},
		Behavior: &Behavior{ScaleUp: &ScalingRules{Tolerance: &tol}},
	}, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	got := c.Tolerances()
	if got.ScaleUp != 0.05 || got.ScaleDown != 0.1 {
		t.Fatalf("tolerances = %+v, want up 0.05 down 0.1", got)
	}
	obs := func(v float64) Observation {
		return Observation{SpecReplicas: 10, StatusReplicas: 10, TotalPods: 10, ReadyPods: 10,
			Metrics: []MetricObservation{{Pods: PodsInput{Values: repeat(v, 10)}}}}
	}
	if d := c.Reconcile(0, obs(105)); d.Rescale {
		t.Fatalf("105MiB must not scale: %+v", d)
	}
	if d := c.Reconcile(15, obs(106)); !d.Rescale || d.DesiredReplicas != 11 {
		t.Fatalf("106MiB must scale to 11: %+v", d)
	}
}

// HPA doc, "Algorithm details": "If there were any missing metrics, the
// control plane recomputes the average more conservatively, assuming those
// pods were consuming 100% of the desired value in case of a scale down, and
// 0% in case of a scale up."
func TestDocMissingMetrics(t *testing.T) {
	// Scale-up: 3 pods at 200 against target 100 plus 1 pod without metrics.
	// Without the correction: ceil(2.0*3) = 6. With it the average becomes
	// (200*3+0)/4 = 150, ratio 1.5 over 4 pods: 6 as well; with 3 missing pods
	// the average is 100 and the scale-up is cancelled.
	got, _, _ := PodsMetricReplicas(4, 100, PodsInput{Values: repeat(200, 3), Missing: 1}, defTol)
	if got != 6 {
		t.Errorf("one missing pod on scale-up: got %d, want 6", got)
	}
	got, _, _ = PodsMetricReplicas(6, 100, PodsInput{Values: repeat(200, 3), Missing: 3}, defTol)
	if got != 6 {
		t.Errorf("three missing pods cancel the scale-up: got %d, want 6 (unchanged)", got)
	}
	// Scale-down: 2 pods at 20 against target 100 plus 2 missing pods counted
	// at 100: average (20+20+100+100)/4 = 60, ratio 0.6 over 4 pods: ceil(2.4) = 3.
	got, _, _ = PodsMetricReplicas(4, 100, PodsInput{Values: repeat(20, 2), Missing: 2}, defTol)
	if got != 3 {
		t.Errorf("missing pods on scale-down: got %d, want 3", got)
	}
}

// HPA doc, "Algorithm details": "if any not-yet-ready pods were present, and
// the workload would have scaled up without factoring in missing metrics or
// not-yet-ready pods, the controller conservatively assumes that the
// not-yet-ready pods are consuming 0% of the desired metric, further dampening
// the magnitude of a scale up." and "If the new ratio reverses the scale
// direction, or is within the tolerance, the controller doesn't take any
// scaling action."
func TestDocNotYetReadyPods(t *testing.T) {
	// 2 ready pods at 300 (target 100) and 2 pending pods: average with the
	// pending pods at 0 is 150 over 4 pods -> 6, not ceil(3.0*2)=6 by
	// coincidence; use 4 pending pods to see the dampening: 600/6 = 100 ->
	// within tolerance -> unchanged.
	got, _, _ := PodsMetricReplicas(4, 100, PodsInput{Values: repeat(300, 2), Unready: 2}, defTol)
	if got != 6 {
		t.Errorf("two pending pods: got %d, want 6", got)
	}
	got, _, _ = PodsMetricReplicas(6, 100, PodsInput{Values: repeat(300, 2), Unready: 4}, defTol)
	if got != 6 {
		t.Errorf("pending pods absorb the scale-up: got %d, want 6 (unchanged)", got)
	}
	// Pending pods are not considered on a scale-down: only ready pods count.
	got, _, _ = PodsMetricReplicas(6, 100, PodsInput{Values: repeat(50, 2), Unready: 4}, defTol)
	if got != 1 {
		t.Errorf("scale-down with pending pods: got %d, want ceil(0.5*2)=1", got)
	}
}

func TestPodsMetricErrors(t *testing.T) {
	if _, _, err := PodsMetricReplicas(0, 100, PodsInput{}, defTol); !errors.Is(err, ErrNoPods) {
		t.Errorf("no pods: got %v", err)
	}
	if _, _, err := PodsMetricReplicas(2, 100, PodsInput{Unready: 2}, defTol); !errors.Is(err, ErrNoPodMetrics) {
		t.Errorf("only pending pods: got %v", err)
	}
	if _, _, err := PodsMetricReplicas(2, 0, PodsInput{Values: []float64{1}}, defTol); !errors.Is(err, ErrBadTarget) {
		t.Errorf("zero target: got %v", err)
	}
}

// Walkthrough, "Autoscaling on metrics not related to Kubernetes objects":
// an External metric with `averageValue: 30` means "one worker per 30
// outstanding tasks".
func TestDocExternalAverageValue(t *testing.T) {
	got, usage, err := ExternalAverageReplicas(2, 30, 300, defTol)
	if err != nil || got != 10 {
		t.Fatalf("300 tasks / 30 per worker: got %d, %v; want 10", got, err)
	}
	if usage != 150 {
		t.Fatalf("per-pod usage = %v, want 150", usage)
	}
	// Within tolerance of the per-pod target: unchanged.
	if got, _, _ = ExternalAverageReplicas(10, 30, 315, defTol); got != 10 {
		t.Fatalf("315 tasks on 10 workers is within tolerance: got %d", got)
	}
	// With zero pods the ratio is undefined and the plain quotient is used.
	if got, _, _ = ExternalAverageReplicas(0, 30, 61, defTol); got != 3 {
		t.Fatalf("61 tasks from zero pods: got %d, want 3", got)
	}
	if got, _, _ = ExternalAverageReplicas(0, 30, 0, defTol); got != 0 {
		t.Fatalf("no tasks from zero pods: got %d, want 0", got)
	}
}

// HPA doc, "How does a HorizontalPodAutoscaler work?": for external metrics
// "a single metric is fetched [...] This metric is compared to the target
// value, to produce a ratio". "Algorithm details": "For external and object
// metrics, the replica count is based on the number of Running and Ready
// Pods".
func TestDocExternalValue(t *testing.T) {
	got, _, err := ExternalValueReplicas(3, 5, 20, 3, 3, defTol)
	if err != nil || got != 12 {
		t.Fatalf("value 20 vs target 5 on 3 ready pods: got %d, %v; want 12", got, err)
	}
	// Only ready pods are multiplied.
	if got, _, _ = ExternalValueReplicas(3, 5, 20, 3, 2, defTol); got != 8 {
		t.Fatalf("2 ready pods: got %d, want 8", got)
	}
	// From zero replicas the ratio itself is the proposal.
	if got, _, _ = ExternalValueReplicas(0, 5, 12, 0, 0, defTol); got != 3 {
		t.Fatalf("from zero: got %d, want ceil(12/5)=3", got)
	}
}

func obsPods(spec int32, perMetric ...[]float64) Observation {
	o := Observation{SpecReplicas: spec, StatusReplicas: spec, TotalPods: int(spec), ReadyPods: int(spec)}
	for _, vals := range perMetric {
		o.Metrics = append(o.Metrics, MetricObservation{Pods: PodsInput{Values: vals}})
	}
	return o
}

func mustController(t *testing.T, spec Spec) *Controller {
	t.Helper()
	c, err := NewController(spec, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// HPA doc, "Scaling on multiple metrics": "the HorizontalPodAutoscaler
// controller evaluates each metric, and proposes a new scale based on that
// metric. The HorizontalPodAutoscaler takes the maximum scale recommended for
// each metric".
func TestDocMultipleMetricsTakeMax(t *testing.T) {
	c := mustController(t, Spec{MinReplicas: 1, MaxReplicas: 50, Metrics: []MetricSpec{
		{Kind: PodsAverage, Name: "a", Target: 10},
		{Kind: PodsAverage, Name: "b", Target: 10},
	}})
	d := c.Reconcile(0, obsPods(4, repeat(12, 4), repeat(15, 4)))
	if !d.Rescale || d.DesiredReplicas != 6 {
		t.Fatalf("want 6 (from metric b: ceil(1.5*4)), got %+v", d)
	}
	if d.Reason != "pods metric b above target" {
		t.Fatalf("reason = %q", d.Reason)
	}
	if d.Metrics[0].Proposal != 5 || d.Metrics[1].Proposal != 6 {
		t.Fatalf("proposals = %d, %d; want 5, 6", d.Metrics[0].Proposal, d.Metrics[1].Proposal)
	}
}

// HPA doc, "Algorithm details": "If any of these metrics cannot be converted
// into a desired replica count [...] and a scale down is suggested by the
// metrics which can be fetched, scaling is skipped. This means that the HPA is
// still capable of scaling up if one or more metrics give a desiredReplicas
// greater than the current value."
func TestDocInvalidMetricBlocksScaleDownOnly(t *testing.T) {
	spec := Spec{MinReplicas: 1, MaxReplicas: 50, Metrics: []MetricSpec{
		{Kind: PodsAverage, Name: "a", Target: 10},
		{Kind: PodsAverage, Name: "b", Target: 10},
	}}
	broken := errors.New("metrics API unavailable")

	c := mustController(t, spec)
	o := obsPods(4, repeat(2, 4), nil)
	o.Metrics[1] = MetricObservation{Err: broken}
	d := c.Reconcile(0, o)
	if d.Rescale || d.DesiredReplicas != 4 || d.Err == nil {
		t.Fatalf("scale-down with an invalid metric must be skipped: %+v", d)
	}

	c = mustController(t, spec)
	o = obsPods(4, repeat(20, 4), nil)
	o.Metrics[1] = MetricObservation{Err: broken}
	d = c.Reconcile(0, o)
	if !d.Rescale || d.DesiredReplicas != 8 {
		t.Fatalf("scale-up with an invalid metric must proceed: %+v", d)
	}

	c = mustController(t, spec)
	o = obsPods(4, nil, nil)
	o.Metrics[0] = MetricObservation{Err: broken}
	o.Metrics[1] = MetricObservation{Err: broken}
	d = c.Reconcile(0, o)
	if d.Rescale || d.MetricProposal != -1 {
		t.Fatalf("all metrics invalid: no scaling: %+v", d)
	}
}

// HPA doc, "Algorithm details", last paragraph and "Stabilization window":
// "the algorithm looks into previously computed desired states, and uses the
// highest value from the specified interval" (300 seconds by default).
func TestDocDownscaleStabilization(t *testing.T) {
	for _, withBehavior := range []bool{false, true} {
		spec := Spec{MinReplicas: 1, MaxReplicas: 50, Metrics: []MetricSpec{{Kind: PodsAverage, Name: "q", Target: 10}}}
		if withBehavior {
			spec.Behavior = &Behavior{} // defaults only
		}
		c := mustController(t, spec)
		// Load drops at t=0: every sync from now on proposes 2 replicas.
		var scaledAt float64 = -1
		for now := 0.0; now <= 400; now += 15 {
			d := c.Reconcile(now, obsPods(10, repeat(2, 10)))
			if d.Rescale {
				scaledAt = now
				if d.DesiredReplicas != 2 {
					t.Fatalf("behavior=%v: scaled to %d, want 2", withBehavior, d.DesiredReplicas)
				}
				if d.Reason != "All metrics below target" {
					t.Fatalf("reason = %q", d.Reason)
				}
				break
			}
			if d.AbleToScale != "ScaleDownStabilized" {
				t.Fatalf("behavior=%v t=%v: AbleToScale = %q, want ScaleDownStabilized", withBehavior, now, d.AbleToScale)
			}
		}
		// The initial recommendation (10 replicas, recorded at t=0) has to
		// leave the 300 s window. A recommendation exactly 300 s old still
		// counts on the legacy path and no longer counts on the behaviour
		// path (upstream compares with Before and After respectively); on a
		// real controller timestamps never coincide like this.
		want := 315.0
		if withBehavior {
			want = 300
		}
		if scaledAt != want {
			t.Fatalf("behavior=%v: scaled down at t=%v, want %v", withBehavior, scaledAt, want)
		}
	}
}

// HPA doc, "Example: change downscale stabilization window" (60 seconds).
func TestDocCustomDownscaleWindow(t *testing.T) {
	c := mustController(t, Spec{MinReplicas: 1, MaxReplicas: 50,
		Metrics:  []MetricSpec{{Kind: PodsAverage, Name: "q", Target: 10}},
		Behavior: &Behavior{ScaleDown: &ScalingRules{StabilizationWindowSeconds: i32(60)}}})
	var scaledAt float64 = -1
	for now := 0.0; now <= 200; now += 15 {
		if d := c.Reconcile(now, obsPods(10, repeat(2, 10))); d.Rescale {
			scaledAt = now
			break
		}
	}
	if scaledAt != 60 {
		t.Fatalf("scaled down at t=%v, want 60", scaledAt)
	}
}

// HPA doc, "Scaling policies": with policies Pods=4/60s and Percent=10/60s,
// "if there are 80 replicas and the target has to be scaled down to 10
// replicas then during the first step 8 replicas will be reduced. In the next
// iteration when the number of replicas is 72, 10% of the pods is 7.2 but the
// number is rounded up to 8. [...] When the number of replicas falls below 40
// the first policy (Pods) is applied and 4 replicas will be reduced at a
// time."
func TestDocScaleDownPolicies(t *testing.T) {
	c := mustController(t, Spec{MinReplicas: 10, MaxReplicas: 100,
		Metrics: []MetricSpec{{Kind: PodsAverage, Name: "q", Target: 100}},
		Behavior: &Behavior{ScaleDown: &ScalingRules{
			StabilizationWindowSeconds: i32(0),
			Policies: []ScalingPolicy{
				{Type: PodsPolicy, Value: 4, PeriodSeconds: 60},
				{Type: PercentPolicy, Value: 10, PeriodSeconds: 60},
			},
		}}})
	replicas := int32(80)
	var sizes []int32
	for now := 0.0; now <= 1500 && replicas > 10; now += 15 {
		d := c.Reconcile(now, obsPods(replicas, repeat(1, int(replicas))))
		if d.Rescale {
			if d.ScalingLimited != "ScaleDownLimit" && d.DesiredReplicas != 10 {
				t.Fatalf("t=%v: ScalingLimited = %q", now, d.ScalingLimited)
			}
			replicas = d.DesiredReplicas
			sizes = append(sizes, replicas)
		}
	}
	want := []int32{72, 64, 57, 51, 45, 40, 36, 32, 28, 24, 20, 16, 12, 10}
	if len(sizes) != len(want) {
		t.Fatalf("sizes = %v, want %v", sizes, want)
	}
	for i := range want {
		if sizes[i] != want[i] {
			t.Fatalf("sizes = %v, want %v", sizes, want)
		}
	}
}

// HPA doc, "Example: limit scale down rate": 10% per minute, plus "no more
// than 5 Pods are removed per minute" with `selectPolicy: Min`.
func TestDocSelectPolicyMin(t *testing.T) {
	c := mustController(t, Spec{MinReplicas: 1, MaxReplicas: 200,
		Metrics: []MetricSpec{{Kind: PodsAverage, Name: "q", Target: 100}},
		Behavior: &Behavior{ScaleDown: &ScalingRules{
			StabilizationWindowSeconds: i32(0),
			SelectPolicy:               SelectMin,
			Policies: []ScalingPolicy{
				{Type: PercentPolicy, Value: 10, PeriodSeconds: 60},
				{Type: PodsPolicy, Value: 5, PeriodSeconds: 60},
			},
		}}})
	// From 100 replicas: 10% is 10 pods, the Pods policy allows 5: Min -> 95.
	d := c.Reconcile(0, obsPods(100, repeat(1, 100)))
	if d.DesiredReplicas != 95 {
		t.Fatalf("got %d, want 95", d.DesiredReplicas)
	}
	// Within the same minute nothing more may be removed.
	d = c.Reconcile(15, obsPods(95, repeat(1, 95)))
	if d.Rescale {
		t.Fatalf("second scale-down inside the period: %+v", d)
	}
	// From 30 replicas: 10% is 3 pods, fewer than 5: Min -> 27.
	c2 := mustController(t, c.Spec())
	d = c2.Reconcile(0, obsPods(30, repeat(1, 30)))
	if d.DesiredReplicas != 27 {
		t.Fatalf("got %d, want 27", d.DesiredReplicas)
	}
}

// HPA doc, "Example: disable scale down": `selectPolicy: Disabled`.
func TestDocSelectPolicyDisabled(t *testing.T) {
	c := mustController(t, Spec{MinReplicas: 1, MaxReplicas: 50,
		Metrics: []MetricSpec{{Kind: PodsAverage, Name: "q", Target: 100}},
		Behavior: &Behavior{ScaleDown: &ScalingRules{
			StabilizationWindowSeconds: i32(0), SelectPolicy: SelectDisabled}}})
	for now := 0.0; now < 900; now += 15 {
		if d := c.Reconcile(now, obsPods(10, repeat(1, 10))); d.Rescale {
			t.Fatalf("scale-down is disabled but got %+v", d)
		}
	}
	// Scale-up still works.
	if d := c.Reconcile(900, obsPods(10, repeat(300, 10))); !d.Rescale || d.DesiredReplicas <= 10 {
		t.Fatalf("scale-up must still work: %+v", d)
	}
}

// HPA doc, "Default behavior": "There are 2 policies where 4 pods or a 100%
// of the currently running replicas may at most be added every 15 seconds".
func TestDocDefaultScaleUpRate(t *testing.T) {
	c := mustController(t, Spec{MinReplicas: 1, MaxReplicas: 100,
		Metrics:  []MetricSpec{{Kind: PodsAverage, Name: "q", Target: 1}},
		Behavior: &Behavior{}})
	replicas := int32(1)
	var sizes []int32
	// Syncs are a little over one period apart so that each one starts a new
	// 15 s policy period, as on a real controller.
	for now := 0.0; len(sizes) < 5; now += 15.5 {
		d := c.Reconcile(now, obsPods(replicas, repeat(1000, int(replicas))))
		if !d.Rescale {
			t.Fatalf("t=%v: expected a scale-up", now)
		}
		replicas = d.DesiredReplicas
		sizes = append(sizes, replicas)
	}
	// max(+4 pods, +100%): 1 -> 5 -> 10 -> 20 -> 40 -> 80.
	want := []int32{5, 10, 20, 40, 80}
	for i := range want {
		if sizes[i] != want[i] {
			t.Fatalf("sizes = %v, want %v", sizes, want)
		}
	}
}

// Without spec.behavior the controller uses its original limit: at most
// double the current replicas, or 4, per sync.
func TestLegacyScaleUpLimit(t *testing.T) {
	c := mustController(t, Spec{MinReplicas: 1, MaxReplicas: 100,
		Metrics: []MetricSpec{{Kind: PodsAverage, Name: "q", Target: 1}}})
	replicas := int32(1)
	var sizes []int32
	for now := 0.0; len(sizes) < 5; now += 15 {
		d := c.Reconcile(now, obsPods(replicas, repeat(1000, int(replicas))))
		if d.ScalingLimited != "ScaleUpLimit" {
			t.Fatalf("ScalingLimited = %q", d.ScalingLimited)
		}
		replicas = d.DesiredReplicas
		sizes = append(sizes, replicas)
	}
	want := []int32{4, 8, 16, 32, 64}
	for i := range want {
		if sizes[i] != want[i] {
			t.Fatalf("sizes = %v, want %v", sizes, want)
		}
	}
}

// A scale-up stabilisation window applies the lowest recent recommendation.
func TestScaleUpStabilization(t *testing.T) {
	c := mustController(t, Spec{MinReplicas: 1, MaxReplicas: 100,
		Metrics:  []MetricSpec{{Kind: PodsAverage, Name: "q", Target: 10}},
		Behavior: &Behavior{ScaleUp: &ScalingRules{StabilizationWindowSeconds: i32(60)}}})
	var scaledAt float64 = -1
	for now := 0.0; now <= 200; now += 15 {
		d := c.Reconcile(now, obsPods(2, repeat(30, 2)))
		if d.Rescale {
			scaledAt = now
			if d.DesiredReplicas != 6 {
				t.Fatalf("scaled to %d, want 6", d.DesiredReplicas)
			}
			break
		}
		if d.AbleToScale != "ScaleUpStabilized" {
			t.Fatalf("t=%v AbleToScale = %q", now, d.AbleToScale)
		}
	}
	if scaledAt != 60 {
		t.Fatalf("scaled up at t=%v, want 60", scaledAt)
	}
}

func TestMinMaxBounds(t *testing.T) {
	spec := Spec{MinReplicas: 2, MaxReplicas: 5, Metrics: []MetricSpec{{Kind: PodsAverage, Name: "q", Target: 10}}}
	c := mustController(t, spec)
	d := c.Reconcile(0, obsPods(4, repeat(100, 4)))
	if d.DesiredReplicas != 5 || d.ScalingLimited != "TooManyReplicas" {
		t.Fatalf("max bound: %+v", d)
	}
	c = mustController(t, spec)
	d = c.Reconcile(0, obsPods(1, repeat(100, 1)))
	if d.DesiredReplicas != 2 || d.Reason != "Current number of replicas below Spec.MinReplicas" {
		t.Fatalf("below min: %+v", d)
	}
	c = mustController(t, spec)
	d = c.Reconcile(0, obsPods(9, repeat(10, 9)))
	if d.DesiredReplicas != 5 || d.Reason != "Current number of replicas above Spec.MaxReplicas" {
		t.Fatalf("above max: %+v", d)
	}
}

// HPA doc, "Implicit maintenance-mode deactivation": "If the target's
// desired replica count is set to 0, and the HPA's minimum replica count is
// greater than 0, the HPA stops adjusting the target".
func TestDocZeroReplicasDisablesScaling(t *testing.T) {
	c := mustController(t, Spec{MinReplicas: 1, MaxReplicas: 5,
		Metrics: []MetricSpec{{Kind: ExternalAverage, Name: "queue", Target: 5}}})
	d := c.Reconcile(0, Observation{Metrics: []MetricObservation{{Total: 100}}})
	if !d.Disabled || d.Rescale {
		t.Fatalf("want scaling disabled, got %+v", d)
	}
}

// HPA doc, "Scaling to and from zero": with an external metric and
// `minReplicas: 0` "the workload is then scaled all the way down to zero
// replicas when there is no load, and scaled back up when the metric indicates
// that at least one replica is needed", and the ScaledToZero condition
// distinguishes that from a manual scale to zero.
func TestDocNativeScaleToZero(t *testing.T) {
	c := mustController(t, Spec{MinReplicas: 0, MaxReplicas: 10,
		Metrics:  []MetricSpec{{Kind: ExternalAverage, Name: "queue", Target: 5}},
		Behavior: &Behavior{ScaleDown: &ScalingRules{StabilizationWindowSeconds: i32(0)}}})
	ext := func(spec int32, total float64) Observation {
		return Observation{SpecReplicas: spec, StatusReplicas: spec, TotalPods: int(spec), ReadyPods: int(spec),
			Metrics: []MetricObservation{{Total: total}}}
	}
	d := c.Reconcile(0, ext(2, 0))
	if !d.Rescale || d.DesiredReplicas != 0 || !c.ScaledToZero() {
		t.Fatalf("empty queue must scale to zero: %+v", d)
	}
	d = c.Reconcile(15, ext(0, 0))
	if d.Rescale || d.Disabled {
		t.Fatalf("still empty: stay at zero and stay active: %+v", d)
	}
	d = c.Reconcile(30, ext(0, 12))
	if !d.Rescale || d.DesiredReplicas != 3 || c.ScaledToZero() {
		t.Fatalf("12 queued / 5 per pod from zero: want 3, got %+v", d)
	}

	// A target that was at zero without the HPA scaling it there is left
	// alone, even with minReplicas 0.
	c2 := mustController(t, c.Spec())
	d = c2.Reconcile(0, ext(0, 50))
	if !d.Disabled {
		t.Fatalf("manual zero must disable scaling: %+v", d)
	}
}

func TestNewControllerValidation(t *testing.T) {
	m := []MetricSpec{{Kind: PodsAverage, Name: "q", Target: 1}}
	bad := []Spec{
		{MinReplicas: 1, MaxReplicas: 0, Metrics: m},
		{MinReplicas: 5, MaxReplicas: 2, Metrics: m},
		{MinReplicas: 0, MaxReplicas: 2, Metrics: m}, // min 0 needs an external metric
		{MinReplicas: 1, MaxReplicas: 2},
		{MinReplicas: 1, MaxReplicas: 2, Metrics: []MetricSpec{{Kind: PodsAverage, Name: "q"}}},
		{MinReplicas: 1, MaxReplicas: 2, Metrics: m, Behavior: &Behavior{ScaleUp: &ScalingRules{SelectPolicy: "Fastest"}}},
		{MinReplicas: 1, MaxReplicas: 2, Metrics: m, Behavior: &Behavior{ScaleUp: &ScalingRules{Policies: []ScalingPolicy{{Type: PodsPolicy, Value: 1, PeriodSeconds: 3600}}}}},
	}
	for i, s := range bad {
		if _, err := NewController(s, DefaultOptions()); err == nil {
			t.Errorf("spec %d: expected an error", i)
		}
	}
}

func TestDescribe(t *testing.T) {
	if got := (MetricSpec{Kind: PodsAverage, Name: "x"}).Describe(); got != "pods metric x" {
		t.Error(got)
	}
	if got := (MetricSpec{Kind: ExternalValue, Name: "x"}).Describe(); got != "external metric x(nil)" {
		t.Error(got)
	}
}
