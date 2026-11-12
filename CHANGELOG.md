# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `run`: replay a trace through an `autoscaling/v2` HorizontalPodAutoscaler, a
  KEDA `ScaledObject` or a fixed fleet, and report TTFT and end-to-end
  percentiles, target attainment, drops, the replica timeline, scale events
  with reasons, GPU-hours, cost and idle share.
- `compare` for several policies side by side and `sweep` for parameter grids
  with the cost-versus-attainment frontier.
- `--slo` objectives with exit code 2 for CI.
- HPA control loop: tolerance, missing-metric and not-yet-ready handling,
  multiple metrics, stabilisation windows, scaling policies and
  `selectPolicy`, API defaults, the built-in limits without `behavior`, and
  scale to zero on external metrics.
- KEDA: polling interval, cooldown, activation thresholds, scale to and from
  zero, the generated HPA, and the `prometheus` trigger for a subset of PromQL.
- Trace formats: CSV, JSONL, the Azure LLM inference trace layout, and an
  approximate mode from Prometheus rate series. `trace stats`, `scale`,
  `slice`, `convert`, `synth`.
- Replica model with a decode curve, prefill cost, batch and queue limits,
  cold-start distributions, node provisioning and cost. `calibrate` from a
  benchmark CSV or a GuideLLM report, cold start from doriki reports.
- Output as table, JSON and Markdown; timeline, events and per-request CSV.
- `explain` and `metrics` commands.
- `--demo` with a bundled synthetic trace.
