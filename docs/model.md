# The replica model

scale-backtest does not simulate an inference engine. It simulates *one number
of replicas changing over time*, and needs just enough of a replica to say how
long requests take at a given load. This page describes that model exactly, so
that you can judge whether it fits your engine and calibrate it.

> The model is an approximation and has **not** been validated against a real
> GPU inference engine in this repository. Calibrate it from your own load
> test and check the goodness of fit.

## File format

```yaml
kind: ReplicaModel
name: my-model-on-l40s
description: where the numbers came from

prefill:
  baseSeconds: 0.04          # fixed part of time to first token
  secondsPerToken: 0.00015   # per prompt token, request running alone
  interference: 0.04         # relative slowdown per other request in the batch

decode:                      # either the two-parameter line ...
  baseSecondsPerToken: 0.014
  secondsPerTokenPerRequest: 0.0009
  # ... or measured points, linearly interpolated:
  # curve:
  #   - {concurrency: 1,  secondsPerToken: 0.0149}
  #   - {concurrency: 24, secondsPerToken: 0.0356}

maxConcurrency: 24           # requests in the running batch (vLLM: --max-num-seqs)
maxQueue: 200                # requests waiting inside the replica; omit for unbounded

coldStart:                   # exactly one of: constant, percentiles, samples, doriki
  percentiles: {p50: 95s, p90: 130s, p99: 180s}

node:                        # optional
  provisionDelay: {percentiles: {p50: 150s, p90: 210s}}
  keepWarm: 10m
  spareNodes: 0

terminationGracePeriod: 600s

cost:
  gpuPricePerHour: 2.50
  gpusPerReplica: 1
```

Durations are Go duration strings (`90s`, `2m30s`) or plain numbers of seconds.
Unknown fields are errors.

## A request's life

1. **Arrival.** The load balancer picks a Ready replica that has room
   (`least-requests`: fewest running plus queued, ties to the oldest replica;
   `round-robin`: the next one in turn). A replica has room if its batch is not
   full or its queue is not full. If no replica has room, the request waits in
   the gateway queue (`--gateway-queue`, unbounded by default) or is dropped.
2. **Queue.** If the replica's batch is full, the request waits in the
   replica's FIFO queue. This is what `vllm:num_requests_waiting` reports.
3. **Prefill.** Once in the batch, the request needs
   `prefill.baseSeconds + prefill.secondsPerToken x promptTokens` seconds of
   solo work. With `n` requests in the batch, that work proceeds
   `1 + prefill.interference x (n - 1)` times slower. The end of prefill is the
   **first token** (the prefill pass produces it); TTFT is measured from
   arrival to here, so it includes all queueing.
4. **Decode.** The remaining `outputTokens - 1` tokens are generated at
   `1 / tpot(n)` tokens per second, where `n` is the number of requests in the
   batch (prefilling or decoding) *at each moment*. When the batch grows or
   shrinks, every request's speed changes from that instant.
5. **Completion.** End-to-end latency is arrival to last token.

Between events the batch composition is constant, so progress is exact: the
simulator jumps from one batch change to the next. There are no decode-step
quanta, and no random noise in service times.

### The decode curve

`tpot(n) = a + b x n`, with `a = decode.baseSecondsPerToken` and
`b = decode.secondsPerTokenPerRequest`.

- one request alone generates `1 / (a + b)` tokens per second;
- replica throughput with `n` requests is `n / (a + b x n)`, which rises with
  `n` and approaches `1 / b`;
- the cost of batching is paid by every request in the batch.

This two-parameter shape is the simplest one that has both properties every
continuous-batching engine shows (per-request speed falls, total throughput
rises and saturates). If your measurements bend differently, give them directly
as `decode.curve`. Between points the curve is interpolated linearly; below the
first point it is flat; above the last it continues the last segment (never
downwards).

### What `n` leaves out

Real engines slow down with total tokens in the batch (context length), not
just the number of sequences; long prompts cost more in every decode step.
Here a request with a 6,000-token context and one with 60 tokens load the batch
equally during decode. Calibrate with a prompt and output mix like your
production traffic so that the average is right.

## Cold start, nodes and scale-down

- A new replica needs a node. It takes a permanently spare node
  (`node.spareNodes`), else a node kept warm after an earlier scale-down, else
  waits `node.provisionDelay` in state **Pending**.
- On a node, it spends `coldStart` in state **Starting** (image pull, weights,
  warm-up), then becomes **Ready**. It exports no metrics until Ready and is
  first seen by an autoscaler at the next scrape.
- Replicas that are Ready at time 0 (`--initial-replicas`) are warm.
- A scale-down removes Pending replicas first, then Starting ones, then the
  most recently Ready. A Ready replica becomes **Terminating**: it gets no new
  requests, finishes the ones it accepted (running and queued), and is gone
  when it is empty or after `terminationGracePeriod`, whichever is first.
  Requests cut off by the grace period are dropped with reason `terminated`.
- A node whose replica is gone stays for `node.keepWarm` (billed, reusable
  without provisioning delay), then is released.

### Distributions

`coldStart` and `node.provisionDelay` accept:

| Form | Sampling |
| --- | --- |
| `constant: 110s` | always that value |
| `percentiles: {p50: 95s, p90: 130s, p99: 180s}` | piecewise-linear inverse CDF through the points. Without `p0`/`min`, the lower end mirrors the first segment (here `95 - (130 - 95) = 60s`), but never below half the lowest value. Without `p100`/`max`, the highest given value is the maximum. |
| `samples: [98s, 106s, 131s]` | each draw picks one |
| `doriki: [a.json, b.json]` | one sample per [doriki](https://github.com/AmitwalaH/doriki) JSON report (paths relative to the model file) |

From a doriki report (`doriki <pod> -o json`, schema `report.v1`) the tool
reads `totals.pending_to_ready_seconds` and the `phases[]` entries with state
`complete`. If the `node-provision` phase completed, its seconds are subtracted,
because node provisioning is modelled separately; put that time in
`node.provisionDelay`.

## Cost

A replica is billed `cost.gpuPricePerHour x cost.gpusPerReplica` from creation
(including Pending and Starting) until it is gone (including draining). Empty
warm and spare nodes are billed too. The report splits billed time into
*starting*, *busy* (at least one request running) and *idle*, and gives the
mean batch fill (running requests / `maxConcurrency` while serving).

The run ends when the trace ends, or when the last request finishes if that is
later; cost is counted to that point.

## Calibration

`scale-backtest calibrate --bench FILE` fits the decode curve and the prefill
cost to a load test.

**Benchmark CSV.** One row per concurrency level; units are part of the column
name:

```csv
concurrency,ttft_ms,tpot_ms,throughput_tokens_per_s,prompt_tokens,output_tokens
1,131,14.8,66,600,400
8,165,21.4,364,600,400
24,249,35.9,650,600,400
```

`tpot_ms` (or `itl_ms`, `tpot_s`, `itl_s`) should be the inter-token latency,
excluding the first token. `ttft_ms`, the throughput and the token counts are
optional.

**GuideLLM.** From a `benchmarks.json` report, for each entry of
`benchmarks[]`, the `successful` distribution of these `metrics` is read:
`request_concurrency.mean`, `time_to_first_token_ms.median`,
`inter_token_latency_ms.mean` (falling back to `time_per_output_token_ms.mean`),
`output_tokens_per_second.mean`, `prompt_token_count.mean`,
`output_token_count.mean`. The field names were taken from GuideLLM's schema
sources; the importer has been tested on a hand-made file with those fields,
not on a report produced by a real GuideLLM run.

**The fit.**

- Decode: least squares of `tpot = a + b x concurrency`, constrained to
  `a, b >= 0`. If TPOT is missing it is derived as `concurrency / throughput`,
  which folds prefill time into the curve (the output says so).
- Prefill: least squares of `ttft = alpha + beta x concurrency`, then
  `T1 = alpha + beta` is the single-request TTFT,
  `interference = beta / T1` and
  `secondsPerToken = (T1 - baseSeconds) / promptTokens`. A benchmark with one
  prompt length cannot separate the fixed part of TTFT from the per-token part,
  so `baseSeconds` is what you pass as `--prefill-base` (default 0).
- Reported per relation: R2, RMSE, mean and worst absolute percentage error,
  and a table of measured against fitted values. Throughput is a cross-check
  (it is not fitted unless TPOT was derived from it).

**Getting a usable benchmark.**

- Use fixed-concurrency (closed-loop) runs. In an open-loop run above
  saturation, TTFT includes queueing and will corrupt the prefill fit.
- Keep concurrency at or below the engine's batch limit, and cover the range
  from 1 to that limit with at least five or six levels.
- Use prompt and output lengths like production. The model has one curve; it
  will be right on average for the mix you measured.
- Measure the cold start separately, several times, from scale-up to Ready.

Everything a benchmark cannot tell (cold start, queue limit, price) comes from
`--base`.

## What the model does not capture

Prefix caching, KV-cache memory pressure and preemption, per-step scheduling
detail, chunked prefill policies, speculative decoding, context-length effects
on decode speed, multi-GPU replicas with non-linear scaling, heterogeneous
replicas, and warm-up effects after a cold start (a replica is at full speed
the moment it is Ready). If one of these dominates your latency, this model
will be wrong about it.
