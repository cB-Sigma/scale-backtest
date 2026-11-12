# Examples

Everything in this directory is **synthetic**: invented numbers in the right
formats, small enough to read. Nothing here was measured on real hardware or
taken from a production system.

| Path | What it is |
| --- | --- |
| `policies/hpa-queue-depth.yaml` | HPA on `vllm:num_requests_waiting`, no `behavior` |
| `policies/hpa-running-requests.yaml` | HPA on `vllm:num_requests_running` with scale-up and scale-down policies |
| `policies/hpa-token-throughput.yaml` | HPA on tokens per second plus queue depth (two metrics) |
| `policies/hpa-scale-to-zero.yaml` | HPA `minReplicas: 0` on an external gateway metric |
| `policies/keda-prometheus.yaml` | KEDA ScaledObject, two Prometheus triggers, `minReplicaCount: 1` |
| `policies/keda-scale-to-zero.yaml` | KEDA ScaledObject that scales to zero on a gateway metric |
| `replicas/demo-gpu.yaml` | replica model used by `--demo` |
| `replicas/cold-node-pool.yaml` | same engine with a measured-points decode curve and node provisioning |
| `traces/demo.csv` | the `--demo` trace: 45 minutes, two bursts |
| `traces/sample.jsonl` | JSONL trace with model and tenant |
| `traces/azure-format-sample.csv` | ten invented rows in the column layout of the Azure LLM inference traces |
| `benchmarks/concurrency-sweep.csv` | benchmark CSV for `calibrate` |
| `benchmarks/guidellm-benchmarks.json` | the fields `calibrate` reads from a GuideLLM report |
| `coldstart/doriki-*.json` | the fields `calibrate --doriki` reads from a doriki report |
| `prometheus/*.json` | `query_range` responses for `trace from-prometheus` |

The demo trace is reproducible:

```sh
scale-backtest trace synth -o examples/traces/demo.csv --seed 7 \
  --profile "0:0.5,8m:0.5,10m:3,18m:3,20m:1,30m:1,31m:4.5,36m:4.5,38m:0.5,45m:0.4" \
  --prompt-mean 600 --output-mean 400
```
