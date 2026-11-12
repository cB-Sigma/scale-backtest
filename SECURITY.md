# Security policy

## Supported versions

Security fixes are made on the latest release.

## Reporting a vulnerability

Please do not open a public issue. Use GitHub's private reporting:
<https://github.com/cb-Sigma/scale-backtest/security/advisories/new>.

Include what you found, how to reproduce it and the version
(`scale-backtest version`). You should get an acknowledgement within a week.
Once a fix is released the advisory is published with credit, unless you ask
otherwise.

## Scope

scale-backtest is an offline command-line tool. It reads local files (policy
manifests, replica models, traces, benchmark and profiler reports) and writes
reports. It makes a network request only when
`scale-backtest trace from-prometheus --url ...` is used, to the URL given, and
sends no credentials. It does not connect to a Kubernetes cluster.

Relevant reports include: a crafted input file that makes the tool execute
code, read or write files other than those named on the command line, or use
unbounded memory or time on a small input.

Traces can contain tenant names and reveal traffic patterns. Treat them, and
reports derived from them, as you treat the logs they came from.
