package hpa

import (
	"errors"
	"fmt"
	"math"
)

// MetricKind is the kind of metric source and target an HPA metric uses.
type MetricKind int

const (
	// PodsAverage is `type: Pods` with an AverageValue target.
	PodsAverage MetricKind = iota
	// ExternalValue is `type: External` with a Value target.
	ExternalValue
	// ExternalAverage is `type: External` with an AverageValue target.
	ExternalAverage
)

func (k MetricKind) String() string {
	switch k {
	case PodsAverage:
		return "Pods/AverageValue"
	case ExternalValue:
		return "External/Value"
	case ExternalAverage:
		return "External/AverageValue"
	}
	return "unknown"
}

// MetricSpec is one entry of spec.metrics.
type MetricSpec struct {
	Kind MetricKind
	// Name is the metric name as written in the manifest.
	Name string
	// Selector is the textual form of the metric selector, used only in the
	// reason the controller reports.
	Selector string
	// Target is the target value (averageValue or value) as a plain number.
	Target float64
}

// Describe returns the name the upstream controller uses for the metric in
// its rescale reason.
func (m MetricSpec) Describe() string {
	if m.Kind == PodsAverage {
		return "pods metric " + m.Name
	}
	sel := m.Selector
	if sel == "" {
		sel = "nil"
	}
	return fmt.Sprintf("external metric %s(%s)", m.Name, sel)
}

// Policy types and select policies, as in autoscaling/v2.
const (
	PodsPolicy    = "Pods"
	PercentPolicy = "Percent"

	SelectMax      = "Max"
	SelectMin      = "Min"
	SelectDisabled = "Disabled"
)

// ScalingPolicy is one entry of behavior.scaleUp.policies or
// behavior.scaleDown.policies.
type ScalingPolicy struct {
	Type          string
	Value         int32
	PeriodSeconds int32
}

// ScalingRules is behavior.scaleUp or behavior.scaleDown.
type ScalingRules struct {
	StabilizationWindowSeconds *int32
	SelectPolicy               string
	Policies                   []ScalingPolicy
	Tolerance                  *float64
}

// Behavior is spec.behavior. A nil *Behavior means the field is absent, in
// which case the controller uses its legacy normalisation path (see
// Controller.Reconcile).
type Behavior struct {
	ScaleUp   *ScalingRules
	ScaleDown *ScalingRules
}

// Spec is the part of an HPA spec the control loop uses.
type Spec struct {
	Name        string
	MinReplicas int32
	MaxReplicas int32
	Metrics     []MetricSpec
	Behavior    *Behavior
}

// HasExternalMetrics reports whether any metric is an External metric. Native
// scale to zero requires one.
func (s Spec) HasExternalMetrics() bool {
	for _, m := range s.Metrics {
		if m.Kind != PodsAverage {
			return true
		}
	}
	return false
}

// Options are the kube-controller-manager settings that shape the loop.
type Options struct {
	// SyncPeriodSeconds is --horizontal-pod-autoscaler-sync-period (15s).
	SyncPeriodSeconds float64
	// Tolerance is --horizontal-pod-autoscaler-tolerance (0.1).
	Tolerance float64
	// DownscaleStabilizationSeconds is
	// --horizontal-pod-autoscaler-downscale-stabilization (300s).
	DownscaleStabilizationSeconds float64
}

// DefaultOptions returns the documented kube-controller-manager defaults.
func DefaultOptions() Options {
	return Options{SyncPeriodSeconds: 15, Tolerance: 0.1, DownscaleStabilizationSeconds: 300}
}

// DefaultBehavior fills in the defaults the API server applies when
// spec.behavior is present but incomplete. The values are the ones in the
// "Default behavior" section of the documentation; a nil scale-down
// stabilisation window is left nil and resolved by the controller from
// Options.DownscaleStabilizationSeconds.
func DefaultBehavior(b *Behavior) *Behavior {
	if b == nil {
		return nil
	}
	zero := int32(0)
	up := &ScalingRules{
		StabilizationWindowSeconds: &zero,
		SelectPolicy:               SelectMax,
		Policies: []ScalingPolicy{
			{Type: PodsPolicy, Value: 4, PeriodSeconds: 15},
			{Type: PercentPolicy, Value: 100, PeriodSeconds: 15},
		},
	}
	down := &ScalingRules{
		SelectPolicy: SelectMax,
		Policies: []ScalingPolicy{
			{Type: PercentPolicy, Value: 100, PeriodSeconds: 15},
		},
	}
	mergeRules(up, b.ScaleUp)
	mergeRules(down, b.ScaleDown)
	return &Behavior{ScaleUp: up, ScaleDown: down}
}

func mergeRules(dst, src *ScalingRules) {
	if src == nil {
		return
	}
	if src.SelectPolicy != "" {
		dst.SelectPolicy = src.SelectPolicy
	}
	if src.StabilizationWindowSeconds != nil {
		v := *src.StabilizationWindowSeconds
		dst.StabilizationWindowSeconds = &v
	}
	if src.Policies != nil {
		dst.Policies = append([]ScalingPolicy(nil), src.Policies...)
	}
	if src.Tolerance != nil {
		v := *src.Tolerance
		dst.Tolerance = &v
	}
}

// MetricObservation is what the controller can see for one metric at a sync.
type MetricObservation struct {
	// Pods is used by PodsAverage metrics.
	Pods PodsInput
	// Total is the metric value used by External metrics (the sum over all
	// matched series).
	Total float64
	// Err is set when the metric could not be fetched at all.
	Err error
}

// Observation is the state of the scale target and its metrics at a sync.
type Observation struct {
	// SpecReplicas is scale.spec.replicas: the currently desired count.
	SpecReplicas int32
	// StatusReplicas is scale.status.replicas: pods that exist and are not
	// being deleted.
	StatusReplicas int32
	// TotalPods is the number of pods matched by the selector, in any state.
	TotalPods int
	// ReadyPods is the number of pods that are Running and Ready, including
	// terminating pods that are still Ready.
	ReadyPods int
	// Metrics is aligned with Spec.Metrics.
	Metrics []MetricObservation
}

// MetricStatus is the outcome of evaluating one metric.
type MetricStatus struct {
	Name     string
	Kind     MetricKind
	Target   float64
	Usage    float64
	Proposal int32
	Err      error
}

// Decision is the outcome of one reconcile.
type Decision struct {
	Time            float64
	CurrentReplicas int32
	// DesiredReplicas is the value written to scale.spec.replicas when
	// Rescale is true; otherwise it equals CurrentReplicas (or 0 when
	// scaling is disabled).
	DesiredReplicas int32
	Rescale         bool
	// Reason is the text of the SuccessfulRescale event, without the
	// "New size: N; reason: " prefix.
	Reason string
	// MetricProposal is the largest per-metric proposal before
	// stabilisation and rate limiting, or -1 when none could be computed.
	MetricProposal int32
	// AbleToScale and ScalingLimited are the reasons of the conditions of
	// the same names; ScalingLimitedActive says whether ScalingLimited is
	// True.
	AbleToScale          string
	ScalingLimited       string
	ScalingLimitedActive bool
	// Disabled is true when scaling is inactive because the target has zero
	// replicas (ScalingActive=False, reason ScalingDisabled).
	Disabled bool
	Metrics  []MetricStatus
	// Err is the error of the first invalid metric, if any.
	Err error
}

type recommendation struct {
	replicas int32
	time     float64
}

type scaleEvent struct {
	change   int32
	time     float64
	outdated bool
}

// Controller holds the per-HPA state the upstream controller keeps in memory:
// past recommendations and past scale events.
type Controller struct {
	spec Spec
	opts Options

	behavior *Behavior // defaulted copy, nil when spec.behavior is absent

	recs         []recommendation
	upEvents     []scaleEvent
	downEvents   []scaleEvent
	scaledToZero bool
	started      bool
}

// NewController validates the spec and returns a controller for it.
func NewController(spec Spec, opts Options) (*Controller, error) {
	if spec.MaxReplicas < 1 {
		return nil, errors.New("hpa: maxReplicas must be at least 1")
	}
	if spec.MinReplicas < 0 {
		return nil, errors.New("hpa: minReplicas must not be negative")
	}
	if spec.MinReplicas > spec.MaxReplicas {
		return nil, errors.New("hpa: minReplicas must not exceed maxReplicas")
	}
	if len(spec.Metrics) == 0 {
		return nil, errors.New("hpa: at least one metric is required")
	}
	if spec.MinReplicas == 0 && !spec.HasExternalMetrics() {
		return nil, errors.New("hpa: minReplicas 0 requires at least one External metric")
	}
	for _, m := range spec.Metrics {
		if m.Target <= 0 {
			return nil, fmt.Errorf("hpa: metric %q: target must be greater than zero", m.Name)
		}
	}
	if opts.SyncPeriodSeconds <= 0 || opts.Tolerance < 0 || opts.DownscaleStabilizationSeconds < 0 {
		return nil, errors.New("hpa: invalid controller options")
	}
	c := &Controller{spec: spec, opts: opts, behavior: DefaultBehavior(spec.Behavior)}
	if c.behavior != nil {
		if c.behavior.ScaleDown.StabilizationWindowSeconds == nil {
			w := int32(opts.DownscaleStabilizationSeconds)
			c.behavior.ScaleDown.StabilizationWindowSeconds = &w
		}
		for _, r := range []*ScalingRules{c.behavior.ScaleUp, c.behavior.ScaleDown} {
			switch r.SelectPolicy {
			case SelectMax, SelectMin, SelectDisabled:
			default:
				return nil, fmt.Errorf("hpa: unknown selectPolicy %q", r.SelectPolicy)
			}
			for _, p := range r.Policies {
				if p.Type != PodsPolicy && p.Type != PercentPolicy {
					return nil, fmt.Errorf("hpa: unknown policy type %q", p.Type)
				}
				if p.PeriodSeconds <= 0 || p.PeriodSeconds > 1800 {
					return nil, fmt.Errorf("hpa: policy periodSeconds must be in 1..1800, got %d", p.PeriodSeconds)
				}
				if p.Value <= 0 {
					return nil, fmt.Errorf("hpa: policy value must be greater than zero, got %d", p.Value)
				}
			}
		}
	}
	return c, nil
}

// Spec returns the spec the controller was built from.
func (c *Controller) Spec() Spec { return c.spec }

// Options returns the controller options.
func (c *Controller) Options() Options { return c.opts }

// Behavior returns the behaviour in effect after defaulting, or nil when
// spec.behavior is absent.
func (c *Controller) Behavior() *Behavior { return c.behavior }

// Tolerances returns the tolerances in effect for this HPA.
func (c *Controller) Tolerances() Tolerances {
	t := Tolerances{ScaleDown: c.opts.Tolerance, ScaleUp: c.opts.Tolerance}
	if c.behavior == nil {
		return t
	}
	if c.behavior.ScaleDown.Tolerance != nil {
		t.ScaleDown = *c.behavior.ScaleDown.Tolerance
	}
	if c.behavior.ScaleUp.Tolerance != nil {
		t.ScaleUp = *c.behavior.ScaleUp.Tolerance
	}
	return t
}

// ScaledToZero reports whether the controller itself scaled the target to
// zero (the ScaledToZero condition).
func (c *Controller) ScaledToZero() bool { return c.scaledToZero }

// Reconcile runs one iteration of the control loop at time now (seconds on
// any monotonic clock) and returns the decision. When the decision is a
// rescale, the controller assumes the scale update succeeded and records it.
func (c *Controller) Reconcile(now float64, obs Observation) Decision {
	current := obs.SpecReplicas
	d := Decision{Time: now, CurrentReplicas: current, DesiredReplicas: current, MetricProposal: -1}
	if !c.started {
		// recordInitialRecommendation
		c.recs = append(c.recs, recommendation{replicas: current, time: now})
		c.started = true
	}

	minReplicas := c.spec.MinReplicas
	canScaleFromZero := c.scaledToZero && c.spec.HasExternalMetrics()
	needsMetrics := true
	var desired int32

	switch {
	case current == 0:
		if (minReplicas != 0 && c.scaledToZero) || canScaleFromZero {
			break
		}
		d.Disabled = true
		d.DesiredReplicas = 0
		d.AbleToScale = "ScalingDisabled"
		return d
	case current > c.spec.MaxReplicas:
		d.Reason = "Current number of replicas above Spec.MaxReplicas"
		desired = c.spec.MaxReplicas
		needsMetrics = false
	case current < minReplicas:
		d.Reason = "Current number of replicas below Spec.MinReplicas"
		desired = minReplicas
		needsMetrics = false
	}

	if needsMetrics {
		proposal, metricName, statuses, err := c.computeReplicasForMetrics(obs)
		d.Metrics = statuses
		d.Err = err
		if proposal == -1 {
			// All metrics invalid, or some invalid while the rest ask for a
			// scale-down: no scaling this round.
			d.AbleToScale = "FailedComputeMetricsReplicas"
			return d
		}
		d.MetricProposal = proposal
		rescaleMetric := ""
		if proposal > desired {
			desired = proposal
			rescaleMetric = metricName
		}
		if desired > current {
			d.Reason = rescaleMetric + " above target"
		}
		if desired < current {
			d.Reason = "All metrics below target"
		}
		if c.behavior == nil {
			desired = c.normalize(now, &d, current, desired)
		} else {
			desired = c.normalizeWithBehaviors(now, &d, current, desired)
		}
		if current == 0 && minReplicas != 0 && c.scaledToZero && desired < minReplicas {
			desired = minReplicas
			if d.Reason == "" {
				d.Reason = "Current number of replicas below Spec.MinReplicas"
			}
		}
	}

	d.DesiredReplicas = desired
	d.Rescale = desired != current
	if d.Rescale {
		c.storeScaleEvent(now, current, desired)
		if current > 0 && desired == 0 && minReplicas == 0 && c.spec.HasExternalMetrics() {
			c.scaledToZero = true
		} else {
			c.scaledToZero = false
		}
	} else {
		d.Reason = ""
	}
	return d
}

func (c *Controller) computeReplicasForMetrics(obs Observation) (replicas int32, metric string, statuses []MetricStatus, err error) {
	tol := c.Tolerances()
	statuses = make([]MetricStatus, len(c.spec.Metrics))
	invalid := 0
	var firstErr error
	for i, m := range c.spec.Metrics {
		st := MetricStatus{Name: m.Name, Kind: m.Kind, Target: m.Target}
		var mo MetricObservation
		if i < len(obs.Metrics) {
			mo = obs.Metrics[i]
		} else {
			mo.Err = errors.New("no observation for metric")
		}
		var proposal int32
		var usage float64
		perr := mo.Err
		if perr == nil {
			switch m.Kind {
			case PodsAverage:
				proposal, usage, perr = PodsMetricReplicas(obs.SpecReplicas, m.Target, mo.Pods, tol)
			case ExternalValue:
				proposal, usage, perr = ExternalValueReplicas(obs.SpecReplicas, m.Target, mo.Total, obs.TotalPods, obs.ReadyPods, tol)
			case ExternalAverage:
				proposal, usage, perr = ExternalAverageReplicas(obs.StatusReplicas, m.Target, mo.Total, tol)
			default:
				perr = fmt.Errorf("unsupported metric kind %d", m.Kind)
			}
		}
		st.Usage, st.Proposal, st.Err = usage, proposal, perr
		statuses[i] = st
		if perr != nil {
			if invalid == 0 {
				firstErr = perr
			}
			invalid++
			continue
		}
		if replicas == 0 || proposal > replicas {
			replicas = proposal
			metric = m.Describe()
		}
	}
	if firstErr != nil {
		firstErr = fmt.Errorf("invalid metrics (%d invalid out of %d), first error is: %w", invalid, len(c.spec.Metrics), firstErr)
	}
	if invalid >= len(c.spec.Metrics) || (invalid > 0 && replicas < obs.SpecReplicas) {
		return -1, "", statuses, firstErr
	}
	return replicas, metric, statuses, firstErr
}

// normalize is the path taken when spec.behavior is absent: a rolling maximum
// over the downscale stabilisation window, then the fixed scale-up limit of
// max(2 x current, 4) and the min/max bounds.
func (c *Controller) normalize(now float64, d *Decision, current, proposal int32) int32 {
	stabilized := c.stabilizeRecommendation(now, proposal)
	if stabilized != proposal {
		d.AbleToScale = "ScaleDownStabilized"
	} else {
		d.AbleToScale = "ReadyForNewScale"
	}
	desired, reason := convertDesiredReplicasWithRules(current, stabilized, c.spec.MinReplicas, c.spec.MaxReplicas)
	d.ScalingLimited = reason
	d.ScalingLimitedActive = desired != stabilized
	return desired
}

func (c *Controller) stabilizeRecommendation(now float64, proposal int32) int32 {
	maxRec := proposal
	found := false
	oldIdx := 0
	cutoff := now - c.opts.DownscaleStabilizationSeconds
	for i, rec := range c.recs {
		if rec.time < cutoff {
			found = true
			oldIdx = i
		} else if rec.replicas > maxRec {
			maxRec = rec.replicas
		}
	}
	if found {
		c.recs[oldIdx] = recommendation{proposal, now}
	} else {
		c.recs = append(c.recs, recommendation{proposal, now})
	}
	return maxRec
}

func convertDesiredReplicasWithRules(current, desired, minReplicas, maxReplicas int32) (int32, string) {
	// Do not scale up too much at once: at most double, or to 4.
	scaleUpLimit := int32(math.Max(2.0*float64(current), 4.0))
	var maxAllowed int32
	var limitReason string
	if maxReplicas > scaleUpLimit {
		maxAllowed = scaleUpLimit
		limitReason = "ScaleUpLimit"
	} else {
		maxAllowed = maxReplicas
		limitReason = "TooManyReplicas"
	}
	if desired < minReplicas {
		return minReplicas, "TooFewReplicas"
	} else if desired > maxAllowed {
		return maxAllowed, limitReason
	}
	return desired, "DesiredWithinRange"
}

func (c *Controller) normalizeWithBehaviors(now float64, d *Decision, current, proposal int32) int32 {
	stabilized, reason := c.stabilizeRecommendationWithBehaviors(now, current, proposal)
	if stabilized != proposal {
		d.AbleToScale = reason
	} else {
		d.AbleToScale = "ReadyForNewScale"
	}
	desired, limitReason := c.convertDesiredReplicasWithBehaviorRate(now, current, stabilized)
	d.ScalingLimited = limitReason
	d.ScalingLimitedActive = desired != stabilized
	return desired
}

func (c *Controller) stabilizeRecommendationWithBehaviors(now float64, current, proposal int32) (int32, string) {
	found := false
	oldIdx := 0

	upRec := proposal
	upCutoff := now - float64(*c.behavior.ScaleUp.StabilizationWindowSeconds)
	downRec := proposal
	downCutoff := now - float64(*c.behavior.ScaleDown.StabilizationWindowSeconds)

	for i, rec := range c.recs {
		if rec.time > upCutoff {
			upRec = min(rec.replicas, upRec)
		}
		if rec.time > downCutoff {
			downRec = max(rec.replicas, downRec)
		}
		if rec.time < upCutoff && rec.time < downCutoff {
			found = true
			oldIdx = i
		}
	}

	// Bring the recommendation to within the upper and lower limits.
	rec := current
	if rec < upRec {
		rec = upRec
	}
	if rec > downRec {
		rec = downRec
	}

	// Record the unstabilised recommendation.
	if found {
		c.recs[oldIdx] = recommendation{proposal, now}
	} else {
		c.recs = append(c.recs, recommendation{proposal, now})
	}

	if proposal >= current {
		return rec, "ScaleUpStabilized"
	}
	return rec, "ScaleDownStabilized"
}

func (c *Controller) convertDesiredReplicasWithBehaviorRate(now float64, current, desired int32) (int32, string) {
	switch {
	case desired > current:
		limit := c.scaleUpLimit(now, current)
		if limit < current {
			// Do not scale up further until old scale-up events expire.
			limit = current
		}
		maxAllowed := c.spec.MaxReplicas
		reason := "TooManyReplicas"
		if maxAllowed > limit {
			maxAllowed = limit
			reason = "ScaleUpLimit"
		}
		if desired > maxAllowed {
			return maxAllowed, reason
		}
	case desired < current:
		limit := c.scaleDownLimit(now, current)
		if limit > current {
			limit = current
		}
		minAllowed := c.spec.MinReplicas
		reason := "TooFewReplicas"
		if minAllowed < limit {
			minAllowed = limit
			reason = "ScaleDownLimit"
		}
		if desired < minAllowed {
			return minAllowed, reason
		}
	}
	return desired, "DesiredWithinRange"
}

func replicasChangePerPeriod(now float64, periodSeconds int32, events []scaleEvent) int32 {
	cutoff := now - float64(periodSeconds)
	var n int32
	for _, e := range events {
		if e.time > cutoff {
			n += e.change
		}
	}
	return n
}

func (c *Controller) scaleUpLimit(now float64, current int32) int32 {
	rules := c.behavior.ScaleUp
	var result int32
	var pick func(a, b int32) int32
	switch rules.SelectPolicy {
	case SelectDisabled:
		return current
	case SelectMin:
		result = math.MaxInt32
		pick = func(a, b int32) int32 { return min(a, b) }
	default:
		result = math.MinInt32
		pick = func(a, b int32) int32 { return max(a, b) }
	}
	for _, p := range rules.Policies {
		added := replicasChangePerPeriod(now, p.PeriodSeconds, c.upEvents)
		removed := replicasChangePerPeriod(now, p.PeriodSeconds, c.downEvents)
		periodStart := current - added + removed
		var proposed int32
		if p.Type == PodsPolicy {
			proposed = periodStart + p.Value
		} else {
			// Rounded up, otherwise a small percentage could never add a pod.
			proposed = int32(math.Ceil(float64(periodStart) * (1 + float64(p.Value)/100)))
		}
		result = pick(result, proposed)
	}
	return result
}

func (c *Controller) scaleDownLimit(now float64, current int32) int32 {
	rules := c.behavior.ScaleDown
	var result int32
	var pick func(a, b int32) int32
	switch rules.SelectPolicy {
	case SelectDisabled:
		return current
	case SelectMin:
		// For scaling down, the smallest change is the largest value.
		result = math.MinInt32
		pick = func(a, b int32) int32 { return max(a, b) }
	default:
		result = math.MaxInt32
		pick = func(a, b int32) int32 { return min(a, b) }
	}
	for _, p := range rules.Policies {
		added := replicasChangePerPeriod(now, p.PeriodSeconds, c.upEvents)
		removed := replicasChangePerPeriod(now, p.PeriodSeconds, c.downEvents)
		periodStart := current - added + removed
		var proposed int32
		if p.Type == PodsPolicy {
			proposed = periodStart - p.Value
		} else {
			proposed = int32(float64(periodStart) * (1 - float64(p.Value)/100))
		}
		result = pick(result, proposed)
	}
	return result
}

func longestPolicyPeriod(r *ScalingRules) int32 {
	var longest int32
	for _, p := range r.Policies {
		if p.PeriodSeconds > longest {
			longest = p.PeriodSeconds
		}
	}
	return longest
}

func (c *Controller) storeScaleEvent(now float64, prev, next int32) {
	if c.behavior == nil {
		return // events are only used by the behaviour path
	}
	store := func(events []scaleEvent, period int32, change int32) []scaleEvent {
		cutoff := now - float64(period)
		idx := -1
		for i := range events {
			if events[i].time < cutoff {
				events[i].outdated = true
			}
			if events[i].outdated {
				idx = i
			}
		}
		ev := scaleEvent{change: change, time: now}
		if idx >= 0 {
			events[idx] = ev
			return events
		}
		return append(events, ev)
	}
	if next > prev {
		c.upEvents = store(c.upEvents, longestPolicyPeriod(c.behavior.ScaleUp), next-prev)
	} else {
		c.downEvents = store(c.downEvents, longestPolicyPeriod(c.behavior.ScaleDown), prev-next)
	}
}
