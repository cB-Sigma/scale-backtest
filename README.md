# scale-backtest

**Backtest a Kubernetes HPA or KEDA autoscaling policy for LLM inference against a request trace, before you deploy it: would it have held your latency targets, and what would the GPUs have cost?**

[![CI](https://github.com/cb-Sigma/scale-backtest/actions/workflows/ci.yml/badge.svg)](https://github.com/cb-Sigma/scale-backtest/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/cb-Sigma/scale-backtest)](https://goreportcard.com/report/github.com/cb-Sigma/scale-backtest)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go version](https://img.shields.io/github/go-mod/go-version/cb-Sigma/scale-backtest)](go.mod)

```sh
scale-backtest run --policy hpa.yaml --trace requests.csv --replica replica.yaml
```

- **The policy is your real manifest.** An `autoscaling/v2` HorizontalPodAutoscaler or a KEDA `ScaledObject`, decoded strictly. The control loops are re-implemented from the Kubernetes and KEDA documentation and tested against the worked examples in those docs.
- **The load is your real history.** A per-request log (CSV, JSONL, the Azure LLM inference trace format), or, more coarsely, request-rate series from Prometheus.
- **The replica is a small model you calibrate** from an ordinary load test: a throughput-versus-concurrency curve, a prefill cost, a cold-start distribution and a GPU price.
- **The answer is a verdict:** TTFT and end-to-end percentiles including queueing, the share of requests over target, dropped requests, the replica timeline, every scale event with the reason the autoscaler gave, GPU-hours, cost and idle share. `compare` puts policies side by side, `sweep` finds the cost-versus-attainment frontier, `--slo` turns it into an exit code for CI.

> **Read this first.** The replica model is an approximation. It has **not** been validated against a real GPU inference engine in this repository. The autoscaler logic and the queueing mechanics are tested (see [Validation](#validation)); the numbers you get are only as good as your calibration. Treat results as a way to compare policies and catch bad ones, not as a latency forecast. See [Limitations](#limitations).

## Demo

All output below is real: it is produced by the bundled synthetic trace (45 minutes, two bursts), a made-up replica model and the policies in [`examples/`](examples/), and a test fails if these blocks drift from what the binary prints.

A queue-depth HPA, the policy many teams start with:

<!-- output: scale-backtest run --demo --max-events 7 -->
```text
HPA vllm-queue-depth · trace demo (4,797 requests over 45m00s) · replica model demo-gpu

Latency (completed requests, queueing included)
               mean     p50    p90    p95    p99    max
  TTFT        7.80s  0.144s  28.7s  54.1s  1m50s  2m09s
  End-to-end  16.6s   7.42s  44.0s  69.3s  2m07s  2m47s
  Queue wait  7.63s      0s  28.4s  54.0s  1m50s  2m08s

Targets (all requests; a dropped request misses its targets)
  TTFT <= 2.00s        80.4%  941 missed
  End-to-end <= 60.0s  93.3%  320 missed
  Both                 80.3%  944 missed

Requests  4,797 completed · 0 dropped · 0 arrived with no ready replica
Replicas  start 1 · ready min 1 / mean 3.69 / max 8 · 21 cold starts · 17 scale events
  ready     ▂▂▂▂▂▂▂▂▂▂▂▂▂▂▃▇███████▂▂▅▇███████▂▂▂▂▂▂▂▂▃█████████▅▂▂▂▂▂▂▂  0..8
  arrivals  ▂▂▂▂▂▂▂▂▂▂▂▃▄▅▆▆▆▇▇▅▅▆▆▆▅▅▄▃▃▃▃▃▃▃▃▃▃▃▃▃▄████████▅▃▂▃▂▂▂▂▂▂▂  0..4.7 req/s
  waiting   ▁▁▁▁▁▁▁▁▁▁▁▁▂▃▃▃▁▁▁▁▁▁▁▂▃▅▃▁▁▁▁▁▁▁▁▁▁▁▁▁▁▅██▅▃▁▁▁▁▁▁▁▁▁▁▁▁▁▁  0..214 requests
Cost      3.40 GPU-hours · $8.50 at $2.50 per GPU-hour · $4.47 per 1M output tokens
          busy 74.5% · idle 7.99% · starting 17.6% · mean batch fill 17.7%

Scale events (first 7 of 17; --max-events 0 shows all)
  00:09:15  1 -> 2  HPA  pods metric vllm:num_requests_waiting above target
                           vllm:num_requests_waiting=6/5 -> 2
  00:09:45  2 -> 4  HPA  pods metric vllm:num_requests_waiting above target
                           vllm:num_requests_waiting=24/5 -> 5; limited: ScaleUpLimit
  00:10:00  4 -> 6  HPA  pods metric vllm:num_requests_waiting above target
                           vllm:num_requests_waiting=26/5 -> 6
  00:10:15  6 -> 8  HPA  pods metric vllm:num_requests_waiting above target
                           vllm:num_requests_waiting=37/5 -> 8
  00:16:45  8 -> 4  HPA  All metrics below target
                           vllm:num_requests_waiting=0/5 -> 0; ScaleDownStabilized
  00:17:00  4 -> 2  HPA  All metrics below target
                           vllm:num_requests_waiting=0/5 -> 0; ScaleDownStabilized
  00:17:15  2 -> 1  HPA  All metrics below target
                           vllm:num_requests_waiting=0/5 -> 0; ScaleDownStabilized
```

The same trace against five policies:

<!-- output: scale-backtest compare --demo -->
```text
trace demo (4,797 requests over 45m00s) · replica model demo-gpu

policy                            attained  TTFT p50  TTFT p95  E2E p95  dropped  ready mean/max  GPU-h    cost   idle
static 6 replicas                     100%    0.114s    0.319s    17.6s        0           6.0/6   4.50  $11.25  21.9%
HPA vllm-queue-depth                 80.3%    0.144s     54.1s    69.3s        0           3.7/8   3.40   $8.50  7.99%
HPA vllm-running-requests            87.0%    0.144s     13.3s    31.5s        0           3.9/8   3.19   $7.98  18.4%
KEDA ScaledObject vllm-keda          90.0%    0.133s     11.2s    28.3s        0           3.5/8   3.08   $7.69  5.95%
KEDA ScaledObject vllm-keda-zero     88.7%    0.138s     12.6s    30.4s        0           3.3/8   2.97   $7.44  9.14%

attained = share of all requests completed within TTFT <= 2.00s and end-to-end <= 60.0s

ready replicas over time
  static 6 replicas                 ████████████████████████████████████████████████████████████  0..6
  HPA vllm-queue-depth              ▂▂▂▂▂▂▂▂▂▂▂▂▂▂▃▇███████▂▂▅▇███████▂▂▂▂▂▂▂▂▃█████████▅▂▂▂▂▂▂▂  0..8
  HPA vllm-running-requests         ▂▂▂▂▂▂▂▂▂▂▂▂▂▃▃▃▄▅▅▅▅▅▅▅▅▅▅▅▅▅▅▅▅▅▅▄▄▄▄▃▃▃▃▅▅▆██████████████  0..8
  KEDA ScaledObject vllm-keda       ▂▂▂▂▂▂▂▂▂▂▂▂▂▃▃▇██████▇▆▅▅▄▄▄▄▄▄▃▂▂▂▃▃▃▃▃▃▃▆████████▇▆▆▅▄▃▃▂  0..8
  KEDA ScaledObject vllm-keda-zero  ▁▁▂▃▆▆▆▆▆▆▂▂▂▃▃▅▅▆▆▆▆▅▅▄▄▄▄▄▄▄▄▄▃▂▂▂▃▃▃▃▃▃▃▆███████▅▅▅▅▄▄▂▂▂  0..8
  arrivals                          ▂▂▂▂▂▂▂▂▂▂▂▃▄▅▆▆▆▇▇▅▅▆▆▆▅▅▄▃▃▃▃▃▃▃▃▃▃▃▃▃▄████████▅▃▂▃▂▂▂▂▂▂▂  0..4.7 req/s
```

What the comparison shows, for this trace and this replica model:

- The fixed fleet meets every target and costs the most.
- The queue-depth HPA is both slow and wasteful. `vllm:num_requests_waiting` is zero until a replica's batch is full, so it fires late, overshoots to the maximum while new pods are still starting, and then scales back to one replica in the middle of the burst because the queue it watches has emptied.
- Scaling on running requests reacts earlier, but with a 95-second cold start a two-minute ramp still outruns it.
- Scale to zero is the cheapest and pays for it at the start, where the requests that pile up behind the first cold start make the HPA ask for six replicas.

Then search for a setting that works:

<!-- output: scale-backtest sweep --demo --slo attainment>=99% -->
```text
sweep of HPA vllm-running-requests · trace demo (4,797 requests over 45m00s) · replica model demo-gpu

minReplicas  target  attained  TTFT p50  TTFT p95  E2E p95  dropped  ready mean/max  GPU-h    cost   idle  SLO   frontier
          3      16    99.97%    0.135s    0.388s    22.8s        0           3.2/4   2.45   $6.14  11.3%  PASS  *
          2      16     72.2%    0.180s     37.9s    53.7s        0           3.1/6   2.50   $6.26  15.3%  FAIL
          1      16     68.3%    0.203s     45.9s    63.0s        0           3.2/8   2.73   $6.82  20.8%  FAIL
          3      12      100%    0.129s    0.367s    21.1s        0           3.8/5   2.94   $7.35  13.0%  PASS  *
          1      12     87.0%    0.144s     13.3s    31.5s        0           3.9/8   3.19   $7.98  18.4%  FAIL
          2      12     87.5%    0.139s     15.3s    31.5s        0           4.1/8   3.32   $8.29  19.0%  FAIL
          2       8      100%    0.122s    0.351s    19.9s        0           4.4/7   3.55   $8.87  15.2%  PASS
          3       8      100%    0.121s    0.346s    19.5s        0           4.7/7   3.69   $9.22  16.6%  PASS
          1       8     89.5%    0.131s     8.37s    26.8s        0           4.6/8   3.78   $9.45  16.7%  FAIL
          2       4      100%    0.114s    0.322s    17.8s        0           6.1/8   4.89  $12.22  19.4%  PASS
          1       4      100%    0.114s    0.327s    18.0s        0           6.1/8   4.91  $12.27  19.2%  PASS
          3       4      100%    0.113s    0.320s    17.5s        0           6.4/8   5.02  $12.56  20.2%  PASS

* = on the cost-versus-attainment frontier (no other setting is both cheaper and better)
attained = share of all requests completed within TTFT <= 2.00s and end-to-end <= 60.0s
cheapest setting that meets the SLO: minReplicas=3 target=16 ($6.14, 99.97% attained)
```

Keeping three replicas warm and letting each run fuller is both cheaper and better here than any setting that starts from one. That is a statement about this synthetic trace; the point is that you can ask the question of your own.

## Why

GPU replicas take minutes to start, so an autoscaling policy for LLM inference is a bet placed minutes ahead of the load. A bad one either misses latency targets during bursts or burns idle GPU-hours, and the usual way to find out which is to deploy it and wait. Teams tune `averageValue`, stabilisation windows and `minReplicaCount` by guesswork because nothing answers "would this policy have held last week, and what would it have cost?"

Existing LLM serving simulators answer neighbouring questions (which engine configuration, how many GPUs, how a specific vendor autoscaler behaves) and model their own autoscalers. None of the ones we found takes the HPA or KEDA manifest you already have and replays your production history through it. See [Related work](#related-work).

## Install

```sh
go install github.com/cb-Sigma/scale-backtest/cmd/scale-backtest@latest
```

From source (Go 1.26 or newer):

```sh
git clone https://github.com/cb-Sigma/scale-backtest
cd scale-backtest
make build        # ./bin/scale-backtest
```

Release binaries for Linux, macOS and Windows are attached to each [GitHub release](https://github.com/cb-Sigma/scale-backtest/releases).

## Quick start

With an HPA ([`examples/policies/hpa-running-requests.yaml`](examples/policies/hpa-running-requests.yaml)):

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: vllm-running-requests
spec:
  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: vllm}
  minReplicas: 1
  maxReplicas: 8
  metrics:
    - type: Pods
      pods:
        metric: {name: vllm:num_requests_running}
        target: {type: AverageValue, averageValue: "12"}
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 0
      policies: [{type: Pods, value: 4, periodSeconds: 30}]
    scaleDown:
      stabilizationWindowSeconds: 600
      policies: [{type: Pods, value: 1, periodSeconds: 120}]
```

```sh
scale-backtest run \
  --policy examples/policies/hpa-running-requests.yaml \
  --trace examples/traces/demo.csv \
  --replica examples/replicas/demo-gpu.yaml \
  --ttft-target 2s --e2e-target 60s \
  --slo "attainment>=99%,drop_rate<=0.1%"     # exit code 2 if not met
```

With a KEDA ScaledObject that scales to zero ([`examples/policies/keda-scale-to-zero.yaml`](examples/policies/keda-scale-to-zero.yaml)):

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: vllm-keda-zero
spec:
  scaleTargetRef: {name: vllm}
  pollingInterval: 15
  cooldownPeriod: 300
  minReplicaCount: 0
  maxReplicaCount: 8
  triggers:
    - type: prometheus
      metadata:
        serverAddress: http://prometheus.monitoring.svc:9090
        query: gateway_requests_in_flight
        threshold: "12"
        activationThreshold: "0"
```

```sh
scale-backtest run --policy examples/policies/keda-scale-to-zero.yaml \
  --trace examples/traces/demo.csv --replica examples/replicas/demo-gpu.yaml --ttft-target 2s
```

See what the tool understood from a manifest, including defaults and anything it will not simulate:

<!-- output: scale-backtest explain --policy examples/policies/keda-prometheus.yaml -->
```text
KEDA ScaledObject vllm-keda

KEDA operator loop (0 <-> 1)
  pollingInterval 15s, cooldownPeriod 300s, initialCooldownPeriod 0s
  minReplicaCount 1, maxReplicaCount 8
  minReplicaCount >= 1: the scaler is always active and activation thresholds are ignored
  trigger running: sum(vllm:num_requests_running)
    evaluated as sum(vllm:num_requests_running) over simulated metric requests_running
    threshold 12 (AverageValue), activationThreshold 0
  trigger waiting: sum(vllm:num_requests_waiting)
    evaluated as sum(vllm:num_requests_waiting) over simulated metric requests_waiting
    threshold 3 (AverageValue), activationThreshold 0

Generated HPA keda-hpa-vllm-keda (1 <-> N)
  minReplicas 1, maxReplicas 8
  metric s0-prometheus (External/AverageValue): target 12 <- simulated requests_running
  metric s1-prometheus (External/AverageValue): target 3 <- simulated requests_waiting
  behavior (after API defaulting)
    scaleUp: stabilizationWindow 0s, selectPolicy Max
      - Pods 4 per 15s
      - Percent 100 per 15s
    scaleDown: stabilizationWindow 300s, selectPolicy Max
      - Pods 1 per 60s
  tolerance: scale up 0.1, scale down 0.1; sync period 15s (controller defaults unless overridden by flags)
```

More:

```sh
scale-backtest compare --policy a.yaml --policy b.yaml --policy static:6 --trace t.csv --replica r.yaml
scale-backtest sweep --policy hpa.yaml --param target=4:16:2 --param scaleDown.stabilizationWindowSeconds=60,300,600 ...
scale-backtest run ... --format json            # or markdown
scale-backtest run ... --timeline-out timeline.csv --events-out events.csv --requests-out requests.csv
scale-backtest trace stats requests.csv         # burstiness, token distributions
scale-backtest trace scale requests.csv -o x2.csv --load 2 --time-compress 4
scale-backtest calibrate --bench bench.csv --base replica.yaml -o replica.yaml
```

## Inputs

| Input | What it is | Reference |
| --- | --- | --- |
| `--policy` | `autoscaling/v2` HPA or `keda.sh/v1alpha1` ScaledObject manifest, or `static:N` for a fixed fleet | [docs/algorithms.md](docs/algorithms.md) |
| `--trace` | per-request CSV or JSONL, or an Azure LLM inference trace; `trace from-prometheus` builds an approximate one from rate series | [docs/trace-formats.md](docs/trace-formats.md) |
| `--replica` | replica model: decode curve, prefill, batch and queue limits, cold start, node provisioning, GPU price | [docs/model.md](docs/model.md) |

Simulation settings, all flags of `run`, `compare` and `sweep`:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--ttft-target`, `--e2e-target` | none | latency targets used for attainment |
| `--slo` | none | objectives such as `attainment>=99%,ttft_p95<=2s,cost<=40`; exit code 2 if missed |
| `--seed` | 1 | the same seed gives byte-identical output |
| `--lb` | `least-requests` | or `round-robin` |
| `--scrape-interval` | 15s | autoscalers only see scraped metrics |
| `--rate-window` | 60s | window of `*_per_second` metrics |
| `--hpa-sync-period`, `--hpa-tolerance`, `--hpa-downscale-stabilization` | 15s, 0.1, 300s | the kube-controller-manager flags of the same names |
| `--initial-replicas` | policy minimum | replicas Ready at time 0 |
| `--gateway-queue` | unbounded | requests that may wait in front of the replicas when none can take them; 0 drops them |
| `--queue-timeout` | none | drop a request that has not started after this long |
| `--time-compress`, `--load` | 1 | replay faster, or with more or fewer requests |
| `--metric-alias` | | map your metric name onto a known one |

Exit codes: `0` success, `1` error, `2` an `--slo` objective was not met.

## What is implemented

Each item is implemented from the linked documentation and covered by a test that cites it. Full detail, including every deliberate simplification, is in [docs/algorithms.md](docs/algorithms.md).

### HorizontalPodAutoscaler ([docs](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/))

| Behaviour | Status |
| --- | --- |
| Periodic control loop, [sync period](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#how-does-a-horizontalpodautoscaler-work) 15 s | yes |
| [`desiredReplicas = ceil(currentReplicas x metric / target)`](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#algorithm-details), applied to ready pods | yes |
| [Tolerance](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#tolerance) band, cluster default 0.1 and per-direction `behavior.*.tolerance` | yes |
| [Not-yet-ready pods and missing metrics](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#algorithm-details): 0 on scale-up, target on scale-down, no direction reversal | yes |
| [Several metrics](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#scaling-on-multiple-metrics): the largest proposal wins; an unavailable metric blocks scale-down only | yes |
| `type: Pods` with `AverageValue`; `type: External` with `Value` or `AverageValue` | yes |
| [Stabilisation windows](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#stabilization-window) for scale-up and scale-down | yes |
| [Scaling policies](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#scaling-policies) `Pods` / `Percent` with `periodSeconds`, `selectPolicy` `Max` / `Min` / `Disabled` | yes |
| [Default behaviour](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#default-behavior), and the controller's built-in limits when `behavior` is absent | yes |
| `minReplicas` / `maxReplicas`; [scaling disabled at zero replicas](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#implicit-maintenance-mode-deactivation) | yes |
| [Scale to and from zero](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#scaling-to-and-from-zero) with `minReplicas: 0` on an external metric | yes |
| `type: Resource` / `ContainerResource` (CPU, memory), and the CPU initialisation-period rules | **refused**: the replica model has no CPU or memory usage |
| `type: Object` | **refused**: express it as `External` |
| Metric label selectors | accepted, ignored, reported |
| Controller restarts, API errors, failed scale updates, several HPAs on one target | not modelled |

### KEDA ScaledObject ([spec](https://keda.sh/docs/latest/reference/scaledobject-spec/), [concepts](https://keda.sh/docs/latest/concepts/scaling-deployments/))

| Behaviour | Status |
| --- | --- |
| [`pollingInterval`](https://keda.sh/docs/latest/reference/scaledobject-spec/#pollinginterval) (30 s): the operator loop that owns 0 <-> 1 | yes |
| [`cooldownPeriod`](https://keda.sh/docs/latest/reference/scaledobject-spec/#cooldownperiod) (300 s) before scaling to zero, [`initialCooldownPeriod`](https://keda.sh/docs/latest/reference/scaledobject-spec/#initialcooldownperiod) | yes |
| [`minReplicaCount`](https://keda.sh/docs/latest/reference/scaledobject-spec/#minreplicacount) (0), [`maxReplicaCount`](https://keda.sh/docs/latest/reference/scaledobject-spec/#maxreplicacount) (100), `idleReplicaCount: 0` | yes |
| [Activation thresholds](https://keda.sh/docs/latest/concepts/scaling-deployments/#activating-and-scaling-thresholds): strictly greater than, default 0, activation wins over scaling, ignored when `minReplicaCount >= 1` | yes |
| The generated HPA (`keda-hpa-<name>`): one external metric per trigger, `AverageValue` or `Value` [`metricType`](https://keda.sh/docs/latest/reference/scaledobject-spec/#triggers), `minReplicas >= 1`, [`advanced.horizontalPodAutoscalerConfig.behavior`](https://keda.sh/docs/latest/reference/scaledobject-spec/#horizontalpodautoscalerconfig) | yes |
| [`prometheus` trigger](https://keda.sh/docs/latest/scalers/prometheus/): `query`, `threshold`, `activationThreshold`, `ignoreNullValues`, `useCachedMetrics` | yes, for a [subset of PromQL](docs/algorithms.md#promql-subset) |
| Any other trigger type, `cpu` / `memory` triggers | **refused** |
| `advanced.scalingModifiers`, `idleReplicaCount` other than 0, pause annotations, unknown fields | **refused** |
| `fallback`, `authenticationRef`, label matchers | accepted, not simulated, reported |
| ScaledJob | not supported |

### PromQL

`metric`, `sum|avg|max|min(metric)`, `rate(counter[w])`, `sum|avg|max|min(rate(counter[w]))`, each optionally followed by `* number` or `/ number`. Anything else is refused with a message that lists these shapes. Label matchers are parsed and ignored.

### Metrics

Policies refer to engine metrics by name. `scale-backtest metrics` prints the full table; the short version:

| Name in a policy | Simulated value |
| --- | --- |
| `vllm:num_requests_running` | requests in a replica's running batch |
| `vllm:num_requests_waiting` | requests queued inside a replica |
| `vllm:generation_tokens_total`, `vllm:prompt_tokens_total`, `vllm:request_success_total` | counters, for `rate()` |
| `tokens_per_second`, `requests_per_second` | per-replica rates, as a metrics adapter exposes counters |
| `vllm:kv_cache_usage_perc`, `DCGM_FI_DEV_GPU_UTIL` | **proxies**: batch fill, and throughput relative to peak |
| `gateway_requests_pending`, `gateway_requests_in_flight`, `gateway_requests_total` | what a gateway in front of the replicas sees |

## The replica model

One replica is a continuous-batching server described by a handful of numbers ([docs/model.md](docs/model.md)):

- **Decode:** with `n` requests in the batch, each generates one token every `tpot(n) = a + b x n` seconds (or a table of measured points). Per-request speed falls as the batch fills; replica throughput `n / tpot(n)` rises and saturates.
- **Prefill:** time to first token is `(base + perToken x promptTokens) x (1 + interference x (n - 1))`.
- **Limits:** `maxConcurrency` requests in the batch, the rest wait in the replica's queue (unbounded by default, as in vLLM).
- **Cold start:** a constant, percentiles, samples, or [doriki](https://github.com/AmitwalaH/doriki) reports; plus an optional node-provisioning delay and warm-node retention.
- **Cost:** GPU price per hour, billed from the moment a replica is created until it is gone.

### Calibrating it

Run a load test at several fixed concurrency levels with your real prompt and output lengths (any tool: GuideLLM, a vLLM benchmark script, k6), then:

<!-- output: scale-backtest calibrate --bench examples/benchmarks/concurrency-sweep.csv --base examples/replicas/demo-gpu.yaml --prefill-base 40ms -->
```text
Fitted 8 points from examples/benchmarks/concurrency-sweep.csv (csv)

decode   tpot(n) = 13.922 ms + 0.9072 ms x n   (peak throughput 672 tokens/s at n=24)
prefill  40.0 ms + 0.1501 ms per prompt token, x (1 + 0.0400 x (n-1))

concurrency     TPOT ms     fitted     TTFT ms     fitted    tokens/s     fitted
          1       14.80      14.83       131.0      130.1          66         67
          2       15.90      15.74       134.0      135.3         123        127
          4       17.40      17.55       147.0      145.7         224        228
          8       21.40      21.18       165.0      166.4         364        378
         12       24.50      24.81       188.0      187.2         476        484
         16       28.60      28.44       206.0      208.0         545        563
         20       31.80      32.07       231.0      228.8         612        624
         24       35.90      35.69       249.0      249.6         650        672

Goodness of fit
  TPOT        R2 0.9992 · RMSE 0.204 ms · mean error 0.79% · worst error 1.26% (8 points)
  TTFT        R2 0.9988 · RMSE 1.42 ms · mean error 0.75% · worst error 0.97% (8 points)
  throughput  R2 0.9965 · RMSE 12.4 tokens/s · mean error 2.66% · worst error 3.77% (8 points)
note: prefill fitted for a mean prompt of 600 tokens; one prompt length cannot separate the fixed part of TTFT from the per-token part, so prefill.baseSeconds is taken as given (0.04s)

---
coldStart:
  percentiles:
    p50: 95s
    p90: 130s
    p99: 180s
cost:
  gpuPricePerHour: 2.5
  gpusPerReplica: 1
decode:
  baseSecondsPerToken: 0.0139218
  secondsPerTokenPerRequest: 0.000907186
description: Synthetic example, not a measurement of any real engine or GPU.
kind: ReplicaModel
maxConcurrency: 24
name: demo-gpu
node:
  provisionDelay:
    constant: 0s
prefill:
  baseSeconds: 0.04
  interference: 0.0399512
  secondsPerToken: 0.000150105
terminationGracePeriod: 600s
```

The benchmark file above is a synthetic sample. `calibrate` reads a simple CSV (`concurrency,ttft_ms,tpot_ms,throughput_tokens_per_s`) or a GuideLLM `benchmarks.json`, and `--doriki report.json` turns doriki cold-start reports into the cold-start distribution. Look at the goodness of fit before trusting the result: a low R2 or a large worst-case error means the two-parameter curve does not describe your engine, and you should give the measured points directly as `decode.curve`.

## Validation

What is checked, and how (`make test`; the numbers are printed by `go test -v ./internal/sim -run 'MM|Processor|Loss'`):

**Queueing theory.** With exponential service and Poisson arrivals the simulator must reproduce closed-form results. One run each, 200,000 requests:

| System | Quantity | Theory | Simulated | Error |
| --- | --- | ---: | ---: | ---: |
| M/M/1, rho 0.5 | mean wait | 1.0000 | 0.9909 | 0.9% |
| M/M/1, rho 0.8 | mean wait | 4.0000 | 3.9437 | 1.4% |
| M/M/1, rho 0.7 | p95 wait | 8.797 | 8.585 | 2.4% |
| M/M/4, rho 0.7 | mean wait (Erlang C) | 0.3572 | 0.3604 | 0.9% |
| M/M/8, rho 0.85 | mean wait (Erlang C) | 0.4771 | 0.4638 | 2.8% |
| M/M/8, rho 0.85 | P(wait) | 0.5726 | 0.5760 | |
| M/M/5/5, offered load 4 | blocking (Erlang B) | 0.1991 | 0.1969 | |
| M/M/1 processor sharing, rho 0.75 | mean sojourn | 4.0000 | 3.9739 | 0.7% |

The processor-sharing case exercises the continuous-batching progress accounting: with `tpot(n) = b x n` the batch shares a fixed capacity equally, and theory says every job, short or long, is slowed by `1 / (1 - rho)`.

**Documented examples.** The HPA maths is tested against the worked examples in the Kubernetes documentation (doubling and halving, the tolerance band, 80 replicas scaled down under `Pods: 4` and `Percent: 10` policies, `selectPolicy: Min` and `Disabled`, the default behaviour, stabilisation, missing metrics, scale to zero), and KEDA against its own (strict activation, `threshold: 10` with `activationThreshold: 50`, `20 / 5 = 4` and `3 x 20 / 5 = 12` for the two metric types). Each test quotes the passage it checks.

**Invariants**, on every bundled policy: every request is served or dropped exactly once; the sum of replica lifetimes equals the integral of the replica timeline, computed three ways; cost equals billed GPU-seconds times price; scale events form a chain.

**Determinism and golden files:** the same seed gives byte-identical JSON; end-to-end outputs are pinned in `internal/cli/testdata`, and the output blocks in this README are checked against the binary.

**What this does not establish:** that the replica model matches a real engine. That needs a comparison against a real cluster, which has not been done here.

## Limitations

- **The replica model is a fluid approximation, not an engine simulator.** No scheduler internals, no token-level batching effects, no KV-cache memory limits, no preemption or swapping. It has not been validated against vLLM, TGI, SGLang or any other engine.
- **Not modelled:** prefix or prompt caching (every prompt pays full prefill); heterogeneous GPUs or mixed replica types; multi-model routing and LoRA adapters (the `model` and `tenant` columns can only be used to slice a trace); request priorities, cancellation and client retries; streaming back-pressure; network latency; pod failures, evictions and spot interruptions; rolling updates; GPU quota or capacity limits beyond `maxReplicas`.
- **Metrics pipeline:** one scrape interval and Prometheus-style staleness. No adapter caching, no recording rules, no extrapolation in `rate()`. `vllm:kv_cache_usage_perc` and `DCGM_FI_DEV_GPU_UTIL` are proxies derived from batch occupancy.
- **Pod lifecycle:** a starting pod is treated as Running without metrics for its whole cold start. Scale-down removes not-yet-ready pods first, then the most recently ready ones. Billing starts when a replica is created.
- **Controller timing:** syncs happen at exact multiples of the sync period, in a fixed order (scrape, KEDA poll, HPA sync). Real controllers jitter.
- **Coarse traces** built from Prometheus rate series invent everything below the bucket length; the report says so on every run that uses one.
- **Output tokens are an input.** The simulator replays the output lengths in the trace; it cannot know how a different latency would have changed user behaviour.

## Independent project

scale-backtest is an independent open-source project. It is not affiliated with or endorsed by the Kubernetes project, the CNCF, KEDA, vLLM, NVIDIA, Microsoft or any other project or company mentioned here. Names are used only to describe compatibility.

## Related work

These are good tools aimed at different questions. Descriptions are from their own documentation at the time of writing; check them for the current state.

| Tool | What it simulates | Relation to scale-backtest |
| --- | --- | --- |
| [AISimulate](https://github.com/ai-dynamo/aisimulate) (NVIDIA) | predicts LLM serving behaviour and searches deployment configurations offline, with measured performance data; replay and sweeps; integrates the Dynamo Planner | far deeper engine and hardware modelling; autoscaling through Dynamo's planner, not an HPA or KEDA manifest |
| [BLIS](https://github.com/inference-sim/inference-sim) (inference-sim) | discrete-event simulation of an llm-d stack end to end: router, admission, KV cache, prefill/decode disaggregation, and autoscaling over node pools from its own policy config | the reference for llm-d fidelity; its autoscaler is configured in its own format |
| [Vidur](https://github.com/microsoft/vidur) (Microsoft) | high-fidelity LLM inference simulator from profiled operator timings; capacity planning and scheduler research; replays the Azure traces | models the engine and a fixed cluster configuration; no Kubernetes autoscaler |
| [LLMServingSim](https://github.com/casys-kaist/LLMServingSim) (KAIST) | cycle-level simulation of serving infrastructure, heterogeneous accelerators and memory tiers | hardware and system research; no Kubernetes autoscaler |
| [shibataka000/hpa-simulator](https://github.com/shibataka000/hpa-simulator) | the HPA algorithm for resource metrics, to debug its behaviour | HPA only, CPU metrics only, no workload model |
| [ksim](https://github.com/SecCodeSmith/ksim) | trace-driven Kubernetes cluster simulator for training and benchmarking autoscalers, with HPA/VPA/CA baselines | general workloads and cluster traces; not LLM serving, not a manifest backtest |
| [queueguard](https://github.com/aruntewatia87/queueguard) | queueing simulator and admission controller for LLM inference, fleet sizing | fixed fleets and admission control, no HPA or KEDA |
| KIS-S ([arXiv:2507.07932](https://arxiv.org/abs/2507.07932)) | a GPU-aware Kubernetes inference simulator with RL-based autoscaling | research on learned autoscalers |

A search of GitHub and the web in November 2026 for tools that take a real HPA or KEDA spec plus production history for LLM inference found none; if you know one, please open an issue so it can be listed.

If you need to know how an engine behaves on particular hardware, use one of the engine simulators above, or a benchmark. If you need to know whether the YAML you are about to apply would have survived last Tuesday, that is what this tool is for; a decode curve taken from one of those simulators is a perfectly good input here.

## FAQ

**How accurate is it?** The autoscaler logic follows the documentation and is tested against it. The latency numbers depend entirely on the replica model, which is an approximation you calibrate. Expect it to rank policies and expose failure modes (late reaction, flapping, cold-start pile-ups) more reliably than it predicts a p99 to the millisecond.

**Which engines does it support?** It does not run or emulate an engine. Any continuous-batching server whose per-request speed you can measure at several concurrency levels can be described. Metric names default to vLLM's; use `--metric-alias` for others.

**My HPA uses CPU utilisation.** That is refused on purpose: CPU use of a GPU inference pod says little about its load, and the model has no CPU. Remove the metric to backtest the others.

**Why does my scale-to-zero ScaledObject never wake up in the simulation?** Because its trigger reads a metric the replicas export, and at zero replicas there are no replicas to export it. The same happens in a real cluster. Trigger on something in front of the replicas (`gateway_requests_pending`, `gateway_requests_in_flight`).

**I only have Prometheus history, not request logs.** `scale-backtest trace from-prometheus` synthesises a trace from request-rate and token-rate series. Use the smallest step you have, and read the result as approximate: bursts shorter than a bucket are gone.

**Can it read my cluster directly?** No, by design: it reads files. Export the manifest with `kubectl get hpa NAME -o yaml` (remove `status` and server-side metadata, or strict decoding will tell you which fields to remove) and the history with a Prometheus `query_range` call.

**Is it deterministic?** Yes. The seed drives cold-start draws and trace transformations; the same inputs and seed give byte-identical output.

**How fast is it?** The 4,797-request demo runs in a few milliseconds, and a 200,000-request validation run in well under a second on a laptop, so sweeps over hundreds of settings are practical.

## Roadmap

- Validation of the replica model against a real engine on a real GPU, published with the data.
- Prefix-cache hit rates as a trace column and a model parameter.
- Several replica types behind one policy (heterogeneous GPUs), and per-model routing.
- More KEDA triggers that make sense for inference (`metrics-api`, `external`), `scalingModifiers`, `fallback`.
- More PromQL: `histogram_quantile` over simulated latency histograms, arithmetic between two selectors.
- Controller timing jitter and metric-adapter caching.
- Gateway API Inference Extension and llm-d style endpoint pickers as load-balancing policies.
- A request-log importer for common gateway access-log formats.

## Contributing

Contributions are welcome, especially calibration data from real engines, corrections where the implementation deviates from the Kubernetes or KEDA documentation, and trace format importers. See [CONTRIBUTING.md](CONTRIBUTING.md) and the [code of conduct](CODE_OF_CONDUCT.md). Security reports: [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE). Copyright 2026 The scale-backtest Authors.
