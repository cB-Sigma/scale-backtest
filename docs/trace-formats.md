# Trace formats

A trace is a list of requests: when each arrived, how many prompt tokens it
had and how many output tokens it produced. The format is detected from the
content; `--trace-format csv|jsonl|azure` forces it.

## Native CSV

```csv
# duration_seconds: 2700
timestamp,prompt_tokens,output_tokens,model,tenant
0.941,232,293,chat-8b,acme
3.385,1220,58,chat-8b,globex
```

- **Header required.** Column names are matched case-insensitively, ignoring
  `_`, `-` and spaces. Accepted names:
  - time: `timestamp`, `arrival`, `arrival_time`, `time`, `ts`, `t`, `request_time`
  - prompt: `prompt_tokens`, `input_tokens`, `context_tokens`, `prompt`, `num_prompt_tokens`, `request_tokens`
  - output: `output_tokens`, `generated_tokens`, `completion_tokens`, `output`, `num_output_tokens`, `response_tokens`, `num_decode_tokens`
  - optional: `model` (`model_name`, `model_id`), `tenant` (`tenant_id`, `user`, `user_id`, `customer`)
- **Timestamps** are RFC 3339 (`2026-01-01T09:00:00.120Z`),
  `2006-01-02 15:04:05.000` with or without a zone offset, or numbers in
  seconds. Numbers that look like Unix time are re-based on the first request;
  small numbers are taken as seconds from the start of the trace. Absolute and
  numeric timestamps cannot be mixed.
- Rows need not be sorted.
- Lines starting with `#` are comments. Two are meaningful:
  `# duration_seconds: N` sets the trace length when it is longer than the
  last arrival (a quiet tail still costs GPU time), and `# approximate: ...`
  marks a trace as synthesised from aggregates (see below).
- Requests with 0 output tokens are simulated as 1.

`model` and `tenant` are carried so that `trace stats` can count them and
`trace slice --model/--tenant` can filter on them. The simulator itself serves
one model and ignores both.

## JSONL

One JSON object per line:

```json
{"timestamp": "2026-01-01T09:00:00.120Z", "prompt_tokens": 512, "output_tokens": 180, "model": "chat-8b", "tenant": "acme"}
```

Keys: `timestamp` (or `arrival`, `time`); `prompt_tokens` (or `input_tokens`);
`output_tokens` (or `completion_tokens`); optional `model`, `tenant` (or
`user`). Other keys are ignored, so an OpenAI-style usage log usually works
after renaming the timestamp.

## Azure LLM inference traces

The public traces in
[Azure/AzurePublicDataset](https://github.com/Azure/AzurePublicDataset)
(`AzureLLMInferenceDataset2023.md`, `AzureLLMInferenceDataset2024.md`) are CSV
files with exactly three columns:

| Field | Description |
| --- | --- |
| `TIMESTAMP` | Invocation time |
| `ContextTokens` | Number of context tokens |
| `GeneratedTokens` | Number of generated tokens |

Both timestamp layouts are read: the 2023 files
(`2023-11-16 18:15:46.6805900`) and the 2024 one-week files
(`2024-05-10 00:00:00.009930+00:00`). A file with this header is detected
automatically. The datasets are licensed CC-BY by their authors; they are not
redistributed here, and `examples/traces/azure-format-sample.csv` contains ten
invented rows in the same layout.

The 2024 files cover a week and are large. Cut a window first:

```sh
scale-backtest trace slice AzureLLMInferenceTrace_conv_1week.csv --from 30h --to 36h -o window.csv
```

## Coarse mode: from Prometheus history

If you have metrics but no request log, `trace from-prometheus` synthesises a
trace from rate series:

```sh
# from saved query_range responses
scale-backtest trace from-prometheus -o approx.csv \
  --rate-json request-rate.json \
  --prompt-rate-json prompt-token-rate.json \
  --output-rate-json output-token-rate.json

# or by querying a server
scale-backtest trace from-prometheus -o approx.csv --url http://prometheus:9090 \
  --rate-query 'sum(rate(vllm:request_success_total[1m]))' \
  --output-rate-query 'sum(rate(vllm:generation_tokens_total[1m]))' \
  --prompt-rate-query 'sum(rate(vllm:prompt_tokens_total[1m]))' \
  --start 2026-01-05T00:00:00Z --end 2026-01-06T00:00:00Z --step 1m
```

Input is the JSON body of `/api/v1/query_range` (`resultType: matrix`); several
series in one result are summed. The request-rate series is in requests per
second, the token-rate series in tokens per second.

**How arrivals are made.** Each sample holds for the step that ends at its
timestamp. Inside that bucket, arrivals are a Poisson process at the sample's
rate. Token counts are log-normal with a mean of token rate / request rate for
the bucket (or `--prompt-mean` / `--output-mean` where there is no token
series) and a coefficient of variation of `--token-cv` (default 1.0).

**What is invented.** Everything below the bucket length: bursts inside a
bucket are flattened to Poisson, the real token-length distributions and the
correlation between prompt and output length are unknown, and a `rate()` over a
window longer than the step has already smoothed the series. Queue build-up and
tail latency are sensitive to exactly these things, so a coarse trace tends to
flatter a policy. The output file starts with `# approximate: ...`, and every
report on a run that uses it prints `APPROXIMATE TRACE`.

Use the smallest step and rate window your retention allows, and prefer a
request log whenever you can get one.

## Trace tools

```sh
scale-backtest trace stats FILE [--format json]
```

<!-- output: scale-backtest trace stats examples/traces/demo.csv -->
```text
trace demo (csv)
  requests        4,797 over 45m00s
  arrival rate    mean 1.78 req/s · peak 12.00 (1s) / 5.80 (10s) / 4.63 (60s) · peak-to-mean 2.61 (60s)
  burstiness      inter-arrival CV 1.65 · index of dispersion 11.94 (10s) / 65.16 (60s)   [Poisson: 1 / 1 / 1]
  prompt tokens   mean 601 · p50 422 · p90 1242 · p99 2912 · max 8000   (1067 tokens/s)
  output tokens   mean 396 · p50 305 · p90 779 · p99 1669 · max 2000   (704 tokens/s)
  rate            ▂▂▂▂▂▂▂▂▂▂▂▄▅▅▆▆▇▆▆▆▅▆▆▆▅▅▄▃▃▃▃▃▃▃▃▃▃▂▃▃▅███████▇▅▃▂▃▂▂▂▂▂▂▂  0..4.7 req/s
```

- *inter-arrival CV*: coefficient of variation of the gaps between arrivals;
  1 for a Poisson process.
- *index of dispersion*: variance / mean of arrival counts in 10 s and 60 s
  windows; 1 for a Poisson process, larger when load comes in waves.
- *peak rates* are the busiest aligned 1 s, 10 s and 60 s windows.

```sh
scale-backtest trace scale FILE -o OUT --time-compress 4      # same requests, 4x faster
scale-backtest trace scale FILE -o OUT --load 2.5             # 2.5x the requests, same duration
scale-backtest trace scale FILE -o OUT --tokens 1.5           # longer prompts and outputs
scale-backtest trace slice FILE -o OUT --from 2h --to 3h --model chat-8b
scale-backtest trace convert FILE -o OUT                      # any format to native CSV
scale-backtest trace synth -o OUT --profile "0:1,10m:1,12m:6,20m:6,22m:1" --seed 3
```

`--load` above 1 replicates requests (each copy shifted by up to `--jitter`,
default 1 s); below 1 it drops requests at random. Both are seeded. Replication
keeps bursts in place and makes them taller; it does not add new users with
their own timing. `run`, `compare` and `sweep` accept `--time-compress` and
`--load` directly.

`trace synth` generates a non-homogeneous Poisson process along a
piecewise-linear rate profile with log-normal token counts. The bundled demo
trace is made with it; see `examples/README.md`.
