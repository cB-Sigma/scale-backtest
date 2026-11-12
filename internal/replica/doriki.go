package replica

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// DorikiReport is what scale-backtest takes from a doriki cold-start report
// (https://github.com/AmitwalaH/doriki, `doriki <pod> -o json`). The field
// names follow doriki's schema/report.v1.json: report_version,
// totals.pending_to_ready_seconds and phases[].{name,seconds,state}.
type DorikiReport struct {
	// PendingToReadySeconds is totals.pending_to_ready_seconds.
	PendingToReadySeconds float64
	// NodeProvisionSeconds is the node-provision phase, when it completed.
	NodeProvisionSeconds float64
	// StartupSeconds is PendingToReadySeconds minus NodeProvisionSeconds:
	// the part that maps to the replica model's coldStart. Node
	// provisioning is modelled separately (node.provisionDelay), so it is
	// taken out to avoid counting it twice.
	StartupSeconds float64
	// Phases lists the completed phases in report order.
	Phases []DorikiPhase
}

// DorikiPhase is one completed phase of a doriki report.
type DorikiPhase struct {
	Name    string
	Seconds float64
}

type dorikiFile struct {
	ReportVersion int `json:"report_version"`
	Phases        []struct {
		Name    string   `json:"name"`
		Seconds *float64 `json:"seconds"`
		State   string   `json:"state"`
	} `json:"phases"`
	Totals struct {
		PendingToReadySeconds *float64 `json:"pending_to_ready_seconds"`
	} `json:"totals"`
}

// ParseDorikiReport parses a doriki JSON report.
func ParseDorikiReport(data []byte) (*DorikiReport, error) {
	var f dorikiFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("not a doriki JSON report: %w", err)
	}
	if f.ReportVersion != 1 {
		return nil, fmt.Errorf("doriki report_version %d is not supported (expected 1)", f.ReportVersion)
	}
	if f.Totals.PendingToReadySeconds == nil {
		return nil, errors.New("the doriki report has no totals.pending_to_ready_seconds: the pod was not Ready when it was profiled")
	}
	r := &DorikiReport{PendingToReadySeconds: *f.Totals.PendingToReadySeconds}
	for _, p := range f.Phases {
		if p.State != "complete" || p.Seconds == nil {
			continue
		}
		r.Phases = append(r.Phases, DorikiPhase{Name: p.Name, Seconds: *p.Seconds})
		if p.Name == "node-provision" {
			r.NodeProvisionSeconds = *p.Seconds
		}
	}
	r.StartupSeconds = r.PendingToReadySeconds - r.NodeProvisionSeconds
	if r.StartupSeconds < 0 {
		return nil, errors.New("the doriki report is inconsistent: node-provision is longer than the time to Ready")
	}
	return r, nil
}

// LoadDorikiReport reads a doriki JSON report from a file.
func LoadDorikiReport(path string) (*DorikiReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r, err := ParseDorikiReport(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}
