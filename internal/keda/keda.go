// Package keda models the part of KEDA that matters for replica counts: the
// operator loop that activates and deactivates a workload (0 <-> 1) and the
// HorizontalPodAutoscaler KEDA generates for scaling between 1 and N.
//
// It follows the ScaledObject specification
// (https://keda.sh/docs/latest/reference/scaledobject-spec/) and the
// "Activating and Scaling thresholds" section of
// https://keda.sh/docs/latest/concepts/scaling-deployments/.
package keda

import (
	"errors"
	"fmt"

	"github.com/cb-Sigma/scale-backtest/internal/hpa"
)

// Metric types a trigger can ask the generated HPA to use.
const (
	MetricTypeAverageValue = "AverageValue"
	MetricTypeValue        = "Value"
)

// Defaults from the ScaledObject specification.
const (
	DefaultPollingInterval = 30
	DefaultCooldownPeriod  = 300
	DefaultMaxReplicaCount = 100
)

// Trigger is one supported trigger: a metric with a scaling threshold and an
// activation threshold.
type Trigger struct {
	// Name is the trigger name, or a generated one.
	Name string
	// Type is the trigger type as written (only "prometheus" is supported).
	Type string
	// Threshold is the HPA target for the metric.
	Threshold float64
	// ActivationThreshold decides activity: the trigger is active when the
	// metric is strictly greater than it. Default 0.
	ActivationThreshold float64
	// MetricType is AverageValue (default) or Value.
	MetricType string
	// UseCachedMetrics makes the HPA see the value from the last operator
	// poll instead of a fresh query.
	UseCachedMetrics bool
	// IgnoreNullValues mirrors the prometheus trigger's ignoreNullValues
	// (default true): an empty query result counts as 0 instead of an error.
	IgnoreNullValues bool
}

// Spec is the subset of a ScaledObject spec that is simulated.
type Spec struct {
	Name                  string
	PollingInterval       float64
	CooldownPeriod        float64
	InitialCooldownPeriod float64
	MinReplicaCount       int32
	MaxReplicaCount       int32
	// IdleReplicaCount is nil when unset. Only 0 is supported, as the KEDA
	// documentation itself notes.
	IdleReplicaCount *int32
	Triggers         []Trigger
	// Behavior is advanced.horizontalPodAutoscalerConfig.behavior.
	Behavior *hpa.Behavior
}

// Validate checks the spec against the rules in the ScaledObject reference.
func (s Spec) Validate() error {
	if len(s.Triggers) == 0 {
		return errors.New("keda: at least one trigger is required")
	}
	if s.MaxReplicaCount < 1 {
		return errors.New("keda: maxReplicaCount must be at least 1")
	}
	if s.MinReplicaCount < 0 || s.MinReplicaCount > s.MaxReplicaCount {
		return errors.New("keda: minReplicaCount must be between 0 and maxReplicaCount")
	}
	if s.IdleReplicaCount != nil {
		if *s.IdleReplicaCount != 0 {
			return errors.New("keda: idleReplicaCount other than 0 is not supported (KEDA documents 0 as the only working value)")
		}
		if s.MinReplicaCount <= *s.IdleReplicaCount {
			return errors.New("keda: idleReplicaCount must be less than minReplicaCount")
		}
	}
	if s.PollingInterval <= 0 || s.CooldownPeriod < 0 || s.InitialCooldownPeriod < 0 {
		return errors.New("keda: pollingInterval must be positive and cooldown periods must not be negative")
	}
	for _, t := range s.Triggers {
		if t.Threshold <= 0 {
			return fmt.Errorf("keda: trigger %q: threshold must be greater than zero", t.Name)
		}
		if t.MetricType != MetricTypeAverageValue && t.MetricType != MetricTypeValue {
			return fmt.Errorf("keda: trigger %q: metricType %q is not supported", t.Name, t.MetricType)
		}
	}
	return nil
}

// ScalesToZero reports whether KEDA may take the workload to zero replicas.
func (s Spec) ScalesToZero() bool {
	return s.MinReplicaCount == 0 || s.IdleReplicaCount != nil
}

// ActiveFloor is the replica count KEDA scales to when the workload becomes
// active from zero: minReplicaCount, or 1 when that is 0.
func (s Spec) ActiveFloor() int32 {
	if s.MinReplicaCount > 0 {
		return s.MinReplicaCount
	}
	return 1
}

// HPASpec returns the HPA KEDA creates for the ScaledObject: one External
// metric per trigger, named s<index>-<type>, with minReplicas of at least 1
// (the HPA never handles zero; KEDA does).
func (s Spec) HPASpec() hpa.Spec {
	out := hpa.Spec{
		Name:        "keda-hpa-" + s.Name,
		MinReplicas: s.ActiveFloor(),
		MaxReplicas: s.MaxReplicaCount,
		Behavior:    s.Behavior,
	}
	for i, t := range s.Triggers {
		kind := hpa.ExternalAverage
		if t.MetricType == MetricTypeValue {
			kind = hpa.ExternalValue
		}
		out.Metrics = append(out.Metrics, hpa.MetricSpec{
			Kind:     kind,
			Name:     fmt.Sprintf("s%d-%s", i, t.Type),
			Selector: "scaledobject.keda.sh/name=" + s.Name,
			Target:   t.Threshold,
		})
	}
	return out
}

// TriggerReading is the value of one trigger's metric at an operator poll.
type TriggerReading struct {
	Value float64
	Err   error
}

// Action is what the operator does at a poll.
type Action struct {
	// Scale is true when the operator writes a new replica count.
	Scale    bool
	Replicas int32
	Reason   string
	// Active is whether any trigger was active at this poll.
	Active bool
}

// Operator is the state of the KEDA operator loop for one ScaledObject.
type Operator struct {
	spec          Spec
	created       float64
	lastActive    float64
	hasLastActive bool
}

// NewOperator returns the operator state for a ScaledObject created at time
// created.
func NewOperator(spec Spec, created float64) (*Operator, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &Operator{spec: spec, created: created}, nil
}

// Spec returns the ScaledObject spec.
func (o *Operator) Spec() Spec { return o.spec }

// IsActive applies the activation rule to a set of readings: active when any
// trigger's metric is strictly greater than its activation threshold. Failed
// triggers are not active.
func (o *Operator) IsActive(readings []TriggerReading) (active bool, by string) {
	for i, t := range o.spec.Triggers {
		if i >= len(readings) || readings[i].Err != nil {
			continue
		}
		if readings[i].Value > t.ActivationThreshold {
			return true, t.Name
		}
	}
	return false, ""
}

// Poll runs one iteration of the operator loop (every pollingInterval) with
// the current replica count of the target.
func (o *Operator) Poll(now float64, currentReplicas int32, readings []TriggerReading) Action {
	active, by := o.IsActive(readings)
	a := Action{Active: active}
	if active {
		o.lastActive = now
		o.hasLastActive = true
	}
	minReplicas := o.spec.MinReplicaCount
	switch {
	case currentReplicas < minReplicas && o.spec.IdleReplicaCount == nil:
		// Below the minimum: bring it up regardless of activity.
		a.Scale, a.Replicas = true, minReplicas
		a.Reason = fmt.Sprintf("replicas below minReplicaCount %d", minReplicas)
	case active && currentReplicas == 0:
		a.Scale, a.Replicas = true, o.spec.ActiveFloor()
		a.Reason = fmt.Sprintf("activated by trigger %s (metric above activation threshold)", by)
	case !active && currentReplicas > 0 && o.spec.ScalesToZero():
		if now < o.created+o.spec.InitialCooldownPeriod {
			break
		}
		// The cooldown runs from the last poll at which a trigger was
		// active. A target that was never seen active is scaled in at once.
		if !o.hasLastActive || now-o.lastActive >= o.spec.CooldownPeriod {
			a.Scale, a.Replicas = true, 0
			a.Reason = fmt.Sprintf("no trigger active for cooldownPeriod %gs", o.spec.CooldownPeriod)
		}
	}
	return a
}
