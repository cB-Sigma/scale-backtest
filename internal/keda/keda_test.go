package keda

import (
	"errors"
	"strings"
	"testing"

	"github.com/cb-Sigma/scale-backtest/internal/hpa"
)

// References below are to the KEDA documentation:
//   - ScaledObject spec: https://keda.sh/docs/latest/reference/scaledobject-spec/
//   - Activation: https://keda.sh/docs/latest/concepts/scaling-deployments/#activating-and-scaling-thresholds

func spec(minReplicas int32, activation float64) Spec {
	return Spec{
		Name: "so", PollingInterval: 30, CooldownPeriod: 300, MinReplicaCount: minReplicas, MaxReplicaCount: 10,
		Triggers: []Trigger{{Name: "queue", Type: "prometheus", Threshold: 10, ActivationThreshold: activation, MetricType: MetricTypeAverageValue}},
	}
}

func reading(v float64) []TriggerReading { return []TriggerReading{{Value: v}} }

func mustOperator(t *testing.T, s Spec) *Operator {
	t.Helper()
	op, err := NewOperator(s, 0)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// "Activation only occurs when this value is greater than the set value; not
// greater than or equal to. ie, in the default case: activationThreshold: 0
// will only activate when the metric value is 1 or more".
func TestDocActivationIsStrictlyGreater(t *testing.T) {
	op := mustOperator(t, spec(0, 0))
	if a := op.Poll(0, 0, reading(0)); a.Scale || a.Active {
		t.Fatalf("metric 0 with activationThreshold 0 must not activate: %+v", a)
	}
	a := op.Poll(30, 0, reading(1))
	if !a.Scale || a.Replicas != 1 || !a.Active {
		t.Fatalf("metric 1 must activate to 1 replica: %+v", a)
	}
	if !strings.Contains(a.Reason, "activated by trigger queue") {
		t.Fatalf("reason = %q", a.Reason)
	}
	op = mustOperator(t, spec(0, 5.5))
	if a := op.Poll(0, 0, reading(5.5)); a.Scale {
		t.Fatalf("metric equal to the activation threshold must not activate: %+v", a)
	}
}

// "The activation value has more priority than the scaling value in case of
// different decisions for each. ie: threshold: 10 and activationThreshold:
// 50, in case of 40 messages the scaler is not active and it'll be scaled to
// zero even the HPA requires 4 instances."
func TestDocActivationWinsOverScaling(t *testing.T) {
	op := mustOperator(t, spec(0, 50))
	// The workload runs 4 replicas; 40 messages is below activation.
	a := op.Poll(0, 4, reading(40))
	if !a.Scale || a.Replicas != 0 {
		t.Fatalf("an inactive scaler with no recorded activity scales to zero: %+v", a)
	}
	// The HPA's own view of the same number would be 4 replicas.
	got, _, _ := hpa.ExternalAverageReplicas(4, 10, 40, hpa.Tolerances{ScaleDown: 0.1, ScaleUp: 0.1})
	if got != 4 {
		t.Fatalf("HPA proposal = %d, want 4", got)
	}
}

// "The period to wait after the last trigger reported active before scaling
// the resource back to 0 [...] By default, it's 300 (5 minutes)." and
// "the KEDA cooldownPeriod only applies when scaling to 0".
func TestDocCooldownPeriod(t *testing.T) {
	op := mustOperator(t, spec(0, 0))
	if a := op.Poll(0, 0, reading(3)); a.Replicas != 1 {
		t.Fatalf("activate: %+v", a)
	}
	// Still active at t=60, then idle from t=90.
	if a := op.Poll(60, 1, reading(2)); a.Scale {
		t.Fatalf("active and already running: nothing to do: %+v", a)
	}
	var scaledAt float64 = -1
	for now := 90.0; now <= 600; now += 30 {
		if a := op.Poll(now, 1, reading(0)); a.Scale {
			if a.Replicas != 0 {
				t.Fatalf("expected a scale to zero: %+v", a)
			}
			scaledAt = now
			break
		}
	}
	// Last active poll at t=60; 300 s later is t=360.
	if scaledAt != 360 {
		t.Fatalf("scaled to zero at t=%v, want 360", scaledAt)
	}
}

// "If the minimum replicas is >= 1, the scaler is always active and the
// activation value will be ignored."
func TestDocMinReplicasAboveZeroNeverScalesToZero(t *testing.T) {
	op := mustOperator(t, spec(2, 100))
	for now := 0.0; now < 1200; now += 30 {
		if a := op.Poll(now, 2, reading(0)); a.Scale {
			t.Fatalf("t=%v: must not scale: %+v", now, a)
		}
	}
	// Below the minimum it is brought back up.
	if a := op.Poll(1200, 1, reading(0)); !a.Scale || a.Replicas != 2 {
		t.Fatalf("below minReplicaCount: %+v", a)
	}
}

// "initialCooldownPeriod: [...] KEDA will not scale the resource back to 0
// until 120 seconds have passed since the ScaledObject creation".
func TestDocInitialCooldownPeriod(t *testing.T) {
	s := spec(0, 0)
	s.InitialCooldownPeriod = 120
	op := mustOperator(t, s)
	if a := op.Poll(30, 1, reading(0)); a.Scale {
		t.Fatalf("inside the initial cooldown: %+v", a)
	}
	if a := op.Poll(120, 1, reading(0)); !a.Scale || a.Replicas != 0 {
		t.Fatalf("after the initial cooldown: %+v", a)
	}
}

// idleReplicaCount: "If there's some activity on target triggers KEDA will
// scale the target resource immediately to minReplicaCount and then will be
// scaling handled by HPA. When there is no activity, the target resource is
// again scaled down to idleReplicaCount."
func TestDocIdleReplicaCount(t *testing.T) {
	s := spec(3, 0)
	idle := int32(0)
	s.IdleReplicaCount = &idle
	op := mustOperator(t, s)
	if a := op.Poll(0, 0, reading(0)); a.Scale {
		t.Fatalf("idle and inactive: stay at idleReplicaCount: %+v", a)
	}
	if a := op.Poll(30, 0, reading(5)); !a.Scale || a.Replicas != 3 {
		t.Fatalf("activity: straight to minReplicaCount: %+v", a)
	}
	if a := op.Poll(60+300, 3, reading(0)); !a.Scale || a.Replicas != 0 {
		t.Fatalf("no activity for the cooldown: back to idle: %+v", a)
	}
	if got := s.HPASpec().MinReplicas; got != 3 {
		t.Fatalf("HPA minReplicas = %d, want 3", got)
	}
	bad := int32(1)
	s.IdleReplicaCount = &bad
	if err := s.Validate(); err == nil {
		t.Fatal("idleReplicaCount other than 0 must be refused")
	}
}

func TestFailedTriggerIsNotActive(t *testing.T) {
	op := mustOperator(t, spec(0, 0))
	a := op.Poll(0, 0, []TriggerReading{{Value: 100, Err: errors.New("prometheus unreachable")}})
	if a.Scale || a.Active {
		t.Fatalf("a failed trigger must not activate: %+v", a)
	}
}

// ScaledObject spec, triggers, metricType: "With AverageValue metric type
// [...] the threshold is 5 messages, and the current message count in the
// queue is 20, HPA will scale the deployment to 20 / 5 = 4 replicas,
// regardless of the current replica count." and "with the Value type [...]
// the threshold is 5 milliseconds, and the current average time is 20
// milliseconds, HPA will scale the deployment to 3 * 20 / 5 = 12."
func TestDocMetricTypes(t *testing.T) {
	s := Spec{Name: "so", PollingInterval: 30, CooldownPeriod: 300, MinReplicaCount: 1, MaxReplicaCount: 100,
		Triggers: []Trigger{
			{Name: "avg", Type: "prometheus", Threshold: 5, MetricType: MetricTypeAverageValue},
			{Name: "val", Type: "prometheus", Threshold: 5, MetricType: MetricTypeValue},
		}}
	h := s.HPASpec()
	if h.Metrics[0].Kind != hpa.ExternalAverage || h.Metrics[1].Kind != hpa.ExternalValue {
		t.Fatalf("metric kinds: %+v", h.Metrics)
	}
	if h.Metrics[0].Name != "s0-prometheus" || h.Metrics[1].Name != "s1-prometheus" || h.Name != "keda-hpa-so" {
		t.Fatalf("generated names: %s %+v", h.Name, h.Metrics)
	}
	tol := hpa.Tolerances{ScaleDown: 0.1, ScaleUp: 0.1}
	for _, current := range []int32{1, 3, 9} {
		if got, _, _ := hpa.ExternalAverageReplicas(current, 5, 20, tol); got != 4 {
			t.Errorf("AverageValue from %d replicas: got %d, want 4", current, got)
		}
	}
	if got, _, _ := hpa.ExternalValueReplicas(3, 5, 20, 3, 3, tol); got != 12 {
		t.Errorf("Value: got %d, want 12", got)
	}
}

// "minReplicaCount [...] Default: 0" and the generated HPA never has
// minReplicas 0: scaling to and from zero is KEDA's job.
func TestGeneratedHPAMinimum(t *testing.T) {
	if got := spec(0, 0).HPASpec().MinReplicas; got != 1 {
		t.Errorf("minReplicaCount 0: HPA minReplicas = %d, want 1", got)
	}
	if got := spec(4, 0).HPASpec().MinReplicas; got != 4 {
		t.Errorf("minReplicaCount 4: HPA minReplicas = %d, want 4", got)
	}
}

func TestValidate(t *testing.T) {
	bad := []func(s *Spec){
		func(s *Spec) { s.Triggers = nil },
		func(s *Spec) { s.MaxReplicaCount = 0 },
		func(s *Spec) { s.MinReplicaCount = 11 },
		func(s *Spec) { s.PollingInterval = 0 },
		func(s *Spec) { s.Triggers[0].Threshold = 0 },
		func(s *Spec) { s.Triggers[0].MetricType = "Utilization" },
	}
	for i, mutate := range bad {
		s := spec(1, 0)
		mutate(&s)
		if _, err := NewOperator(s, 0); err == nil {
			t.Errorf("case %d: expected an error", i)
		}
	}
}
