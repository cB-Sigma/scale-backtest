// Package scalebacktest embeds the bundled examples so that the
// `scale-backtest --demo` commands work from the binary alone.
//
// The command lives in cmd/scale-backtest; the simulator is in internal/.
package scalebacktest

import "embed"

// Examples holds the examples/ directory: sample policies, replica models,
// a small synthetic trace and sample benchmark files.
//
//go:embed examples
var Examples embed.FS
