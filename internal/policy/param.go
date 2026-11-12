package policy

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/cb-Sigma/scale-backtest/internal/hpa"
)

// Params lists the parameter names Set understands, for help texts.
var Params = []string{
	"target", "target[i]", "minReplicas", "maxReplicas",
	"scaleUp.stabilizationWindowSeconds", "scaleDown.stabilizationWindowSeconds",
	"scaleUp.tolerance", "scaleDown.tolerance", "tolerance",
	"pollingInterval", "cooldownPeriod", "activationThreshold", "activationThreshold[i]",
	"replicas",
}

func splitIndex(name string) (base string, idx int, err error) {
	open := strings.IndexByte(name, '[')
	if open < 0 {
		return name, 0, nil
	}
	if !strings.HasSuffix(name, "]") {
		return "", 0, fmt.Errorf("parameter %q: missing ]", name)
	}
	idx, err = strconv.Atoi(name[open+1 : len(name)-1])
	if err != nil || idx < 0 {
		return "", 0, fmt.Errorf("parameter %q: bad index", name)
	}
	return name[:open], idx, nil
}

func wholeNumber(name string, v float64) (int32, error) {
	if v != math.Trunc(v) || v < 0 || v > math.MaxInt32 {
		return 0, fmt.Errorf("parameter %s: %g is not a non-negative whole number", name, v)
	}
	return int32(v), nil
}

// Set changes one parameter of the policy in place. It is how sweeps vary a
// policy; the names follow the manifest fields.
//
// Setting a behavior field on a policy without spec.behavior adds the
// behavior block with its API defaults, exactly as editing the manifest
// would.
func (p *Policy) Set(name string, value float64) error {
	base, idx, err := splitIndex(name)
	if err != nil {
		return err
	}
	key := strings.ToLower(base)
	if p.Kind == KindStatic {
		if key != "replicas" {
			return fmt.Errorf("parameter %s: a static policy only has \"replicas\"", name)
		}
		n, err := wholeNumber(name, value)
		if err != nil || n < 1 {
			return fmt.Errorf("parameter replicas: need a whole number of at least 1, got %g", value)
		}
		p.StaticReplicas = n
		p.Name = fmt.Sprintf("static-%d", n)
		return nil
	}

	rules := func(up bool) *hpa.ScalingRules {
		b := p.HPA.Behavior
		if p.KEDA != nil {
			b = p.KEDA.Behavior
		}
		if b == nil {
			b = &hpa.Behavior{}
		}
		if b.ScaleUp == nil {
			b.ScaleUp = &hpa.ScalingRules{}
		}
		if b.ScaleDown == nil {
			b.ScaleDown = &hpa.ScalingRules{}
		}
		if p.KEDA != nil {
			p.KEDA.Behavior = b
		}
		p.HPA.Behavior = b
		if up {
			return b.ScaleUp
		}
		return b.ScaleDown
	}

	switch key {
	case "target", "threshold":
		if idx >= len(p.HPA.Metrics) {
			return fmt.Errorf("parameter %s: the policy has %d metric(s)", name, len(p.HPA.Metrics))
		}
		if value <= 0 {
			return fmt.Errorf("parameter %s: must be greater than zero", name)
		}
		p.HPA.Metrics[idx].Target = value
		if p.KEDA != nil {
			p.KEDA.Triggers[idx].Threshold = value
		}
	case "minreplicas", "min", "minreplicacount":
		n, err := wholeNumber(name, value)
		if err != nil {
			return err
		}
		if p.KEDA != nil {
			p.KEDA.MinReplicaCount = n
		} else {
			p.HPA.MinReplicas = n
		}
	case "maxreplicas", "max", "maxreplicacount":
		n, err := wholeNumber(name, value)
		if err != nil {
			return err
		}
		if p.KEDA != nil {
			p.KEDA.MaxReplicaCount = n
		} else {
			p.HPA.MaxReplicas = n
		}
	case "scaleup.stabilizationwindowseconds", "scaledown.stabilizationwindowseconds":
		n, err := wholeNumber(name, value)
		if err != nil {
			return err
		}
		if n > 3600 {
			return fmt.Errorf("parameter %s: must be at most 3600", name)
		}
		rules(strings.HasPrefix(key, "scaleup")).StabilizationWindowSeconds = &n
	case "scaleup.tolerance", "scaledown.tolerance", "tolerance":
		if value < 0 {
			return fmt.Errorf("parameter %s: must not be negative", name)
		}
		if key != "scaledown.tolerance" {
			v := value
			rules(true).Tolerance = &v
		}
		if key != "scaleup.tolerance" {
			v := value
			rules(false).Tolerance = &v
		}
	case "pollinginterval", "cooldownperiod", "activationthreshold":
		if p.KEDA == nil {
			return fmt.Errorf("parameter %s only exists on a KEDA ScaledObject", name)
		}
		switch key {
		case "pollinginterval":
			if value <= 0 {
				return fmt.Errorf("parameter %s: must be greater than zero", name)
			}
			p.KEDA.PollingInterval = value
		case "cooldownperiod":
			if value < 0 {
				return fmt.Errorf("parameter %s: must not be negative", name)
			}
			p.KEDA.CooldownPeriod = value
		default:
			if idx >= len(p.KEDA.Triggers) {
				return fmt.Errorf("parameter %s: the ScaledObject has %d trigger(s)", name, len(p.KEDA.Triggers))
			}
			p.KEDA.Triggers[idx].ActivationThreshold = value
		}
	default:
		return fmt.Errorf("unknown parameter %q (known: %s)", name, strings.Join(Params, ", "))
	}

	if p.KEDA != nil {
		if err := p.KEDA.Validate(); err != nil {
			return fmt.Errorf("parameter %s=%g: %w", name, value, err)
		}
		p.HPA = p.KEDA.HPASpec()
	}
	if _, err := hpa.NewController(p.HPA, hpa.DefaultOptions()); err != nil {
		return fmt.Errorf("parameter %s=%g: %w", name, value, err)
	}
	return nil
}
