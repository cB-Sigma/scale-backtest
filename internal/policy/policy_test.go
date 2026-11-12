package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cb-Sigma/scale-backtest/internal/hpa"
	"github.com/cb-Sigma/scale-backtest/internal/metrics"
)

const examples = "../../examples/policies"

func TestExamplesLoad(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(examples, "*.yaml"))
	if err != nil || len(files) < 6 {
		t.Fatalf("expected the example policies, got %v, %v", files, err)
	}
	for _, f := range files {
		p, err := Load(f, nil)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if p.Name == "" || len(p.HPA.Metrics) == 0 || len(p.Sources) != len(p.HPA.Metrics) {
			t.Errorf("%s: incomplete policy %+v", f, p)
		}
		if strings.HasPrefix(filepath.Base(f), "keda-") != (p.KEDA != nil) {
			t.Errorf("%s: kind %s", f, p.Kind)
		}
	}
}

func TestHPABehaviorConversion(t *testing.T) {
	p, err := Load(filepath.Join(examples, "hpa-running-requests.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindHPA || p.HPA.MinReplicas != 1 || p.HPA.MaxReplicas != 8 {
		t.Fatalf("%+v", p.HPA)
	}
	m := p.HPA.Metrics[0]
	if m.Kind != hpa.PodsAverage || m.Name != "vllm:num_requests_running" || m.Target != 12 {
		t.Fatalf("metric %+v", m)
	}
	if p.Sources[0].Ref.Series != metrics.RequestsRunning {
		t.Fatalf("source %+v", p.Sources[0])
	}
	b := p.HPA.Behavior
	if b == nil || *b.ScaleDown.StabilizationWindowSeconds != 600 || len(b.ScaleDown.Policies) != 1 ||
		b.ScaleDown.Policies[0] != (hpa.ScalingPolicy{Type: "Pods", Value: 1, PeriodSeconds: 120}) ||
		b.ScaleUp.Policies[0] != (hpa.ScalingPolicy{Type: "Pods", Value: 4, PeriodSeconds: 30}) {
		t.Fatalf("behavior %+v %+v", b.ScaleUp, b.ScaleDown)
	}
}

const hpaHead = `apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: x}
spec:
  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: x}
  maxReplicas: 5
`

func TestHPADefaultsAndQuantities(t *testing.T) {
	p, err := Parse([]byte(hpaHead+`  metrics:
  - type: Pods
    pods:
      metric: {name: vllm:num_requests_waiting}
      target: {type: AverageValue, averageValue: 1500m}
  - type: External
    external:
      metric: {name: gateway_requests_pending, selector: {matchLabels: {route: chat}}}
      target: {type: Value, value: "30"}
  behavior:
    scaleUp:
      tolerance: "0.05"
      selectPolicy: Min
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.HPA.MinReplicas != 1 {
		t.Errorf("minReplicas defaults to 1, got %d", p.HPA.MinReplicas)
	}
	if p.HPA.Metrics[0].Target != 1.5 || p.HPA.Metrics[1].Target != 30 || p.HPA.Metrics[1].Kind != hpa.ExternalValue {
		t.Errorf("targets: %+v", p.HPA.Metrics)
	}
	if got := p.HPA.Metrics[1].Describe(); got != "external metric gateway_requests_pending(route=chat)" {
		t.Errorf("describe: %q", got)
	}
	if b := p.HPA.Behavior; b == nil || *b.ScaleUp.Tolerance != 0.05 || b.ScaleUp.SelectPolicy != "Min" || b.ScaleDown != nil {
		t.Errorf("behavior: %+v", b)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "selector") {
		t.Errorf("warnings: %v", p.Warnings)
	}
}

func TestHPARefusals(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"cpu utilisation": {hpaHead + `  metrics:
  - type: Resource
    resource: {name: cpu, target: {type: Utilization, averageUtilization: 60}}
`, "not simulated"},
		"object metric": {hpaHead + `  metrics:
  - type: Object
    object:
      metric: {name: requests-per-second}
      describedObject: {apiVersion: networking.k8s.io/v1, kind: Ingress, name: main-route}
      target: {type: Value, value: 2k}
`, "Object metrics are not supported"},
		"no metrics":                      {hpaHead, "spec.metrics is empty"},
		"unknown metric":                  {hpaHead + "  metrics:\n  - type: Pods\n    pods:\n      metric: {name: http_requests}\n      target: {type: AverageValue, averageValue: 5}\n", "not mapped"},
		"unknown field (strict decoding)": {hpaHead + "  minReplica: 2\n  metrics:\n  - type: Pods\n    pods:\n      metric: {name: requests_running}\n      target: {type: AverageValue, averageValue: 5}\n", "minReplica"},
		"pods metric with value target":   {hpaHead + "  metrics:\n  - type: Pods\n    pods:\n      metric: {name: requests_running}\n      target: {type: Value, value: 5}\n", "AverageValue"},
		"raw counter":                     {hpaHead + "  metrics:\n  - type: Pods\n    pods:\n      metric: {name: vllm:generation_tokens_total}\n      target: {type: AverageValue, averageValue: 5}\n", "raw counter"},
		"gateway metric as pods metric":   {hpaHead + "  metrics:\n  - type: Pods\n    pods:\n      metric: {name: gateway_requests_pending}\n      target: {type: AverageValue, averageValue: 5}\n", "gateway metric"},
		"min 0 without external metric":   {hpaHead + "  minReplicas: 0\n  metrics:\n  - type: Pods\n    pods:\n      metric: {name: requests_running}\n      target: {type: AverageValue, averageValue: 5}\n", "minReplicas 0 requires"},
		"old api version":                 {strings.Replace(hpaHead, "autoscaling/v2", "autoscaling/v1", 1) + "  targetCPUUtilizationPercentage: 50\n", "not supported"},
		"not a policy":                    {"apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: x}\n", "no HorizontalPodAutoscaler or ScaledObject"},
	}
	for name, tc := range cases {
		_, err := Parse([]byte(tc.yaml), nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error mentioning %q", name, err, tc.want)
		}
	}
}

func TestHPAScaleToZeroWarning(t *testing.T) {
	p, err := Parse([]byte(hpaHead+"  minReplicas: 0\n  metrics:\n  - type: External\n    external:\n      metric: {name: vllm:num_requests_waiting}\n      target: {type: AverageValue, averageValue: 5}\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "zero replicas") {
		t.Fatalf("warnings: %v", p.Warnings)
	}
	p, err = Load(filepath.Join(examples, "hpa-scale-to-zero.yaml"), nil)
	if err != nil || len(p.Warnings) != 0 {
		t.Fatalf("the example reads a gateway metric and must load without warnings: %v %v", err, p.Warnings)
	}
}

const kedaHead = `apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata: {name: so}
spec:
  scaleTargetRef: {name: vllm}
`

const promTrigger = `  triggers:
  - type: prometheus
    metadata:
      serverAddress: http://prom:9090
      query: sum(vllm:num_requests_waiting)
      threshold: "5"
`

func TestKEDADefaults(t *testing.T) {
	p, err := Parse([]byte(kedaHead+promTrigger), nil)
	if err != nil {
		t.Fatal(err)
	}
	k := p.KEDA
	// Defaults from the ScaledObject specification.
	if k.PollingInterval != 30 || k.CooldownPeriod != 300 || k.MinReplicaCount != 0 || k.MaxReplicaCount != 100 {
		t.Fatalf("defaults: %+v", k)
	}
	tr := k.Triggers[0]
	if tr.Threshold != 5 || tr.ActivationThreshold != 0 || tr.MetricType != "AverageValue" || !tr.IgnoreNullValues || tr.Name != "s0-prometheus" {
		t.Fatalf("trigger: %+v", tr)
	}
	if p.HPA.MinReplicas != 1 || p.HPA.MaxReplicas != 100 || p.HPA.Behavior != nil || p.HPA.Metrics[0].Kind != hpa.ExternalAverage {
		t.Fatalf("generated HPA: %+v", p.HPA)
	}
	// Scaling to zero on a metric the replicas export is flagged.
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "zero replicas") {
		t.Fatalf("warnings: %v", p.Warnings)
	}
}

func TestKEDARefusals(t *testing.T) {
	trig := func(meta string) string {
		return kedaHead + "  triggers:\n  - type: prometheus\n    metadata:\n" + meta
	}
	cases := map[string]struct{ yaml, want string }{
		"cpu trigger":          {kedaHead + "  triggers:\n  - type: cpu\n    metricType: Utilization\n    metadata: {value: \"60\"}\n", "cpu trigger is not simulated"},
		"kafka trigger":        {kedaHead + "  triggers:\n  - type: kafka\n    metadata: {topic: t}\n", "only the prometheus trigger"},
		"scaling modifiers":    {kedaHead + "  advanced:\n    scalingModifiers: {formula: a+b, target: \"2\"}\n" + promTrigger, "scalingModifiers is not supported"},
		"unknown spec field":   {kedaHead + "  coolDownPeriod: 30\n" + promTrigger, "coolDownPeriod"},
		"unknown metadata key": {trig("      query: sum(requests_waiting)\n      threshold: \"5\"\n      thresold: \"7\"\n"), "thresold"},
		"unsupported query":    {trig("      query: histogram_quantile(0.9, sum(rate(x_bucket[5m])) by (le))\n      threshold: \"5\"\n"), "unsupported PromQL"},
		"missing threshold":    {trig("      query: sum(requests_waiting)\n"), "threshold is required"},
		"missing query":        {trig("      threshold: \"5\"\n"), "query is required"},
		"utilization type":     {kedaHead + "  triggers:\n  - type: prometheus\n    metricType: Utilization\n    metadata: {query: sum(requests_waiting), threshold: \"5\"}\n", "metricType"},
		"no triggers":          {kedaHead + "  triggers: []\n", "triggers is empty"},
		"idle count 1":         {kedaHead + "  idleReplicaCount: 1\n  minReplicaCount: 3\n" + promTrigger, "idleReplicaCount"},
		"paused":               {strings.Replace(kedaHead, "metadata: {name: so}", "metadata:\n  name: so\n  annotations: {autoscaling.keda.sh/paused: \"true\"}", 1) + promTrigger, "pauses autoscaling"},
		"wrong api version":    {strings.Replace(kedaHead, "v1alpha1", "v2", 1) + promTrigger, "not supported"},
	}
	for name, tc := range cases {
		_, err := Parse([]byte(tc.yaml), nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error mentioning %q", name, err, tc.want)
		}
	}
}

func TestKEDAFlaggedButAccepted(t *testing.T) {
	p, err := Parse([]byte(kedaHead+"  minReplicaCount: 1\n  fallback: {failureThreshold: 3, replicas: 6}\n"+
		"  triggers:\n  - type: prometheus\n    useCachedMetrics: true\n    metricType: Value\n    authenticationRef: {name: prom-auth}\n"+
		"    metadata:\n      query: sum(vllm:num_requests_waiting{model=\"m\"})\n      threshold: \"5\"\n      activationThreshold: \"2.5\"\n      ignoreNullValues: \"false\"\n      unsafeSsl: \"true\"\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := p.KEDA.Triggers[0]
	if !tr.UseCachedMetrics || tr.MetricType != "Value" || tr.ActivationThreshold != 2.5 || tr.IgnoreNullValues {
		t.Fatalf("trigger: %+v", tr)
	}
	joined := strings.Join(p.Warnings, "\n")
	for _, want := range []string{"fallback is not simulated", "label matchers", "authenticationRef"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing warning about %q in:\n%s", want, joined)
		}
	}
}

func TestMultiDocumentManifest(t *testing.T) {
	doc := "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: vllm}\n---\n" + kedaHead + "  minReplicaCount: 1\n" + promTrigger
	p, err := Parse([]byte(doc), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindKEDA || len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "Deployment") {
		t.Fatalf("%s %v", p.Kind, p.Warnings)
	}
}

func TestStaticAndLoadErrors(t *testing.T) {
	p, err := Load("static:4", nil)
	if err != nil || p.Kind != KindStatic || p.StaticReplicas != 4 || p.Label() != "static 4 replicas" {
		t.Fatalf("%+v %v", p, err)
	}
	for _, bad := range []string{"static:0", "static:x", filepath.Join(t.TempDir(), "missing.yaml")} {
		if _, err := Load(bad, nil); err == nil {
			t.Errorf("%s: expected an error", bad)
		}
	}
	garbage := filepath.Join(t.TempDir(), "x.yaml")
	if err := os.WriteFile(garbage, []byte("kind: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(garbage, nil); err == nil {
		t.Error("invalid YAML must fail")
	}
}

func TestSetParameters(t *testing.T) {
	base, err := Load(filepath.Join(examples, "hpa-queue-depth.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := base.Clone()
	for name, v := range map[string]float64{"target": 9, "minReplicas": 2, "maxReplicas": 6, "scaleDown.stabilizationWindowSeconds": 120, "tolerance": 0.2} {
		if err := p.Set(name, v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if p.HPA.Metrics[0].Target != 9 || p.HPA.MinReplicas != 2 || p.HPA.MaxReplicas != 6 ||
		*p.HPA.Behavior.ScaleDown.StabilizationWindowSeconds != 120 || *p.HPA.Behavior.ScaleUp.Tolerance != 0.2 || *p.HPA.Behavior.ScaleDown.Tolerance != 0.2 {
		t.Fatalf("%+v %+v", p.HPA, p.HPA.Behavior)
	}
	// The original is untouched.
	if base.HPA.Metrics[0].Target != 5 || base.HPA.Behavior != nil {
		t.Fatalf("Clone shares state: %+v", base.HPA)
	}
	for name, v := range map[string]float64{"target": 0, "minReplicas": 99, "maxReplicas": 1.5, "nope": 1, "cooldownPeriod": 60, "target[3]": 1} {
		if err := base.Clone().Set(name, v); err == nil {
			t.Errorf("%s=%g: expected an error", name, v)
		}
	}

	k, err := Load(filepath.Join(examples, "keda-scale-to-zero.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	kc := k.Clone()
	for name, v := range map[string]float64{"target": 20, "cooldownPeriod": 60, "pollingInterval": 5, "minReplicas": 2, "activationThreshold": 3, "scaleUp.stabilizationWindowSeconds": 30} {
		if err := kc.Set(name, v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if kc.KEDA.Triggers[0].Threshold != 20 || kc.HPA.Metrics[0].Target != 20 || kc.KEDA.CooldownPeriod != 60 || kc.HPA.MinReplicas != 2 ||
		kc.KEDA.Triggers[0].ActivationThreshold != 3 || kc.HPA.Behavior == nil || *kc.HPA.Behavior.ScaleUp.StabilizationWindowSeconds != 30 {
		t.Fatalf("%+v / %+v", kc.KEDA, kc.HPA)
	}
	if k.KEDA.CooldownPeriod != 300 || k.HPA.Behavior != nil {
		t.Fatal("Clone shares KEDA state")
	}

	s := Static(3)
	if err := s.Set("replicas", 5); err != nil || s.StaticReplicas != 5 {
		t.Fatalf("%v %+v", err, s)
	}
	if err := s.Set("target", 5); err == nil {
		t.Fatal("a static policy has no target")
	}
}
