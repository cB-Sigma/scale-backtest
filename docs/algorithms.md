# Autoscaler algorithms

This page says exactly what scale-backtest does when it plays the part of the
HorizontalPodAutoscaler controller and of KEDA, where each rule comes from, and
where the simulation simplifies. The implementation is in `internal/hpa` and
`internal/keda`; the tests next to it quote the documentation passages they
check.

Sources:

- Kubernetes, *Horizontal Pod Autoscaling*:
  <https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/>
- Kubernetes, *HorizontalPodAutoscaler Walkthrough*:
  <https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale-walkthrough/>
- KEDA, *ScaledObject specification*:
  <https://keda.sh/docs/latest/reference/scaledobject-spec/> (read at version 2.18)
- KEDA, *Scaling Deployments, StatefulSets & Custom Resources*:
  <https://keda.sh/docs/latest/concepts/scaling-deployments/>
- KEDA, *Prometheus scaler*: <https://keda.sh/docs/latest/scalers/prometheus/>

Where the documentation leaves a detail open (rounding, the order of checks,
what exactly is multiplied by the ratio), the implementation follows the
upstream controller source, `pkg/controller/podautoscaler` in
kubernetes/kubernetes, as of November 2026.

## The simulated cluster, as the autoscaler sees it

Every `--scrape-interval` (15 s) each Ready or Terminating replica and the
gateway are scraped. Autoscalers never see live state, only the latest scraped
sample, and no sample older than 5 minutes. Events at the same instant are
processed in this order: replica state changes, scrape, KEDA poll, HPA sync.

| Replica state | Pod phase it stands for | For a `Pods` metric |
| --- | --- | --- |
| Pending (waiting for a node) | `Pending` | "not yet ready" |
| Starting (cold start) | `Running`, not Ready, exporting nothing | "missing metrics" |
| Ready, not yet scraped | `Running`, Ready | "missing metrics" |
| Ready | `Running`, Ready | its scraped value |
| Terminating | has a deletion timestamp | ignored |

Treating the whole cold start as "running without metrics" is a
simplification: in a real pod the image pull happens in phase `Pending`. It
only matters on a scale-down decision taken while pods are starting, where
missing pods are assumed to be at the target and pending pods are left out.

`scale.spec.replicas` is the desired count; `scale.status.replicas` counts
Pending, Starting and Ready replicas; "ready pods" counts Ready and Terminating
ones, as the documentation says terminating pods that are still Ready do.

## HorizontalPodAutoscaler

### The loop

Every `--hpa-sync-period`
([15 s](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#how-does-a-horizontalpodautoscaler-work)),
starting one period after the start of the trace:

1. If the target has 0 replicas: scaling is disabled
   ([implicit maintenance mode](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#implicit-maintenance-mode-deactivation)),
   unless the HPA itself scaled it to zero (see [scale to zero](#scale-to-zero)).
2. If current replicas are above `maxReplicas` or below `minReplicas`: go to
   the bound, without looking at metrics.
3. Otherwise compute a proposal per metric and take the largest
   ([multiple metrics](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#scaling-on-multiple-metrics)).
   If every metric failed, do nothing. If some failed and the rest propose
   fewer replicas than now, do nothing; a scale-up still goes ahead
   ([algorithm details](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#algorithm-details)).
4. Stabilise and rate-limit the proposal (below), clamp to min/max, and write
   the result if it differs from the current count.

The reason attached to a scale event is the text of the controller's
`SuccessfulRescale` event: `<metric> above target`, `All metrics below target`,
`Current number of replicas below Spec.MinReplicas`, and so on. The detail line
adds each metric's value, target and proposal, and the `ScalingLimited` or
`AbleToScale` condition reason (`ScaleUpLimit`, `ScaleDownStabilized`, ...)
when it shaped the result.

### Per-metric proposals

All arithmetic is done in milli-units with integer division for averages, as
the controller does on `resource.Quantity` values.

**`type: Pods`, `AverageValue`**
([algorithm details](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#algorithm-details)):

```text
ratio = average(metric over pods with a sample) / target
if no pending pods affect a scale-up and no pod is missing metrics:
    if ratio within tolerance: keep current replicas
    else: ceil(ratio x number of pods with a sample)
otherwise:
    missing pods count as 0 on a scale-up, as exactly the target on a scale-down
    pending pods count as 0 on a scale-up (and are left out on a scale-down)
    recompute the ratio over all those pods
    if the new ratio is within tolerance or points the other way: keep current
    else: ceil(new ratio x number of pods counted), unless that moves against the ratio
```

Note that the multiplier is the number of pods that reported, not
`spec.replicas`; the documentation's
`ceil(currentReplicas x currentMetricValue / desiredMetricValue)` is the case
where the two agree. If no pod has a sample the metric is invalid.

**`type: External`, `AverageValue`**
([walkthrough](https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale-walkthrough/#autoscaling-on-metrics-not-related-to-kubernetes-objects)):
`ceil(total / target)`, unless `total / (target x status.replicas)` is within
tolerance. The total is the sum of the metric over all replicas, or the gateway
value.

**`type: External`, `Value`:** `ratio = total / target`; within tolerance, keep
the current count; otherwise `ceil(ratio x ready pods)`; from zero replicas,
`ceil(ratio)`.

An external metric with no series at all (no replica has been scraped) is an
invalid metric.

### Tolerance

The proposal is ignored when `1 - down <= ratio <= 1 + up`
([tolerance](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#tolerance)).
Both default to `--hpa-tolerance` (0.1); `behavior.scaleUp.tolerance` and
`behavior.scaleDown.tolerance` override them per HPA. The bounds are inclusive.

### Stabilisation and rate limits

Two code paths exist upstream, and both are implemented.

**Without `spec.behavior`:**

- the proposal is replaced by the highest proposal of the last
  `--hpa-downscale-stabilization` (300 s);
- a scale-up is capped at `max(2 x current, 4)` replicas per sync.

**With `spec.behavior`** (after the API server's defaulting,
[default behavior](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#default-behavior):
scale-up window 0 with `Pods: 4 / 15 s` and `Percent: 100 / 15 s`, `Max`;
scale-down window 300 s with `Percent: 100 / 15 s`):

- [stabilisation](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#stabilization-window):
  the current count is raised to the *lowest* proposal of the scale-up window
  and lowered to the *highest* proposal of the scale-down window;
- [policies](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#scaling-policies):
  for each policy, the replica count at the start of its period is
  reconstructed from the scale events inside `periodSeconds`; a `Pods` policy
  allows `start +/- value`, a `Percent` policy `ceil(start x (1 + value/100))`
  upwards and `floor(start x (1 - value/100))` downwards. `selectPolicy: Max`
  takes the most permissive limit, `Min` the least, `Disabled` forbids the
  direction.

A manifest that sets only part of `behavior` gets the defaults for the rest,
and its `policies` list replaces the default list as a whole.

`scale-backtest explain --policy FILE` prints the behaviour in effect.

### Scale to zero

[Scaling to and from zero](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/#scaling-to-and-from-zero):
`minReplicas: 0` needs at least one External metric. When the HPA takes the
target to zero it remembers that (the `ScaledToZero` condition) and keeps
evaluating external metrics at zero replicas; a target that was at zero for any
other reason is left alone. Per-pod metrics cannot be computed without pods, so
only an external metric that still has a value at zero replicas (a gateway
metric) can bring the workload back. The tool warns when a policy scales to
zero on replica metrics only.

### Not implemented

- `Resource` and `ContainerResource` metrics, and with them the CPU
  initialisation period and initial readiness delay: **refused**.
- `Object` metrics: **refused**.
- Metric label selectors: ignored with a warning.
- `--horizontal-pod-autoscaler-*` flags other than the three exposed.
- Controller restarts (which forget recommendations and scale events), API
  conflicts, rate-limited requeues, and sync jitter.

## KEDA

KEDA splits the work
([activating and scaling thresholds](https://keda.sh/docs/latest/concepts/scaling-deployments/#activating-and-scaling-thresholds)):
its operator decides between zero and one replica, and an HPA that KEDA creates
handles one to N.

### The operator loop (0 <-> 1)

Every [`pollingInterval`](https://keda.sh/docs/latest/reference/scaledobject-spec/#pollinginterval)
(default 30 s), starting at time 0, each trigger's query is evaluated on the
scraped metrics.

- A trigger is **active** when its value is strictly greater than its
  `activationThreshold` (default 0). The ScaledObject is active when any
  trigger is.
- Active at 0 replicas: scale to `minReplicaCount`, or to 1 when that is 0.
- Not active, replicas above 0, and the ScaledObject may go to zero
  (`minReplicaCount: 0`, or `idleReplicaCount: 0`): scale to 0 once
  [`cooldownPeriod`](https://keda.sh/docs/latest/reference/scaledobject-spec/#cooldownperiod)
  (default 300 s) has passed since the last poll that saw a trigger active, and
  not before
  [`initialCooldownPeriod`](https://keda.sh/docs/latest/reference/scaledobject-spec/#initialcooldownperiod).
  A workload that was never seen active is scaled in at the first poll.
- Replicas below `minReplicaCount` (without `idleReplicaCount`): scale up to it.
- With `minReplicaCount >= 1` and no `idleReplicaCount`, KEDA never changes the
  replica count and activation thresholds have no effect.

Because activation is decided on the polling grid and from scraped values, a
request that arrives at zero replicas waits up to one scrape interval plus one
polling interval before KEDA even starts a pod, and then the whole cold start.

### The generated HPA (1 <-> N)

`keda-hpa-<name>` has `minReplicas = max(minReplicaCount, 1)`,
`maxReplicas = maxReplicaCount` (default 100), `behavior` from
`advanced.horizontalPodAutoscalerConfig.behavior` (absent by default, so the
controller's built-in rules apply), and one External metric per trigger named
`s<index>-<type>`, with the trigger's `threshold` as an `AverageValue` target,
or a `Value` target with `metricType: Value`. It runs the HPA loop above on its
own period. At zero replicas it is in the HPA's disabled state, which is why it
never fights the operator.

By default the HPA's metric request triggers a fresh evaluation of the query;
with `useCachedMetrics: true` it gets the value from the last operator poll.

### The prometheus trigger

`query`, `threshold`, `activationThreshold`, `ignoreNullValues` (default true:
a query that selects nothing counts as 0; false: it is an error and the metric
is invalid for that sync). `serverAddress` and the other connection settings
are accepted and unused.

#### PromQL subset

```text
metric                              gateway metrics only (a single series)
sum|avg|max|min(metric)
rate(counter[w])                    gateway counters only
sum|avg|max|min(rate(counter[w]))
<any of the above> * number
<any of the above> / number
```

`rate()` is `(last - first) / (t_last - t_first)` over the scraped samples
inside the window, without Prometheus' extrapolation to the window edges; it
needs two samples. Label matchers are parsed and ignored (there is one model
and one series per replica). Grouping (`by`, `without`), binary operations
between selectors, subqueries, offsets, `histogram_quantile`, `*_over_time`,
`irate` and everything else are refused with a message listing the supported
shapes.

### Not implemented

Refused: every trigger type other than `prometheus`; `cpu` and `memory`
triggers; `advanced.scalingModifiers`; `idleReplicaCount` other than 0 (KEDA's
documentation itself says only 0 works); the pause annotations; any field the
tool does not know. Accepted but not simulated, with a warning: `fallback`
(scalers never fail in a backtest), `authenticationRef`. Not supported at all:
ScaledJob, push scalers, the KEDA HTTP add-on's interceptor (its queueing role
is approximated by the gateway queue).

## Metric names

`scale-backtest metrics` prints this table from the binary.

<!-- output: scale-backtest metrics -->
```text
Simulated series
  requests_running             gauge, per replica: requests in the running batch of a replica (prefill or decode)
  requests_waiting             gauge, per replica: requests queued inside a replica, not yet in the batch
  kv_cache_usage               gauge, per replica: proxy: running requests / maxConcurrency, 0..1
  gpu_utilization              gauge, per replica: proxy: decode throughput at the current batch size / peak throughput, 0..100
  generation_tokens_total      counter, per replica: output tokens generated by a replica
  prompt_tokens_total          counter, per replica: prompt tokens prefilled by a replica
  requests_completed_total     counter, per replica: requests completed by a replica
  gateway_requests_pending     gauge, gateway: requests held in front of the replicas because none can accept them
  gateway_requests_in_flight   gauge, gateway: requests accepted and not yet finished, anywhere in the system
  gateway_requests_total       counter, gateway: requests that arrived

Accepted names
  vllm:num_requests_running            -> requests_running  (vLLM)
  vllm_num_requests_running            -> requests_running  (vLLM via a metrics adapter)
  num_requests_running                 -> requests_running
  requests_running                     -> requests_running  (canonical)
  tgi_batch_current_size               -> requests_running  (TGI, by analogy)
  vllm:num_requests_waiting            -> requests_waiting  (vLLM)
  vllm_num_requests_waiting            -> requests_waiting  (vLLM via a metrics adapter)
  num_requests_waiting                 -> requests_waiting
  requests_waiting                     -> requests_waiting  (canonical)
  queue_depth                          -> requests_waiting
  tgi_queue_size                       -> requests_waiting  (TGI, by analogy)
  vllm:kv_cache_usage_perc             -> kv_cache_usage  (vLLM; proxy)
  vllm:gpu_cache_usage_perc            -> kv_cache_usage  (older vLLM name, absent from 0.28 onward; proxy)
  vllm_kv_cache_usage_perc             -> kv_cache_usage  (proxy)
  vllm_gpu_cache_usage_perc            -> kv_cache_usage  (proxy)
  kv_cache_usage                       -> kv_cache_usage  (canonical; proxy)
  DCGM_FI_DEV_GPU_UTIL                 -> gpu_utilization  (DCGM exporter; proxy)
  gpu_utilization                      -> gpu_utilization  (canonical; proxy)
  gpu_util                             -> gpu_utilization  (proxy)
  vllm:generation_tokens_total         -> generation_tokens_total  (vLLM counter)
  vllm_generation_tokens_total         -> generation_tokens_total  (counter)
  generation_tokens_total              -> generation_tokens_total  (canonical counter)
  vllm:generation_tokens_per_second    -> rate(generation_tokens_total)  (rate; a recording-rule name, not a native vLLM metric)
  vllm_generation_tokens_per_second    -> rate(generation_tokens_total)  (rate; a recording-rule name)
  generation_tokens_per_second         -> rate(generation_tokens_total)  (rate)
  tokens_per_second                    -> rate(generation_tokens_total)  (rate)
  vllm:prompt_tokens_total             -> prompt_tokens_total  (vLLM counter)
  vllm_prompt_tokens_total             -> prompt_tokens_total  (counter)
  prompt_tokens_total                  -> prompt_tokens_total  (canonical counter)
  prompt_tokens_per_second             -> rate(prompt_tokens_total)  (rate)
  vllm:request_success_total           -> requests_completed_total  (vLLM counter)
  vllm_request_success_total           -> requests_completed_total  (counter)
  requests_completed_total             -> requests_completed_total  (canonical counter)
  requests_per_second                  -> rate(requests_completed_total)  (rate)
  gateway_requests_pending             -> gateway_requests_pending  (canonical)
  gateway_requests_in_flight           -> gateway_requests_in_flight  (canonical)
  gateway_requests_total               -> gateway_requests_total  (canonical counter)
  gateway_requests_per_second          -> rate(gateway_requests_total)  (rate)
```

Notes:

- `requests_running` counts requests in the batch, prefilling or decoding.
- `kv_cache_usage` and `gpu_utilization` are **proxies**. Real KV-cache use
  depends on context lengths and real GPU utilisation is close to 100%
  whenever anything is running; treat policies built on them with suspicion,
  in simulation and in production.
- The `*_per_second` names are the per-replica rate of a counter over
  `--rate-window` (60 s), which is how a custom-metrics adapter usually exposes
  counters to an HPA.
- The gateway metrics stand for whatever sits in front of your replicas
  (ingress, gateway, service mesh). They are the only metrics that exist at
  zero replicas.
- For an engine with other metric names:
  `--metric-alias tgi_queue_size=vllm:num_requests_waiting`.
