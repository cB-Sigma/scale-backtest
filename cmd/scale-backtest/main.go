// Command scale-backtest replays a request trace through a Kubernetes HPA or
// KEDA autoscaling policy in simulation and reports latency-target attainment
// and GPU cost.
package main

import (
	"os"

	"github.com/cb-Sigma/scale-backtest/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
