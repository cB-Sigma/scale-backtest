# Contributing

Thanks for considering a contribution. The most valuable ones are:

- **Calibration and validation data.** A benchmark of a real engine, or a
  comparison of a simulated replay with a real cluster. The replica model has
  not been validated against real hardware, and data is what will change that.
  There is an issue template for it.
- **Corrections to the autoscaler logic.** If the simulated HPA or KEDA
  behaviour differs from the documentation or from what you observe on a
  cluster, open an issue with the manifest and the doc section.
- **Trace importers**, more KEDA triggers and more PromQL shapes, each with the
  documentation that defines them.

## Development

You need Go 1.26 or newer and, for linting, golangci-lint v2.

```sh
make build        # ./bin/scale-backtest
make test         # go test ./... -race -count=1
make test-short   # smaller queueing-theory samples, a few seconds
make lint         # golangci-lint run ./...
make fmt-check vet
make validate     # print the queueing-theory comparison
```

## Layout

| Path | Contents |
| --- | --- |
| `cmd/scale-backtest` | the binary |
| `internal/hpa` | the HPA control loop, with no clock and no I/O |
| `internal/keda` | the KEDA operator loop and the HPA it generates |
| `internal/policy` | manifests to policies, strict decoding, sweep parameters |
| `internal/promql` | the supported PromQL subset |
| `internal/metrics` | metric names and the store of scraped samples |
| `internal/trace` | trace formats, statistics, transformations |
| `internal/replica` | replica model, distributions, calibration, doriki import |
| `internal/sim` | the discrete-event simulation |
| `internal/report` | table, Markdown, JSON and CSV output, SLO evaluation |
| `internal/cli` | commands and flags |
| `examples/`, `docs/` | sample inputs (all synthetic) and documentation |

## Rules of the house

1. **Autoscaler behaviour comes from the documentation.** A change to
   `internal/hpa` or `internal/keda` needs a test that quotes the passage of
   the Kubernetes or KEDA documentation it implements. Where the docs are
   silent and you follow the upstream source, say so in the test and in
   `docs/algorithms.md`.
2. **Refuse rather than guess.** A manifest field that is not simulated is an
   error, or a warning that reaches the report. Nothing is dropped silently.
3. **Determinism.** A run is a pure function of its inputs and seed. No wall
   clock, no map iteration order in anything that reaches the output, one
   seeded random source.
4. **Honest documentation.** If a change adds an approximation, describe it in
   `docs/` and in the limitations in the README. Do not describe the tool as
   validated against anything it has not been validated against.
5. **Small synthetic samples only.** No production traces and no large files
   in the repository. Say in the file or in `examples/README.md` that a sample
   is synthetic.

## Golden files and documentation output

End-to-end outputs are pinned in `internal/cli/testdata/*.golden`, and the
output blocks in `README.md` and `docs/` (those introduced by
`<!-- output: scale-backtest ... -->`) are checked against the binary. After a
change that alters output:

```sh
make golden
git diff          # read it: every changed number should be one you can explain
```

Golden files are generated on linux/amd64, which is what CI runs. On another
architecture a last-digit floating-point difference is possible; if you see
one, mention it in the pull request rather than regenerating the files there.

## Pull requests

- One topic per pull request, with tests.
- `make fmt-check vet lint test` must pass.
- Add a line to `CHANGELOG.md` under "Unreleased".
- Commit messages: a short imperative summary, then why.

By contributing you agree that your contribution is licensed under the
Apache License 2.0, as the rest of the project is.
