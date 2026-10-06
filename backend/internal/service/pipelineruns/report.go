package pipelineruns

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Specialist outcomes beyond the generic succeeded/failed.
const (
	// OutcomeProductionDefect: the specialist found a defect in production code.
	// It is reported for return to Build, never fixed under specialist authority.
	OutcomeProductionDefect = "production_defect"

	findingMet           = "met"
	findingUnmet         = "unmet"
	findingNotApplicable = "not_applicable"
	findingUnverified    = "unverified"

	maxReportItems  = 50
	maxReportText   = 2000
	maxDefectPaths  = 50
	maxCommandBytes = 1000
)

// StageReport is the structured result a specialist submits with its outcome.
// An agent-authored report is a claim: it can never satisfy a gate that
// deterministic checks or scope validation refuse.
type StageReport struct {
	// Findings records, per acceptance criterion, what the specialist observed.
	Findings []ReportFinding `json:"findings"`
	// Commands lists what the specialist ran and how it ended.
	Commands []ReportCommand `json:"commands"`
	// RemainingIssues are known gaps that did not block the outcome.
	RemainingIssues []string `json:"remainingIssues"`
	// Defects are production-code problems; required for production_defect.
	Defects []ReportDefect `json:"defects"`
}

// ReportFinding is one acceptance-criterion observation.
type ReportFinding struct {
	Criterion string `json:"criterion"`
	Status    string `json:"status" enum:"met,unmet,not_applicable,unverified"`
	Evidence  string `json:"evidence,omitempty"`
}

// ReportCommand is one command the specialist ran.
type ReportCommand struct {
	Command  string `json:"command"`
	ExitCode int    `json:"exitCode"`
	Summary  string `json:"summary,omitempty"`
}

// ReportDefect is one production-code defect for Build to fix.
type ReportDefect struct {
	Description string   `json:"description"`
	Paths       []string `json:"paths,omitempty"`
}

// validate checks a report against the outcome it accompanies and returns a
// normalized copy with every slice non-nil.
func (r StageReport) validate(outcome string) (StageReport, error) {
	out := StageReport{Findings: []ReportFinding{}, Commands: []ReportCommand{}, RemainingIssues: []string{}, Defects: []ReportDefect{}}
	if len(r.Findings) > maxReportItems || len(r.Commands) > maxReportItems || len(r.RemainingIssues) > maxReportItems || len(r.Defects) > maxReportItems {
		return out, fmt.Errorf("a report may list at most %d findings, commands, remaining issues, and defects each", maxReportItems)
	}
	text := func(field, v string, limit int) (string, error) {
		v = strings.TrimSpace(v)
		if !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
			return "", fmt.Errorf("%s must be valid text", field)
		}
		if utf8.RuneCountInString(v) > limit {
			return "", fmt.Errorf("%s must be at most %d characters", field, limit)
		}
		return v, nil
	}
	unmet := 0
	for i, f := range r.Findings {
		c, err := text(fmt.Sprintf("findings[%d].criterion", i), f.Criterion, maxReportText)
		if err != nil {
			return out, err
		}
		if c == "" {
			return out, fmt.Errorf("findings[%d].criterion is required", i)
		}
		switch f.Status {
		case findingMet, findingUnmet, findingNotApplicable, findingUnverified:
		default:
			return out, fmt.Errorf("findings[%d].status must be met, unmet, not_applicable, or unverified", i)
		}
		if f.Status == findingUnmet {
			unmet++
		}
		e, err := text(fmt.Sprintf("findings[%d].evidence", i), f.Evidence, maxReportText)
		if err != nil {
			return out, err
		}
		out.Findings = append(out.Findings, ReportFinding{Criterion: c, Status: f.Status, Evidence: e})
	}
	for i, c := range r.Commands {
		cmd, err := text(fmt.Sprintf("commands[%d].command", i), c.Command, maxCommandBytes)
		if err != nil {
			return out, err
		}
		if cmd == "" {
			return out, fmt.Errorf("commands[%d].command is required", i)
		}
		s, err := text(fmt.Sprintf("commands[%d].summary", i), c.Summary, maxReportText)
		if err != nil {
			return out, err
		}
		out.Commands = append(out.Commands, ReportCommand{Command: cmd, ExitCode: c.ExitCode, Summary: s})
	}
	for i, issue := range r.RemainingIssues {
		v, err := text(fmt.Sprintf("remainingIssues[%d]", i), issue, maxReportText)
		if err != nil {
			return out, err
		}
		if v != "" {
			out.RemainingIssues = append(out.RemainingIssues, v)
		}
	}
	for i, d := range r.Defects {
		desc, err := text(fmt.Sprintf("defects[%d].description", i), d.Description, maxReportText)
		if err != nil {
			return out, err
		}
		if desc == "" {
			return out, fmt.Errorf("defects[%d].description is required", i)
		}
		if len(d.Paths) > maxDefectPaths {
			return out, fmt.Errorf("defects[%d].paths may list at most %d paths", i, maxDefectPaths)
		}
		paths := make([]string, 0, len(d.Paths))
		for j, p := range d.Paths {
			v, err := text(fmt.Sprintf("defects[%d].paths[%d]", i, j), p, 500)
			if err != nil {
				return out, err
			}
			paths = append(paths, v)
		}
		out.Defects = append(out.Defects, ReportDefect{Description: desc, Paths: paths})
	}
	switch outcome {
	case OutcomeSucceeded:
		if len(out.Findings) == 0 {
			return out, fmt.Errorf("a passing report needs at least one acceptance-criterion finding")
		}
		if unmet > 0 {
			return out, fmt.Errorf("a passing report cannot contain %d unmet finding(s); report a production defect or failure instead", unmet)
		}
		if len(out.Defects) > 0 {
			return out, fmt.Errorf("a passing report cannot list production defects")
		}
	case OutcomeProductionDefect:
		if len(out.Defects) == 0 {
			return out, fmt.Errorf("a production_defect report must describe at least one defect")
		}
	}
	return out, nil
}

func (r StageReport) marshal() string {
	data, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return string(data)
}

func parseReport(raw string) *StageReport {
	if raw == "" {
		return nil
	}
	var r StageReport
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil
	}
	return &r
}
