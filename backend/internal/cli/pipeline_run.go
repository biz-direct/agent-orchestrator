package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// These DTOs mirror the daemon's pipeline run API without importing service types.
type pipelineRunEnvelopeDTO struct {
	Run *pipelineRunDTO `json:"run"`
}

type pipelineRunDTO struct {
	ID               string                   `json:"id"`
	WorkflowID       string                   `json:"workflowId"`
	State            string                   `json:"state"`
	PauseReason      string                   `json:"pauseReason,omitempty"`
	PauseDetail      string                   `json:"pauseDetail,omitempty"`
	CurrentStageID   string                   `json:"currentStageId,omitempty"`
	RequestedBy      string                   `json:"requestedBy"`
	ExpectedBranch   string                   `json:"expectedBranch,omitempty"`
	RepairBudget     int                      `json:"repairBudget"`
	RepairsUsed      int                      `json:"repairsUsed"`
	RepairsRemaining int                      `json:"repairsRemaining"`
	Stages           []pipelineStageViewDTO   `json:"stages"`
	Attempts         []pipelineAttemptViewDTO `json:"attempts"`
	Checkpoint       *pipelineCheckpointDTO   `json:"checkpoint,omitempty"`
	LastRejection    *pipelineRejectionDTO    `json:"lastRejection,omitempty"`
}

type pipelineStageViewDTO struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	State          string `json:"state"`
	Harness        string `json:"harness,omitempty"`
	Model          string `json:"model,omitempty"`
	SettingsSource string `json:"settingsSource"`
}

type pipelineAttemptViewDTO struct {
	ID                   string             `json:"id"`
	StageID              string             `json:"stageId"`
	AttemptNo            int                `json:"attemptNo"`
	State                string             `json:"state"`
	ControllerGeneration string             `json:"controllerGeneration,omitempty"`
	InputCommit          string             `json:"inputCommit,omitempty"`
	OutputCommit         string             `json:"outputCommit,omitempty"`
	Validation           []pipelineCheckDTO `json:"validation"`
}

type pipelineCheckDTO struct {
	Round     int    `json:"round"`
	CommandID string `json:"commandId"`
	Required  bool   `json:"required"`
	Revision  string `json:"revision"`
	Status    string `json:"status"`
	ExitCode  int    `json:"exitCode"`
	Detail    string `json:"detail,omitempty"`
}

type pipelineCheckpointDTO struct {
	StageID      string `json:"stageId"`
	InputCommit  string `json:"inputCommit"`
	OutputCommit string `json:"outputCommit"`
	NoChange     bool   `json:"noChange"`
}

type pipelineRejectionDTO struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Paths   []string `json:"paths,omitempty"`
}

type pipelineStageOverrideDTO struct {
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
}

type pipelineStartRequestDTO struct {
	WorkflowID  string                              `json:"workflowId"`
	RequestedBy string                              `json:"requestedBy"`
	Overrides   map[string]pipelineStageOverrideDTO `json:"overrides,omitempty"`
}

type pipelineSubmitRequestDTO struct {
	RunID                string `json:"runId"`
	AttemptID            string `json:"attemptId"`
	ControllerGeneration string `json:"controllerGeneration"`
	IdempotencyKey       string `json:"idempotencyKey"`
	Outcome              string `json:"outcome"`
	ExpectedInputCommit  string `json:"expectedInputCommit"`
	OutputCommit         string `json:"outputCommit,omitempty"`
	Summary              string `json:"summary,omitempty"`
	// Report is the specialist's structured result, passed through verbatim so
	// the daemon validates it against its own contract.
	Report json.RawMessage `json:"report,omitempty"`
}

type pipelineSubmitResultDTO struct {
	Run      pipelineRunDTO         `json:"run"`
	Attempt  pipelineAttemptViewDTO `json:"attempt"`
	Accepted bool                   `json:"accepted"`
	Replayed bool                   `json:"replayed"`
}

// pipelineRequester reports who is calling. A command running inside an AO
// session (AO_SESSION_ID set) is an orchestrator or worker acting for a user;
// only a person at a shell may supply stage harness/model overrides.
func pipelineRequester() string {
	if strings.TrimSpace(os.Getenv("AO_SESSION_ID")) != "" {
		return "orchestrator"
	}
	return "user"
}

func pipelineSessionID(flag string) (string, error) {
	id := strings.TrimSpace(flag)
	if id == "" {
		id = strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
	}
	if id == "" {
		return "", usageError{errors.New("session id is required (pass --session or set AO_SESSION_ID)")}
	}
	return id, nil
}

func newPipelineStartCommand(ctx *commandContext) *cobra.Command {
	var session, stage, harness, model string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "start <workflow-id>",
		Short: "Start a pipeline run on an existing worker task",
		Long: "Start a snapshotted pipeline run on a worker task. Selection is explicit: " +
			"AO never falls back to a normal worker when the workflow cannot run. Only a user at a shell " +
			"may override a stage's harness or model; inside an AO session the workflow defaults apply.",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.ExactArgs(1)(cmd, args); err != nil {
				return usageError{err}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := pipelineSessionID(session)
			if err != nil {
				return err
			}
			req := pipelineStartRequestDTO{WorkflowID: strings.TrimSpace(args[0]), RequestedBy: pipelineRequester()}
			if harness != "" || model != "" {
				if stage == "" {
					return usageError{errors.New("--harness and --model need --override-stage to name the stage they apply to")}
				}
				if req.RequestedBy != "user" {
					return usageError{errors.New("stage harness/model overrides are user-only and cannot be set from inside an AO session")}
				}
				req.Overrides = map[string]pipelineStageOverrideDTO{stage: {Harness: harness, Model: model}}
			} else if stage != "" {
				return usageError{errors.New("--override-stage needs --harness or --model")}
			}
			var res pipelineRunEnvelopeDTO
			if err := ctx.postJSON(cmd.Context(), "sessions/"+url.PathEscape(id)+"/pipeline", req, &res); err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			return writePipelineRun(cmd.OutOrStdout(), res.Run)
		},
	}
	f := cmd.Flags()
	f.StringVar(&session, "session", "", "Worker session id (default: AO_SESSION_ID)")
	f.StringVar(&stage, "override-stage", "", "Stage whose harness/model to override (user only)")
	f.StringVar(&harness, "harness", "", "Harness override for --override-stage (user only)")
	f.StringVar(&model, "model", "", "Model override for --override-stage (user only)")
	f.BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}

func newPipelineStatusCommand(ctx *commandContext) *cobra.Command {
	var session string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show a task's pipeline run: stages, attempts, checkpoint, and any rejection",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := pipelineSessionID(session)
			if err != nil {
				return err
			}
			var res pipelineRunEnvelopeDTO
			if err := ctx.getJSON(cmd.Context(), "sessions/"+url.PathEscape(id)+"/pipeline", &res); err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			return writePipelineRun(cmd.OutOrStdout(), res.Run)
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "Worker session id (default: AO_SESSION_ID)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}

func newPipelineSubmitCommand(ctx *commandContext) *cobra.Command {
	var session, outcome, summary, reportFile string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "submit",
		Short: "Submit the active stage's result from a clean, committed worktree",
		Long: "Report the active stage's outcome. AO verifies the claim itself: the working tree must be " +
			"clean on the task branch and HEAD must be the commit you are reporting. `ao report` does not " +
			"complete a stage; only this verified submission does.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if outcome != "succeeded" && outcome != "failed" && outcome != "production_defect" {
				return usageError{errors.New(`--outcome must be "succeeded", "failed", or "production_defect"`)}
			}
			var report json.RawMessage
			if reportFile != "" {
				raw, err := readReport(cmd, reportFile)
				if err != nil {
					return err
				}
				report = raw
			}
			id, err := pipelineSessionID(session)
			if err != nil {
				return err
			}
			var current pipelineRunEnvelopeDTO
			if err := ctx.getJSON(cmd.Context(), "sessions/"+url.PathEscape(id)+"/pipeline", &current); err != nil {
				return err
			}
			if current.Run == nil || current.Run.State != "running" {
				return errors.New("this task has no running pipeline stage to submit")
			}
			var attempt *pipelineAttemptViewDTO
			for i := range current.Run.Attempts {
				if current.Run.Attempts[i].State == "active" {
					attempt = &current.Run.Attempts[i]
				}
			}
			if attempt == nil {
				return errors.New("the pipeline run has no active stage attempt")
			}
			output := ""
			if outcome != "failed" {
				out, gitErr := exec.CommandContext(cmd.Context(), "git", "rev-parse", "HEAD").Output()
				if gitErr != nil {
					return fmt.Errorf("could not read HEAD of the current worktree: %w", gitErr)
				}
				output = strings.TrimSpace(string(out))
			}
			sum := sha256.Sum256([]byte(strings.Join([]string{attempt.ID, outcome, output, summary, string(report)}, "\x00")))
			req := pipelineSubmitRequestDTO{
				RunID: current.Run.ID, AttemptID: attempt.ID, ControllerGeneration: attempt.ControllerGeneration,
				IdempotencyKey: hex.EncodeToString(sum[:16]), Outcome: outcome,
				ExpectedInputCommit: attempt.InputCommit, OutputCommit: output, Summary: summary, Report: report,
			}
			var res pipelineSubmitResultDTO
			if err := ctx.postJSON(cmd.Context(), "sessions/"+url.PathEscape(id)+"/pipeline/results", req, &res); err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			verb := "recorded"
			switch {
			case res.Replayed:
				verb = "already recorded"
			case res.Accepted:
				verb = "accepted"
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "stage %s %s (%s)\n", res.Attempt.StageID, verb, res.Run.State); err != nil {
				return err
			}
			return writePipelineRun(cmd.OutOrStdout(), &res.Run)
		},
	}
	f := cmd.Flags()
	f.StringVar(&session, "session", "", "Worker session id (default: AO_SESSION_ID)")
	f.StringVar(&outcome, "outcome", "", `Stage outcome: "succeeded" or "failed"`)
	f.StringVar(&summary, "summary", "", "Short description of what the stage did or why it failed")
	f.StringVar(&reportFile, "report-file", "", `Structured specialist report as JSON ("-" reads stdin): findings, commands, remainingIssues, defects`)
	f.BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}

func writePipelineRun(w io.Writer, run *pipelineRunDTO) error {
	if run == nil {
		_, err := fmt.Fprintln(w, "no pipeline run: this task is an ordinary worker")
		return err
	}
	status := run.State
	if run.PauseReason != "" {
		status += " (" + run.PauseReason + ")"
	}
	if _, err := fmt.Fprintf(w, "run %s  workflow %s  %s  branch %s\n", run.ID, run.WorkflowID, status, run.ExpectedBranch); err != nil {
		return err
	}
	if run.PauseDetail != "" {
		if _, err := fmt.Fprintf(w, "paused: %s\n", run.PauseDetail); err != nil {
			return err
		}
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "STAGE\tKIND\tSTATE\tHARNESS\tMODEL")
	for _, st := range run.Stages {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", st.ID, st.Kind, st.State, st.Harness, st.Model)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if run.Checkpoint != nil {
		change := run.Checkpoint.OutputCommit
		if run.Checkpoint.NoChange {
			change += " (no new commits)"
		}
		if _, err := fmt.Fprintf(w, "checkpoint: %s -> %s\n", shortHash(run.Checkpoint.InputCommit), change); err != nil {
			return err
		}
	}
	for _, a := range run.Attempts {
		if len(a.Validation) == 0 {
			continue
		}
		round := 0
		for _, c := range a.Validation {
			round = max(round, c.Round)
		}
		if _, err := fmt.Fprintf(w, "validation for stage %s (round %d):\n", a.StageID, round); err != nil {
			return err
		}
		for _, c := range a.Validation {
			if c.Round != round {
				continue
			}
			if _, err := fmt.Fprintf(w, "  %-12s %-11s exit=%d rev=%s %s\n", c.CommandID, c.Status, c.ExitCode, shortHash(c.Revision), c.Detail); err != nil {
				return err
			}
		}
	}
	if r := run.LastRejection; r != nil {
		if _, err := fmt.Fprintf(w, "last submission rejected [%s]: %s\n", r.Code, r.Message); err != nil {
			return err
		}
		for _, p := range r.Paths {
			if _, err := fmt.Fprintf(w, "  %s\n", p); err != nil {
				return err
			}
		}
	}
	return nil
}

func shortHash(h string) string {
	if len(h) > 10 {
		return h[:10]
	}
	return h
}

// readReport loads a specialist report from a file or stdin and checks it is a
// JSON object; the daemon owns the schema and validates the contents.
func readReport(cmd *cobra.Command, source string) (json.RawMessage, error) {
	var (
		raw []byte
		err error
	)
	if source == "-" {
		raw, err = io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
	} else {
		raw, err = os.ReadFile(source) //nolint:gosec // the caller names their own report file
	}
	if err != nil {
		return nil, usageError{fmt.Errorf("could not read the report: %w", err)}
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, usageError{fmt.Errorf("--report-file must contain a JSON object: %w", err)}
	}
	return json.RawMessage(raw), nil
}
