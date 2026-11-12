// Package policy loads autoscaling policies from the manifests people
// actually deploy: autoscaling/v2 HorizontalPodAutoscaler objects and KEDA
// ScaledObjects. Anything in a manifest that the simulator cannot honour is
// refused or reported, never silently dropped.
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"

	"github.com/cb-Sigma/scale-backtest/internal/hpa"
	"github.com/cb-Sigma/scale-backtest/internal/keda"
	"github.com/cb-Sigma/scale-backtest/internal/metrics"
	"github.com/cb-Sigma/scale-backtest/internal/promql"
)

// Kind is the kind of policy.
type Kind string

// Policy kinds.
const (
	KindHPA    Kind = "HorizontalPodAutoscaler"
	KindKEDA   Kind = "ScaledObject"
	KindStatic Kind = "Static"
)

// MetricSource says where the value of one HPA metric comes from in the
// simulation.
type MetricSource struct {
	// Ref is the simulated metric.
	Ref metrics.Ref
	// Query is set for KEDA prometheus triggers.
	Query *promql.Query
}

// Policy is a loaded policy.
type Policy struct {
	Kind Kind
	// Name is metadata.name (or "static-N").
	Name string
	// Path is where the policy was read from.
	Path string
	// HPA is the HPA spec: the manifest itself, or the HPA KEDA generates.
	HPA hpa.Spec
	// Sources is aligned with HPA.Metrics.
	Sources []MetricSource
	// KEDA is set for ScaledObjects.
	KEDA *keda.Spec
	// StaticReplicas is the fixed replica count of a static policy.
	StaticReplicas int32
	// Warnings lists manifest content that is accepted but not simulated.
	Warnings []string
}

// Label returns a short human-readable label.
func (p *Policy) Label() string {
	switch p.Kind {
	case KindStatic:
		return fmt.Sprintf("static %d replicas", p.StaticReplicas)
	case KindKEDA:
		return "KEDA ScaledObject " + p.Name
	}
	return "HPA " + p.Name
}

// Clone returns a deep copy, so that sweeps can modify parameters.
func (p *Policy) Clone() *Policy {
	c := *p
	c.HPA.Metrics = append([]hpa.MetricSpec(nil), p.HPA.Metrics...)
	c.HPA.Behavior = cloneBehavior(p.HPA.Behavior)
	c.Sources = append([]MetricSource(nil), p.Sources...)
	c.Warnings = append([]string(nil), p.Warnings...)
	if p.KEDA != nil {
		k := *p.KEDA
		k.Triggers = append([]keda.Trigger(nil), p.KEDA.Triggers...)
		k.Behavior = cloneBehavior(p.KEDA.Behavior)
		if p.KEDA.IdleReplicaCount != nil {
			v := *p.KEDA.IdleReplicaCount
			k.IdleReplicaCount = &v
		}
		c.KEDA = &k
	}
	return &c
}

func cloneBehavior(b *hpa.Behavior) *hpa.Behavior {
	if b == nil {
		return nil
	}
	out := &hpa.Behavior{}
	cp := func(r *hpa.ScalingRules) *hpa.ScalingRules {
		if r == nil {
			return nil
		}
		n := *r
		n.Policies = append([]hpa.ScalingPolicy(nil), r.Policies...)
		if r.StabilizationWindowSeconds != nil {
			v := *r.StabilizationWindowSeconds
			n.StabilizationWindowSeconds = &v
		}
		if r.Tolerance != nil {
			v := *r.Tolerance
			n.Tolerance = &v
		}
		return &n
	}
	out.ScaleUp, out.ScaleDown = cp(b.ScaleUp), cp(b.ScaleDown)
	return out
}

// Static returns a policy that keeps a fixed number of replicas.
func Static(n int32) *Policy {
	return &Policy{Kind: KindStatic, Name: fmt.Sprintf("static-%d", n), StaticReplicas: n}
}

// Load reads a policy. spec is a path to a manifest, or "static:N".
func Load(spec string, reg *metrics.Registry) (*Policy, error) {
	if rest, ok := strings.CutPrefix(spec, "static:"); ok {
		n, err := strconv.Atoi(rest)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("policy %q: static:N needs a replica count of at least 1", spec)
		}
		return Static(int32(n)), nil
	}
	data, err := os.ReadFile(spec)
	if err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	p, err := Parse(data, reg)
	if err != nil {
		return nil, fmt.Errorf("policy %s: %w", spec, err)
	}
	p.Path = spec
	return p, nil
}

type typeMeta struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

// Parse parses a manifest. If the input holds several YAML documents, the
// first HorizontalPodAutoscaler or ScaledObject is used and the others are
// reported in Warnings.
func Parse(data []byte, reg *metrics.Registry) (*Policy, error) {
	if reg == nil {
		reg = metrics.NewRegistry()
	}
	var chosen []byte
	var chosenMeta typeMeta
	var skipped []string
	for _, doc := range splitDocuments(data) {
		var tm typeMeta
		if err := yaml.Unmarshal(doc, &tm); err != nil {
			return nil, fmt.Errorf("not valid YAML: %w", err)
		}
		if tm.Kind == "" {
			continue
		}
		isPolicy := tm.Kind == "HorizontalPodAutoscaler" || tm.Kind == "ScaledObject"
		if isPolicy && chosen == nil {
			chosen, chosenMeta = doc, tm
			continue
		}
		skipped = append(skipped, tm.Kind)
	}
	if chosen == nil {
		return nil, errors.New("no HorizontalPodAutoscaler or ScaledObject found in the manifest")
	}
	var p *Policy
	var err error
	switch chosenMeta.Kind {
	case "HorizontalPodAutoscaler":
		if chosenMeta.APIVersion != "autoscaling/v2" {
			return nil, fmt.Errorf("HorizontalPodAutoscaler apiVersion %q is not supported; convert the manifest to autoscaling/v2", chosenMeta.APIVersion)
		}
		p, err = parseHPA(chosen, reg)
	case "ScaledObject":
		if chosenMeta.APIVersion != "keda.sh/v1alpha1" {
			return nil, fmt.Errorf("ScaledObject apiVersion %q is not supported; expected keda.sh/v1alpha1", chosenMeta.APIVersion)
		}
		p, err = parseScaledObject(chosen, reg)
	}
	if err != nil {
		return nil, err
	}
	for _, k := range skipped {
		p.Warnings = append(p.Warnings, fmt.Sprintf("ignored additional %s document in the manifest", k))
	}
	return p, nil
}

func splitDocuments(data []byte) [][]byte {
	var docs [][]byte
	var cur bytes.Buffer
	flush := func() {
		if len(bytes.TrimSpace(cur.Bytes())) > 0 {
			docs = append(docs, append([]byte(nil), cur.Bytes()...))
		}
		cur.Reset()
	}
	for _, line := range bytes.SplitAfter(data, []byte("\n")) {
		trimmed := bytes.TrimRight(line, "\r\n")
		if bytes.Equal(trimmed, []byte("---")) || bytes.HasPrefix(trimmed, []byte("--- ")) {
			flush()
			continue
		}
		cur.Write(line)
	}
	flush()
	return docs
}

// strictDecode decodes a YAML document the way the Kubernetes API server
// does with strict field validation: field names are case-sensitive, and
// unknown or duplicate fields are errors.
func strictDecode(doc []byte, into any) error {
	data, err := yaml.YAMLToJSONStrict(doc)
	if err != nil {
		return err
	}
	strictErrs, err := kjson.UnmarshalStrict(data, into)
	if err != nil {
		return err
	}
	return errors.Join(strictErrs...)
}

func quantity(q *resource.Quantity) float64 {
	return q.AsApproximateFloat64()
}

func selectorText(sel *metav1.LabelSelector) string {
	if sel == nil {
		return ""
	}
	return metav1.FormatLabelSelector(sel)
}

func parseHPA(doc []byte, reg *metrics.Registry) (*Policy, error) {
	var obj autoscalingv2.HorizontalPodAutoscaler
	if err := strictDecode(doc, &obj); err != nil {
		return nil, fmt.Errorf("strict decoding of autoscaling/v2 HorizontalPodAutoscaler failed: %w", err)
	}
	p := &Policy{Kind: KindHPA, Name: obj.Name}
	spec := hpa.Spec{Name: obj.Name, MinReplicas: 1, MaxReplicas: obj.Spec.MaxReplicas}
	if obj.Spec.MinReplicas != nil {
		spec.MinReplicas = *obj.Spec.MinReplicas
	}
	if len(obj.Spec.Metrics) == 0 {
		return nil, errors.New("spec.metrics is empty: the API server would default it to 80% CPU utilisation, which is not simulated (CPU use of a GPU inference pod says little about its load); add a Pods or External metric")
	}
	for i, m := range obj.Spec.Metrics {
		where := fmt.Sprintf("spec.metrics[%d]", i)
		switch m.Type {
		case autoscalingv2.PodsMetricSourceType:
			if m.Pods == nil {
				return nil, fmt.Errorf("%s: type Pods without a pods block", where)
			}
			if m.Pods.Target.Type != autoscalingv2.AverageValueMetricType || m.Pods.Target.AverageValue == nil {
				return nil, fmt.Errorf("%s: a Pods metric needs target.type AverageValue with averageValue", where)
			}
			ref, ok := reg.Resolve(m.Pods.Metric.Name)
			if !ok {
				return nil, fmt.Errorf("%s: %w", where, metrics.UnknownError(m.Pods.Metric.Name))
			}
			if d := ref.Series.Describe(); d.Gateway {
				return nil, fmt.Errorf("%s: %s is a gateway metric, not a per-pod metric; use type External", where, m.Pods.Metric.Name)
			} else if d.Counter && !ref.Rate {
				return nil, fmt.Errorf("%s: %s is a raw counter; scale on its rate (for example %s) instead", where, m.Pods.Metric.Name, "tokens_per_second")
			}
			if m.Pods.Metric.Selector != nil {
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s: metric selector %q is ignored (one model, one metric series per replica)", where, selectorText(m.Pods.Metric.Selector)))
			}
			spec.Metrics = append(spec.Metrics, hpa.MetricSpec{Kind: hpa.PodsAverage, Name: m.Pods.Metric.Name, Target: quantity(m.Pods.Target.AverageValue)})
			p.Sources = append(p.Sources, MetricSource{Ref: ref})
		case autoscalingv2.ExternalMetricSourceType:
			if m.External == nil {
				return nil, fmt.Errorf("%s: type External without an external block", where)
			}
			ref, ok := reg.Resolve(m.External.Metric.Name)
			if !ok {
				return nil, fmt.Errorf("%s: %w", where, metrics.UnknownError(m.External.Metric.Name))
			}
			if d := ref.Series.Describe(); d.Counter && !ref.Rate {
				return nil, fmt.Errorf("%s: %s is a raw counter; scale on its rate instead", where, m.External.Metric.Name)
			}
			ms := hpa.MetricSpec{Name: m.External.Metric.Name, Selector: selectorText(m.External.Metric.Selector)}
			switch {
			case m.External.Target.Type == autoscalingv2.AverageValueMetricType && m.External.Target.AverageValue != nil:
				ms.Kind, ms.Target = hpa.ExternalAverage, quantity(m.External.Target.AverageValue)
			case m.External.Target.Type == autoscalingv2.ValueMetricType && m.External.Target.Value != nil:
				ms.Kind, ms.Target = hpa.ExternalValue, quantity(m.External.Target.Value)
			default:
				return nil, fmt.Errorf("%s: an External metric needs target.type Value with value, or AverageValue with averageValue", where)
			}
			if m.External.Metric.Selector != nil {
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s: metric selector %q is ignored; the value is the sum over all replicas", where, ms.Selector))
			}
			spec.Metrics = append(spec.Metrics, ms)
			p.Sources = append(p.Sources, MetricSource{Ref: ref})
		case autoscalingv2.ResourceMetricSourceType, autoscalingv2.ContainerResourceMetricSourceType:
			return nil, fmt.Errorf("%s: %s metrics (CPU/memory) are not simulated: the replica model has no CPU or memory usage. Scale on an engine metric such as vllm:num_requests_waiting instead, or remove this metric to backtest the others", where, m.Type)
		case autoscalingv2.ObjectMetricSourceType:
			return nil, fmt.Errorf("%s: Object metrics are not supported; express the metric as type External (for example gateway_requests_pending)", where)
		default:
			return nil, fmt.Errorf("%s: unknown metric type %q", where, m.Type)
		}
	}
	b, err := convertBehavior(obj.Spec.Behavior)
	if err != nil {
		return nil, err
	}
	spec.Behavior = b
	if _, err := hpa.NewController(spec, hpa.DefaultOptions()); err != nil {
		return nil, err
	}
	if spec.MinReplicas == 0 {
		gateway := false
		for i, src := range p.Sources {
			gateway = gateway || (spec.Metrics[i].Kind != hpa.PodsAverage && src.Ref.Series.Describe().Gateway)
		}
		if !gateway {
			p.Warnings = append(p.Warnings, "minReplicas is 0 but no External metric reads a gateway metric: metrics exported by the replicas have no series at zero replicas, so nothing can scale the workload back up")
		}
	}
	p.HPA = spec
	return p, nil
}

func convertBehavior(b *autoscalingv2.HorizontalPodAutoscalerBehavior) (*hpa.Behavior, error) {
	if b == nil {
		return nil, nil
	}
	conv := func(r *autoscalingv2.HPAScalingRules) *hpa.ScalingRules {
		if r == nil {
			return nil
		}
		out := &hpa.ScalingRules{}
		if r.StabilizationWindowSeconds != nil {
			v := *r.StabilizationWindowSeconds
			out.StabilizationWindowSeconds = &v
		}
		if r.SelectPolicy != nil {
			out.SelectPolicy = string(*r.SelectPolicy)
		}
		if r.Policies != nil {
			out.Policies = []hpa.ScalingPolicy{}
			for _, pol := range r.Policies {
				out.Policies = append(out.Policies, hpa.ScalingPolicy{Type: string(pol.Type), Value: pol.Value, PeriodSeconds: pol.PeriodSeconds})
			}
		}
		if r.Tolerance != nil {
			v := r.Tolerance.AsApproximateFloat64()
			out.Tolerance = &v
		}
		return out
	}
	out := &hpa.Behavior{ScaleUp: conv(b.ScaleUp), ScaleDown: conv(b.ScaleDown)}
	for _, r := range []*hpa.ScalingRules{out.ScaleUp, out.ScaleDown} {
		if r == nil {
			continue
		}
		if r.StabilizationWindowSeconds != nil && (*r.StabilizationWindowSeconds < 0 || *r.StabilizationWindowSeconds > 3600) {
			return nil, fmt.Errorf("behavior: stabilizationWindowSeconds must be in 0..3600, got %d", *r.StabilizationWindowSeconds)
		}
		if r.Tolerance != nil && *r.Tolerance < 0 {
			return nil, errors.New("behavior: tolerance must not be negative")
		}
	}
	return out, nil
}

// scaledObject mirrors the ScaledObject fields that are understood. Decoding
// is strict, so any other field is an error rather than a silent omission.
type scaledObject struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   map[string]any `json:"metadata"`
	Spec       struct {
		ScaleTargetRef        map[string]any   `json:"scaleTargetRef"`
		PollingInterval       *int32           `json:"pollingInterval"`
		CooldownPeriod        *int32           `json:"cooldownPeriod"`
		InitialCooldownPeriod *int32           `json:"initialCooldownPeriod"`
		IdleReplicaCount      *int32           `json:"idleReplicaCount"`
		MinReplicaCount       *int32           `json:"minReplicaCount"`
		MaxReplicaCount       *int32           `json:"maxReplicaCount"`
		Fallback              *json.RawMessage `json:"fallback"`
		Advanced              *struct {
			RestoreToOriginalReplicaCount *bool `json:"restoreToOriginalReplicaCount"`
			HorizontalPodAutoscalerConfig *struct {
				Name     string                                         `json:"name"`
				Behavior *autoscalingv2.HorizontalPodAutoscalerBehavior `json:"behavior"`
			} `json:"horizontalPodAutoscalerConfig"`
			ScalingModifiers *json.RawMessage `json:"scalingModifiers"`
		} `json:"advanced"`
		Triggers []struct {
			Type              string            `json:"type"`
			Name              string            `json:"name"`
			Metadata          map[string]string `json:"metadata"`
			UseCachedMetrics  bool              `json:"useCachedMetrics"`
			AuthenticationRef map[string]any    `json:"authenticationRef"`
			MetricType        string            `json:"metricType"`
		} `json:"triggers"`
	} `json:"spec"`
	Status map[string]any `json:"status"`
}

// Annotations that pause a ScaledObject.
var pauseAnnotations = []string{
	"autoscaling.keda.sh/paused",
	"autoscaling.keda.sh/paused-replicas",
	"autoscaling.keda.sh/paused-scale-in",
	"autoscaling.keda.sh/paused-scale-out",
}

// Prometheus trigger metadata that has no effect on a simulation.
var ignoredPromMetadata = map[string]bool{
	"serverAddress": true, "namespace": true, "customHeaders": true, "queryParameters": true,
	"unsafeSsl": true, "timeout": true, "authModes": true, "metricName": true, "cortexOrgID": true,
}

func parseScaledObject(doc []byte, reg *metrics.Registry) (*Policy, error) {
	var obj scaledObject
	if err := strictDecode(doc, &obj); err != nil {
		return nil, fmt.Errorf("strict decoding of ScaledObject failed (a field this tool does not implement is refused rather than ignored): %w", err)
	}
	name, _ := obj.Metadata["name"].(string)
	p := &Policy{Kind: KindKEDA, Name: name}
	if ann, ok := obj.Metadata["annotations"].(map[string]any); ok {
		for _, a := range pauseAnnotations {
			if _, set := ann[a]; set {
				return nil, fmt.Errorf("annotation %s pauses autoscaling; remove it to backtest the policy", a)
			}
		}
	}
	s := keda.Spec{
		Name:            name,
		PollingInterval: keda.DefaultPollingInterval,
		CooldownPeriod:  keda.DefaultCooldownPeriod,
		MaxReplicaCount: keda.DefaultMaxReplicaCount,
	}
	sp := obj.Spec
	if sp.PollingInterval != nil {
		s.PollingInterval = float64(*sp.PollingInterval)
	}
	if sp.CooldownPeriod != nil {
		s.CooldownPeriod = float64(*sp.CooldownPeriod)
	}
	if sp.InitialCooldownPeriod != nil {
		s.InitialCooldownPeriod = float64(*sp.InitialCooldownPeriod)
	}
	if sp.MinReplicaCount != nil {
		s.MinReplicaCount = *sp.MinReplicaCount
	}
	if sp.MaxReplicaCount != nil {
		s.MaxReplicaCount = *sp.MaxReplicaCount
	}
	s.IdleReplicaCount = sp.IdleReplicaCount
	if sp.Fallback != nil {
		p.Warnings = append(p.Warnings, "spec.fallback is not simulated: scalers never fail in a backtest, so the fallback replica count is never used")
	}
	if sp.Advanced != nil {
		if sp.Advanced.ScalingModifiers != nil {
			return nil, errors.New("spec.advanced.scalingModifiers is not supported: formulas over several triggers are not evaluated")
		}
		if c := sp.Advanced.HorizontalPodAutoscalerConfig; c != nil {
			b, err := convertBehavior(c.Behavior)
			if err != nil {
				return nil, err
			}
			s.Behavior = b
		}
	}
	if len(sp.Triggers) == 0 {
		return nil, errors.New("spec.triggers is empty")
	}
	for i, tr := range sp.Triggers {
		where := fmt.Sprintf("spec.triggers[%d]", i)
		switch tr.Type {
		case "prometheus":
		case "cpu", "memory":
			return nil, fmt.Errorf("%s: the %s trigger is not simulated: the replica model has no CPU or memory usage", where, tr.Type)
		default:
			return nil, fmt.Errorf("%s: trigger type %q is not supported; only the prometheus trigger is", where, tr.Type)
		}
		t := keda.Trigger{Name: tr.Name, Type: tr.Type, MetricType: keda.MetricTypeAverageValue,
			UseCachedMetrics: tr.UseCachedMetrics, IgnoreNullValues: true}
		if t.Name == "" {
			t.Name = fmt.Sprintf("s%d-%s", i, tr.Type)
		}
		switch tr.MetricType {
		case "", keda.MetricTypeAverageValue:
		case keda.MetricTypeValue:
			t.MetricType = keda.MetricTypeValue
		default:
			return nil, fmt.Errorf("%s: metricType %q is not supported for the prometheus trigger (AverageValue or Value)", where, tr.MetricType)
		}
		query := ""
		haveThreshold := false
		for k, v := range tr.Metadata {
			switch k {
			case "query":
				query = v
			case "threshold":
				f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
				if err != nil {
					return nil, fmt.Errorf("%s: threshold %q is not a number", where, v)
				}
				t.Threshold, haveThreshold = f, true
			case "activationThreshold":
				f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
				if err != nil {
					return nil, fmt.Errorf("%s: activationThreshold %q is not a number", where, v)
				}
				t.ActivationThreshold = f
			case "ignoreNullValues":
				b, err := strconv.ParseBool(strings.TrimSpace(v))
				if err != nil {
					return nil, fmt.Errorf("%s: ignoreNullValues %q is not a boolean", where, v)
				}
				t.IgnoreNullValues = b
			default:
				if !ignoredPromMetadata[k] {
					return nil, fmt.Errorf("%s: metadata field %q is not known to this tool", where, k)
				}
			}
		}
		if query == "" {
			return nil, fmt.Errorf("%s: metadata.query is required", where)
		}
		if !haveThreshold {
			return nil, fmt.Errorf("%s: metadata.threshold is required", where)
		}
		q, err := promql.Parse(query)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		ref, err := reg.CheckQuery(q)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		if q.Matchers != "" {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s: label matchers {%s} are ignored (one model, one metric series per replica)", where, q.Matchers))
		}
		if tr.AuthenticationRef != nil {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s: authenticationRef is irrelevant to a simulation and ignored", where))
		}
		s.Triggers = append(s.Triggers, t)
		p.Sources = append(p.Sources, MetricSource{Ref: ref, Query: q})
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if s.ScalesToZero() {
		for i, src := range p.Sources {
			if !src.Ref.Series.Describe().Gateway {
				p.Warnings = append(p.Warnings, fmt.Sprintf("spec.triggers[%d]: the query reads a metric exported by the replicas themselves; at zero replicas it has no series, so nothing can reactivate the workload unless another trigger reads a gateway metric (for example gateway_requests_pending)", i))
			}
		}
	}
	p.KEDA = &s
	p.HPA = s.HPASpec()
	if _, err := hpa.NewController(p.HPA, hpa.DefaultOptions()); err != nil {
		return nil, err
	}
	return p, nil
}
