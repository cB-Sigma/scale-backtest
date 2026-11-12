// Package hpa is a deterministic re-implementation of the Kubernetes
// HorizontalPodAutoscaler control loop for use inside a simulator.
//
// It follows the algorithm described in the Kubernetes documentation
// (https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/)
// and, where the documentation leaves details open, the upstream controller
// (pkg/controller/podautoscaler in kubernetes/kubernetes). It has no clock and
// no API client: the caller passes the current time and what the controller
// would have observed, and receives the decision.
package hpa

import (
	"errors"
	"math"
)

// Tolerances are the scale-down and scale-up tolerances on the usage ratio.
// The cluster-wide default is 0.1 in both directions
// (--horizontal-pod-autoscaler-tolerance); behavior.scaleUp.tolerance and
// behavior.scaleDown.tolerance override it per direction.
type Tolerances struct {
	ScaleDown float64
	ScaleUp   float64
}

// IsWithin reports whether a usage ratio is close enough to 1.0 that the
// controller skips scaling.
func (t Tolerances) IsWithin(usageRatio float64) bool {
	return (1.0-t.ScaleDown) <= usageRatio && usageRatio <= (1.0+t.ScaleUp)
}

// Errors returned by the replica calculators. They mirror the situations in
// which the upstream controller cannot turn a metric into a replica count.
var (
	ErrNoPods       = errors.New("no pods returned by selector while calculating replica count")
	ErrNoPodMetrics = errors.New("did not receive metrics for targeted pods (pods might be unready)")
	ErrBadTarget    = errors.New("metric target must be greater than zero")
)

// milli converts a value to Kubernetes milli-units. The controller works on
// resource.Quantity milli-values (int64), so averages are integer divisions;
// reproducing that keeps borderline tolerance decisions identical.
func milli(v float64) int64 {
	return int64(math.Round(v * 1000))
}

func ceilToInt32(f float64) int32 {
	c := math.Ceil(f)
	if c >= math.MaxInt32 {
		return math.MaxInt32
	}
	if c <= math.MinInt32 || math.IsNaN(c) {
		if math.IsNaN(c) {
			return 0
		}
		return math.MinInt32
	}
	return int32(c)
}

// usageRatio returns sum(values)/len(values)/target using integer milli
// arithmetic, plus the average usage.
func usageRatio(valuesMilli []int64, targetMilli int64) (ratio float64, usageMilli int64) {
	var total int64
	for _, v := range valuesMilli {
		total += v
	}
	usageMilli = total / int64(len(valuesMilli))
	return float64(usageMilli) / float64(targetMilli), usageMilli
}

// PodsInput is what the controller sees for one per-pod (type: Pods) metric.
type PodsInput struct {
	// Values holds the metric of every pod that is running, not being deleted
	// and has a metric sample.
	Values []float64
	// Unready is the number of pods in phase Pending. The controller sets
	// them aside and, on a scale-up, counts them as consuming 0.
	Unready int
	// Missing is the number of running pods that have no metric sample. They
	// are counted as 0 on a scale-up and as exactly the target on a
	// scale-down.
	Missing int
}

// PodsMetricReplicas computes the replica proposal for a per-pod metric with
// an AverageValue target. It is a port of calcPlainMetricReplicas.
//
// Pods that are being deleted or have failed must not be included in the
// input at all: the controller ignores them.
func PodsMetricReplicas(currentReplicas int32, target float64, in PodsInput, tol Tolerances) (replicas int32, usage float64, err error) {
	targetMilli := milli(target)
	if targetMilli <= 0 {
		return 0, 0, ErrBadTarget
	}
	if len(in.Values)+in.Unready+in.Missing == 0 {
		return 0, 0, ErrNoPods
	}
	if len(in.Values) == 0 {
		return 0, 0, ErrNoPodMetrics
	}
	metrics := make([]int64, 0, len(in.Values)+in.Unready+in.Missing)
	for _, v := range in.Values {
		metrics = append(metrics, milli(v))
	}
	readyPodCount := len(in.Values)

	ratio, usageMilli := usageRatio(metrics, targetMilli)
	usage = float64(usageMilli) / 1000

	scaleUpWithUnready := in.Unready > 0 && ratio > 1.0
	if !scaleUpWithUnready && in.Missing == 0 {
		if tol.IsWithin(ratio) {
			return currentReplicas, usage, nil
		}
		return ceilToInt32(ratio * float64(readyPodCount)), usage, nil
	}

	if in.Missing > 0 {
		switch {
		case ratio < 1.0:
			// On a scale-down, treat missing pods as using exactly the target.
			for range in.Missing {
				metrics = append(metrics, targetMilli)
			}
		case ratio > 1.0:
			// On a scale-up, treat missing pods as using nothing.
			for range in.Missing {
				metrics = append(metrics, 0)
			}
		}
	}
	if scaleUpWithUnready {
		// On a scale-up, treat not-yet-ready pods as using nothing.
		for range in.Unready {
			metrics = append(metrics, 0)
		}
	}

	newRatio, _ := usageRatio(metrics, targetMilli)
	if tol.IsWithin(newRatio) || (ratio < 1.0 && newRatio > 1.0) || (ratio > 1.0 && newRatio < 1.0) {
		// The change would be too small, or the conservative assumptions
		// reverse the scale direction.
		return currentReplicas, usage, nil
	}
	newReplicas := ceilToInt32(newRatio * float64(len(metrics)))
	if (newRatio < 1.0 && newReplicas > currentReplicas) || (newRatio > 1.0 && newReplicas < currentReplicas) {
		return currentReplicas, usage, nil
	}
	return newReplicas, usage, nil
}

// ExternalValueReplicas computes the proposal for an External metric with a
// Value target: the metric total is compared with the target directly and the
// ratio is applied to the number of ready pods. It is a port of
// GetExternalMetricReplicas / getUsageRatioReplicaCount.
//
// totalPods is the number of pods matched by the selector (any state);
// readyPods is the number that are Running and Ready.
func ExternalValueReplicas(currentReplicas int32, target, total float64, totalPods, readyPods int, tol Tolerances) (replicas int32, usage float64, err error) {
	targetMilli := milli(target)
	if targetMilli <= 0 {
		return 0, 0, ErrBadTarget
	}
	usageMilli := milli(total)
	usage = float64(usageMilli) / 1000
	ratio := float64(usageMilli) / float64(targetMilli)
	if currentReplicas != 0 {
		if tol.IsWithin(ratio) {
			return currentReplicas, usage, nil
		}
		if totalPods == 0 {
			return 0, 0, ErrNoPods
		}
		return ceilToInt32(ratio * float64(readyPods)), usage, nil
	}
	// Scale to zero or n pods depending on the ratio.
	return ceilToInt32(ratio), usage, nil
}

// ExternalAverageReplicas computes the proposal for an External metric with
// an AverageValue target: the metric total divided by the per-pod target,
// rounded up, unless the per-pod usage is within tolerance. It is a port of
// GetExternalPerPodMetricReplicas / getPerPodReplicas.
//
// statusReplicas is the scale subresource's status.replicas. usage is the
// per-pod usage the controller would report in status.
func ExternalAverageReplicas(statusReplicas int32, targetPerPod, total float64, tol Tolerances) (replicas int32, usage float64, err error) {
	targetMilli := milli(targetPerPod)
	if targetMilli <= 0 {
		return 0, 0, ErrBadTarget
	}
	usageMilli := milli(total)
	if statusReplicas != 0 {
		usage = math.Ceil(float64(usageMilli)/float64(statusReplicas)) / 1000
		ratio := float64(usageMilli) / (float64(targetMilli) * float64(statusReplicas))
		if tol.IsWithin(ratio) {
			return statusReplicas, usage, nil
		}
	}
	return ceilToInt32(float64(usageMilli) / float64(targetMilli)), usage, nil
}
