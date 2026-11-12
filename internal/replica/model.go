// Package replica holds the performance model of a single serving replica:
// how long prefill and decode take as a function of how many requests share
// the batch, how many requests fit, how long a cold start takes and what the
// replica costs. The model is deliberately small so that it can be calibrated
// from an ordinary load-test result; docs/model.md describes it in full.
package replica

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"sigs.k8s.io/yaml"
)

// Kind is the value of the kind field of a replica model file.
const Kind = "ReplicaModel"

// Prefill is the cost of processing a prompt.
type Prefill struct {
	// BaseSeconds is the fixed part of time to first token for a request
	// that runs alone (tokenisation, scheduling, the first decode step).
	BaseSeconds float64 `json:"baseSeconds"`
	// SecondsPerToken is the prefill time per prompt token for a request
	// that runs alone.
	SecondsPerToken float64 `json:"secondsPerToken"`
	// Interference is the relative slowdown of prefill per additional
	// request in the batch: prefill takes (1 + interference x (n-1)) times
	// longer when n requests share the replica.
	Interference float64 `json:"interference"`
}

// CurvePoint is one measured point of the decode curve.
type CurvePoint struct {
	Concurrency     float64 `json:"concurrency"`
	SecondsPerToken float64 `json:"secondsPerToken"`
}

// Decode is the throughput-versus-concurrency curve of the decode phase,
// expressed as the time per output token of one request when n requests
// share the batch.
type Decode struct {
	// BaseSecondsPerToken (a) and SecondsPerTokenPerRequest (b) define the
	// parametric curve tpot(n) = a + b x n. Per-request speed is 1/tpot(n)
	// tokens per second and replica throughput is n/tpot(n), which
	// saturates at 1/b.
	BaseSecondsPerToken       float64 `json:"baseSecondsPerToken"`
	SecondsPerTokenPerRequest float64 `json:"secondsPerTokenPerRequest"`
	// Curve, when set, replaces the parametric curve with measured points,
	// linearly interpolated.
	Curve []CurvePoint `json:"curve,omitempty"`
}

// Node describes how replicas get a GPU node.
type Node struct {
	// ProvisionDelay is how long a new replica stays Pending while a node
	// is provisioned for it. Leave unset when nodes are already there.
	ProvisionDelay Dist `json:"provisionDelay,omitzero"`
	// SpareNodes is the number of idle nodes available at the start, beyond
	// those of the initial replicas.
	SpareNodes int `json:"spareNodes,omitempty"`
	// KeepWarm is how long a node stays (and is billed) after its replica
	// is gone, during which a new replica can reuse it without the
	// provisioning delay. This is the cluster autoscaler's scale-down delay.
	KeepWarm Duration `json:"keepWarm,omitempty"`
}

// Cost is the price of a replica.
type Cost struct {
	GPUPricePerHour float64 `json:"gpuPricePerHour"`
	// GPUsPerReplica defaults to 1.
	GPUsPerReplica float64 `json:"gpusPerReplica,omitempty"`
}

// Model is the model of one replica.
type Model struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Name       string `json:"name"`
	// Description is free text, for example where the numbers came from.
	Description string `json:"description,omitempty"`

	Prefill Prefill `json:"prefill"`
	Decode  Decode  `json:"decode"`

	// MaxConcurrency is the largest number of requests in the running batch
	// (for vLLM: --max-num-seqs).
	MaxConcurrency int `json:"maxConcurrency"`
	// MaxQueue is how many requests may wait inside the replica. Unset
	// means unbounded, which is what vLLM does; 0 means no queue.
	MaxQueue *int `json:"maxQueue,omitempty"`

	// ColdStart is the time from a container being scheduled on a node to
	// the replica being Ready (image pull, model load, warm-up).
	ColdStart Dist `json:"coldStart"`
	Node      Node `json:"node,omitzero"`

	// TerminationGracePeriod bounds how long a replica removed by a
	// scale-down may keep serving the requests it already accepted.
	// Requests still running after it are dropped. Default 600s.
	TerminationGracePeriod *Duration `json:"terminationGracePeriod,omitempty"`

	Cost Cost `json:"cost"`

	curve []CurvePoint // sorted, validated
}

// DefaultTerminationGrace is used when the model does not set one.
const DefaultTerminationGrace = 600

// Load reads and validates a replica model file.
func Load(path string) (*Model, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("replica model: %w", err)
	}
	m, err := Parse(data, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("replica model %s: %w", path, err)
	}
	return m, nil
}

// Parse parses and validates a replica model. baseDir locates files the
// model refers to.
func Parse(data []byte, baseDir string) (*Model, error) {
	var m Model
	if err := yaml.UnmarshalStrict(data, &m); err != nil {
		return nil, err
	}
	if m.Kind != "" && m.Kind != Kind {
		return nil, fmt.Errorf("kind is %q, expected %s", m.Kind, Kind)
	}
	if err := m.Resolve(baseDir); err != nil {
		return nil, err
	}
	return &m, nil
}

func resolvePath(baseDir, p string) string {
	if filepath.IsAbs(p) || baseDir == "" {
		return p
	}
	return filepath.Join(baseDir, p)
}

// Resolve validates the model and prepares it for use. It must be called on
// models built in code.
func (m *Model) Resolve(baseDir string) error {
	if m.MaxConcurrency < 1 {
		return errors.New("maxConcurrency must be at least 1")
	}
	if m.MaxQueue != nil && *m.MaxQueue < 0 {
		return errors.New("maxQueue must not be negative (leave it out for an unbounded queue)")
	}
	p := m.Prefill
	if p.BaseSeconds < 0 || p.SecondsPerToken < 0 || p.Interference < 0 {
		return errors.New("prefill values must not be negative")
	}
	d := m.Decode
	if len(d.Curve) > 0 {
		if d.BaseSecondsPerToken != 0 || d.SecondsPerTokenPerRequest != 0 {
			return errors.New("decode: set either the parametric curve (baseSecondsPerToken, secondsPerTokenPerRequest) or curve points, not both")
		}
		c := append([]CurvePoint(nil), d.Curve...)
		sort.Slice(c, func(i, j int) bool { return c[i].Concurrency < c[j].Concurrency })
		for i, pt := range c {
			if pt.Concurrency < 1 || pt.SecondsPerToken <= 0 {
				return errors.New("decode.curve: concurrency must be at least 1 and secondsPerToken positive")
			}
			if i > 0 && pt.Concurrency == c[i-1].Concurrency {
				return errors.New("decode.curve: duplicate concurrency")
			}
		}
		m.curve = c
	} else {
		if d.BaseSecondsPerToken < 0 || d.SecondsPerTokenPerRequest < 0 {
			return errors.New("decode values must not be negative")
		}
		if d.BaseSecondsPerToken+d.SecondsPerTokenPerRequest <= 0 {
			return errors.New("decode: the time per output token must be positive (set baseSecondsPerToken and/or secondsPerTokenPerRequest, or curve points)")
		}
	}
	if m.ColdStart.IsZero() {
		m.ColdStart = ConstantDist(0)
	}
	if err := m.ColdStart.resolve("coldStart", baseDir); err != nil {
		return err
	}
	if m.Node.ProvisionDelay.IsZero() {
		m.Node.ProvisionDelay = ConstantDist(0)
	}
	if err := m.Node.ProvisionDelay.resolve("node.provisionDelay", baseDir); err != nil {
		return err
	}
	if m.Node.SpareNodes < 0 || m.Node.KeepWarm < 0 {
		return errors.New("node.spareNodes and node.keepWarm must not be negative")
	}
	if m.TerminationGracePeriod != nil && *m.TerminationGracePeriod < 0 {
		return errors.New("terminationGracePeriod must not be negative")
	}
	if m.Cost.GPUPricePerHour < 0 || m.Cost.GPUsPerReplica < 0 {
		return errors.New("cost values must not be negative")
	}
	if m.Cost.GPUsPerReplica == 0 {
		m.Cost.GPUsPerReplica = 1
	}
	return nil
}

// TerminationGrace returns the grace period in seconds.
func (m *Model) TerminationGrace() float64 {
	if m.TerminationGracePeriod == nil {
		return DefaultTerminationGrace
	}
	return float64(*m.TerminationGracePeriod)
}

// SecondsPerToken returns the time per output token of one request when n
// requests share the batch.
func (m *Model) SecondsPerToken(n int) float64 {
	if n < 1 {
		n = 1
	}
	if len(m.curve) == 0 {
		return m.Decode.BaseSecondsPerToken + m.Decode.SecondsPerTokenPerRequest*float64(n)
	}
	c := m.curve
	x := float64(n)
	if x <= c[0].Concurrency || len(c) == 1 {
		return c[0].SecondsPerToken
	}
	for i := 1; i < len(c); i++ {
		if x <= c[i].Concurrency {
			a, b := c[i-1], c[i]
			return a.SecondsPerToken + (b.SecondsPerToken-a.SecondsPerToken)*(x-a.Concurrency)/(b.Concurrency-a.Concurrency)
		}
	}
	// Beyond the last point: continue the last segment, never downwards.
	a, b := c[len(c)-2], c[len(c)-1]
	slope := math.Max(0, (b.SecondsPerToken-a.SecondsPerToken)/(b.Concurrency-a.Concurrency))
	return b.SecondsPerToken + slope*(x-b.Concurrency)
}

// DecodeRate returns the output tokens per second of one request when n
// requests share the batch.
func (m *Model) DecodeRate(n int) float64 { return 1 / m.SecondsPerToken(n) }

// Throughput returns the output tokens per second of the whole replica with
// n requests in the batch.
func (m *Model) Throughput(n int) float64 {
	if n < 1 {
		return 0
	}
	return float64(n) / m.SecondsPerToken(n)
}

// PeakThroughput returns the highest throughput over 1..MaxConcurrency.
func (m *Model) PeakThroughput() float64 {
	peak := 0.0
	for n := 1; n <= m.MaxConcurrency; n++ {
		peak = math.Max(peak, m.Throughput(n))
	}
	return peak
}

// PrefillSlowdown returns the factor by which prefill is slower with n
// requests in the batch than alone.
func (m *Model) PrefillSlowdown(n int) float64 {
	if n < 1 {
		n = 1
	}
	return 1 + m.Prefill.Interference*float64(n-1)
}

// PrefillSeconds returns the prefill time of a prompt for a request that
// runs alone.
func (m *Model) PrefillSeconds(promptTokens int) float64 {
	return m.Prefill.BaseSeconds + m.Prefill.SecondsPerToken*float64(promptTokens)
}

// Marshal renders the model as YAML.
func (m *Model) Marshal() ([]byte, error) {
	c := *m
	if c.Kind == "" {
		c.Kind = Kind
	}
	return yaml.Marshal(&c)
}
