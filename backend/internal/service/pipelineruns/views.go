package pipelineruns

import (
	"encoding/json"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/pipeline"
)

// Stage display states are derived from attempts and the run; they are never
// stored.
const (
	StageStatePending     = "pending"
	StageStateActive      = "active"
	StageStateAccepted    = "accepted"
	StageStateFailed      = "failed"
	StageStateInterrupted = "interrupted"
	StageStatePaused      = "paused"
	StageStateHandoff     = "handoff"
	StageStateValidating  = "validating"
)

// RunView is the API read model of one run.
type RunView struct {
	ID               string        `json:"id"`
	SessionID        string        `json:"sessionId"`
	ProjectID        string        `json:"projectId"`
	WorkflowID       string        `json:"workflowId"`
	State            string        `json:"state" enum:"running,paused,completed,cancelled"`
	PauseReason      string        `json:"pauseReason,omitempty"`
	PauseDetail      string        `json:"pauseDetail,omitempty"`
	CurrentStageID   string        `json:"currentStageId,omitempty"`
	RequestedBy      string        `json:"requestedBy" enum:"user,orchestrator"`
	ExpectedBranch   string        `json:"expectedBranch,omitempty"`
	RepairBudget     int           `json:"repairBudget"`
	RepairsUsed      int           `json:"repairsUsed"`
	RepairsRemaining int           `json:"repairsRemaining"`
	SnapshotSHA256   string        `json:"snapshotSha256"`
	SnapshotCaptured string        `json:"snapshotCapturedAt"`
	Stages           []StageView   `json:"stages"`
	Attempts         []AttemptView `json:"attempts"`
	// Checkpoint is the newest accepted committed revision, if any.
	Checkpoint *CheckpointView `json:"checkpoint,omitempty"`
	// LastRejection explains the newest refused stage submission that is still
	// relevant to the active attempt.
	LastRejection *RejectionView `json:"lastRejection,omitempty"`
	// Evidence lists specialist results with the exact revision each covers. A
	// result never speaks for later revisions, so it is never shown as
	// validation of a newer head.
	Evidence []EvidenceView `json:"evidence"`
	// Repairs are the counted automatic returns to Build, in order. They share
	// one budget across every source of repair.
	Repairs []RepairView `json:"repairs"`
	// ReviewGate is the live readiness of the Review stage for the current
	// head: what AO is waiting for, or what a person has to do. It is present
	// only while the run is at a Review stage.
	ReviewGate *ReviewGateView `json:"reviewGate,omitempty"`
	// Reviews records, per Review attempt, the pull request head and AO review
	// run it was bound to and how it ended.
	Reviews []ReviewEvidenceView `json:"reviews"`
	// Control says which run controls apply now and why; RepairGrants lists the
	// human authorizations of extra repairs (resume never creates one).
	Control      ControlView       `json:"control"`
	RepairGrants []RepairGrantView `json:"repairGrants"`
	Events       []EventView       `json:"events"`
	Revision     int64             `json:"revision"`
	CreatedAt    time.Time         `json:"createdAt"`
	UpdatedAt    time.Time         `json:"updatedAt"`
	CompletedAt  *time.Time        `json:"completedAt,omitempty"`
}

// StageView is one workflow stage with its derived state and resolved settings.
type StageView struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	Profile        string `json:"profile,omitempty"`
	RepairTo       string `json:"repairTo,omitempty"`
	Harness        string `json:"harness,omitempty"`
	Model          string `json:"model,omitempty"`
	SettingsSource string `json:"settingsSource"`
	// AllowedPaths is the change scope snapshotted for a specialist stage. It is
	// a hand-off constraint checked on commits, not a filesystem or process sandbox.
	AllowedPaths []string `json:"allowedPaths,omitempty"`
	State        string   `json:"state" enum:"pending,handoff,active,validating,accepted,failed,interrupted,paused"`
}

// AttemptView is one stage attempt.
type AttemptView struct {
	ID                   string `json:"id"`
	StageID              string `json:"stageId"`
	AttemptNo            int    `json:"attemptNo"`
	State                string `json:"state" enum:"active,accepted,failed,interrupted,cancelled"`
	ExecutorSessionID    string `json:"executorSessionId"`
	ControllerGeneration string `json:"controllerGeneration,omitempty"`
	InputCommit          string `json:"inputCommit,omitempty"`
	OutputCommit         string `json:"outputCommit,omitempty"`
	NoChange             bool   `json:"noChange"`
	Outcome              string `json:"outcome,omitempty"`
	Summary              string `json:"summary,omitempty"`
	InstructionDelivery  string `json:"instructionDelivery" enum:"pending,delivered,failed"`
	// PredecessorAttemptID names the attempt whose output is this attempt's input.
	PredecessorAttemptID string `json:"predecessorAttemptId,omitempty"`
	// ConversationSessionID is the attached specialist conversation that
	// executes (or executed) this attempt; empty for the worker's own stage.
	ConversationSessionID string `json:"conversationSessionId,omitempty"`
	// Report is the specialist's structured result, retained even when the
	// attempt did not advance the run.
	Report *StageReport `json:"report,omitempty"`
	// Validation is AO's own execution of the stage's checks, bound to the
	// revision each ran against. It is independent of anything the agent said.
	Validation []CommandResultView `json:"validation"`
	// RepairSourceAttemptID names the failed attempt that sent this Build
	// attempt back; Feedback is the revision-bound explanation it received.
	RepairSourceAttemptID string     `json:"repairSourceAttemptId,omitempty"`
	Feedback              *Feedback  `json:"feedback,omitempty"`
	StartedAt             time.Time  `json:"startedAt"`
	FinishedAt            *time.Time `json:"finishedAt,omitempty"`
}

// CommandResultView is the evidence of one independently executed command.
type CommandResultView struct {
	Round     int    `json:"round"`
	CommandID string `json:"commandId"`
	Command   string `json:"command"`
	Required  bool   `json:"required"`
	// Revision is the commit the command ran against.
	Revision   string     `json:"revision"`
	Status     string     `json:"status" enum:"running,passed,failed,operational,timeout,cancelled,skipped,unknown"`
	ExitCode   int        `json:"exitCode"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	DurationMs int64      `json:"durationMs"`
	// Log is the bounded, sanitized output (head and tail of a long log).
	Log          string `json:"log,omitempty"`
	LogTruncated bool   `json:"logTruncated"`
	Detail       string `json:"detail,omitempty"`
}

// CheckpointView names a committed revision recorded for the run.
type CheckpointView struct {
	StageID      string `json:"stageId"`
	InputCommit  string `json:"inputCommit"`
	OutputCommit string `json:"outputCommit"`
	NoChange     bool   `json:"noChange"`
}

// RejectionView explains why a submission was refused.
type RejectionView struct {
	Code    string    `json:"code"`
	Message string    `json:"message"`
	Paths   []string  `json:"paths,omitempty"`
	At      time.Time `json:"at"`
}

// EvidenceView is one specialist result bound to the revision it covers.
type EvidenceView struct {
	StageID   string `json:"stageId"`
	AttemptID string `json:"attemptId"`
	Profile   string `json:"profile,omitempty"`
	// Revision is the commit the result is about (the stage's output commit).
	Revision string `json:"revision"`
	Outcome  string `json:"outcome"`
	Findings int    `json:"findings"`
	Commands int    `json:"commands"`
	Defects  int    `json:"defects"`
	Issues   int    `json:"remainingIssues"`
	// Current is true while the result still covers the latest checkpoint. Once
	// Build produces a newer commit it is false: the result is retained, but it
	// no longer satisfies anything.
	Current bool `json:"current"`
}

// ReviewGateView is the current-head readiness of a Review stage.
type ReviewGateView struct {
	State   string `json:"state" enum:"waiting,ready,blocked"`
	Code    string `json:"code"`
	Message string `json:"message"`
	// Manual is true while the run waits for a person to trigger the review
	// because Auto review is off.
	Manual     bool `json:"manual"`
	AutoReview bool `json:"autoReview"`
	// Checkpoint is the revision the pipeline is reviewing; HeadSHA is the
	// pull request's head as AO last observed it. They must match.
	Checkpoint  string `json:"checkpoint"`
	HeadSHA     string `json:"headSha,omitempty"`
	PRURL       string `json:"prUrl,omitempty"`
	PRNumber    int    `json:"prNumber,omitempty"`
	ReviewRunID string `json:"reviewRunId,omitempty"`
	// ReviewStatus and Verdict describe the review pass for this exact head.
	ReviewStatus string `json:"reviewStatus,omitempty"`
	Verdict      string `json:"verdict,omitempty"`
	// CI is AO's judgement of required CI for this head; CIDetail explains it.
	CI       string `json:"ci,omitempty" enum:"passing,no_checks,non_required_failing,pending,failing,unknown"`
	CIDetail string `json:"ciDetail,omitempty"`
}

// ReviewEvidenceView is one Review attempt bound to the head it evaluated.
type ReviewEvidenceView struct {
	StageID     string `json:"stageId"`
	AttemptID   string `json:"attemptId"`
	Revision    string `json:"revision"`
	PRURL       string `json:"prUrl,omitempty"`
	HeadSHA     string `json:"headSha,omitempty"`
	ReviewRunID string `json:"reviewRunId,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
	Summary     string `json:"summary,omitempty"`
	// Current is true while the evaluated revision is still the run's latest
	// accepted checkpoint.
	Current bool `json:"current"`
}

// ControlView reports which controls make sense right now.
type ControlView struct {
	CanPause  bool `json:"canPause"`
	CanResume bool `json:"canResume"`
	CanCancel bool `json:"canCancel"`
	// ResumeNeedsUser is true when the current pause is a decision only a
	// person may make; an orchestrator's resume is refused.
	ResumeNeedsUser bool `json:"resumeNeedsUser"`
	// NeedsRepairAuthorization is true when the repair budget is spent: resume
	// will not grant attempts, and a person must authorize more first.
	NeedsRepairAuthorization bool `json:"needsRepairAuthorization"`
	// LastStop is what AO knows about the last executor it asked to stop.
	LastStop *StopView `json:"lastStop,omitempty"`
	// RecoveryOptions lists the choices a person has for a recovery decision.
	RecoveryOptions []string `json:"recoveryOptions"`
	// LastRecovery is what AO concluded the last time it reconciled this run
	// after a restart, so recovery shows up in the task's own stage view.
	LastRecovery *RecoveryView `json:"lastRecovery,omitempty"`
}

// RecoveryView is one reconciliation outcome.
type RecoveryView struct {
	Outcome string    `json:"outcome" enum:"continued,retrying,paused"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// RepairGrantView is one persisted human authorization of extra repairs.
type RepairGrantView struct {
	Amount       int       `json:"amount"`
	AuthorizedBy string    `json:"authorizedBy"`
	Note         string    `json:"note,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

// RepairView is one counted return to Build.
type RepairView struct {
	Ordinal         int       `json:"ordinal"`
	Kind            string    `json:"kind" enum:"production_defect,validation_failed,review_feedback"`
	SourceStageID   string    `json:"sourceStageId"`
	SourceAttemptID string    `json:"sourceAttemptId"`
	TargetStageID   string    `json:"targetStageId"`
	ReturnStageID   string    `json:"returnStageId"`
	CreatedAt       time.Time `json:"createdAt"`
}

// EventView is one recorded execution fact.
type EventView struct {
	Kind      string    `json:"kind"`
	AttemptID string    `json:"attemptId,omitempty"`
	Message   string    `json:"message,omitempty"`
	At        time.Time `json:"at"`
}

// RunEnvelope is the body of GET /sessions/{id}/pipeline.
type RunEnvelope struct {
	Run *RunView `json:"run"`
	// Intent is the pipeline selected when the task was created, with what
	// became of it (pending, started, skipped, or failed and why).
	Intent *IntentView `json:"intent,omitempty"`
}

// eventDetail is the JSON stored in pipeline_events.detail.
type eventDetail struct {
	Code    string   `json:"code,omitempty"`
	Message string   `json:"message,omitempty"`
	Paths   []string `json:"paths,omitempty"`
}

func (d eventDetail) marshal() string {
	data, err := json.Marshal(d)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func parseEventDetail(raw string) eventDetail {
	var d eventDetail
	_ = json.Unmarshal([]byte(raw), &d)
	return d
}

// buildRunView derives the presentation from durable facts.
func buildRunView(run domain.PipelineRun, snap Snapshot, attempts []domain.PipelineStageAttempt, events []domain.PipelineEvent, commands map[string][]domain.PipelineCommandResult, repairs []domain.PipelineRepair, links []domain.PipelineReviewLink) RunView {
	v := RunView{
		ID: run.ID, SessionID: string(run.SessionID), ProjectID: string(run.ProjectID), WorkflowID: run.WorkflowID,
		State: string(run.State), PauseReason: string(run.PauseReason), PauseDetail: run.PauseDetail,
		CurrentStageID: run.CurrentStageID, RequestedBy: string(run.RequestedBy), ExpectedBranch: run.ExpectedBranch,
		RepairBudget: run.RepairBudget, RepairsUsed: run.RepairsUsed, RepairsRemaining: max(run.RepairBudget-run.RepairsUsed, 0),
		SnapshotSHA256: run.SnapshotSHA256, SnapshotCaptured: snap.CapturedAt.Format(time.RFC3339),
		Stages: []StageView{}, Attempts: []AttemptView{}, Evidence: []EvidenceView{}, Repairs: []RepairView{}, Reviews: []ReviewEvidenceView{}, RepairGrants: []RepairGrantView{}, Events: []EventView{},
		Revision: run.Revision, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt, CompletedAt: run.CompletedAt,
	}
	latestByStage := map[string]domain.PipelineStageAttempt{}
	var activeAttempt *domain.PipelineStageAttempt
	for i, a := range attempts {
		latestByStage[a.StageID] = a
		v.Attempts = append(v.Attempts, AttemptView{
			ID: a.ID, StageID: a.StageID, AttemptNo: a.AttemptNo, State: string(a.State),
			ExecutorSessionID: string(a.ExecutorSessionID), ControllerGeneration: a.ControllerGeneration,
			InputCommit: a.InputCommit, OutputCommit: a.OutputCommit, NoChange: a.NoChange, Outcome: a.Outcome,
			Summary: a.Summary, InstructionDelivery: a.InstructionDelivery, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt,
			PredecessorAttemptID: a.PredecessorAttemptID, ConversationSessionID: attachedConversation(run, a),
			Report: parseReport(a.ResultJSON), Validation: commandViews(commands[a.ID]),
			RepairSourceAttemptID: a.RepairSourceAttemptID, Feedback: parseFeedback(a.FeedbackJSON),
		})
		if rep := parseReport(a.ResultJSON); rep != nil && a.OutputCommit != "" {
			profile := ""
			if st, _, ok := snap.stage(a.StageID); ok {
				profile = st.Profile
			}
			v.Evidence = append(v.Evidence, EvidenceView{
				StageID: a.StageID, AttemptID: a.ID, Profile: profile, Revision: a.OutputCommit, Outcome: a.Outcome,
				Findings: len(rep.Findings), Commands: len(rep.Commands), Defects: len(rep.Defects), Issues: len(rep.RemainingIssues),
			})
		}
		if a.State == domain.PipelineAttemptActive || a.State == domain.PipelineAttemptHandoff || a.State == domain.PipelineAttemptValidating {
			activeAttempt = &attempts[i]
		}
		if a.State == domain.PipelineAttemptAccepted {
			v.Checkpoint = &CheckpointView{StageID: a.StageID, InputCommit: a.InputCommit, OutputCommit: a.OutputCommit, NoChange: a.NoChange}
		}
	}
	for i := range v.Evidence {
		v.Evidence[i].Current = v.Checkpoint != nil && v.Evidence[i].Revision == v.Checkpoint.OutputCommit
	}
	v.Reviews = reviewEvidence(attempts, links, v.Checkpoint)
	for _, r := range repairs {
		v.Repairs = append(v.Repairs, RepairView{Ordinal: r.Ordinal, Kind: string(r.Kind), SourceStageID: r.SourceStageID, SourceAttemptID: r.SourceAttemptID, TargetStageID: r.TargetStageID, ReturnStageID: r.ReturnStageID, CreatedAt: r.CreatedAt})
	}
	for _, st := range snap.Stages {
		sv := StageView{ID: st.ID, Kind: string(st.Kind), Profile: st.Profile, RepairTo: st.RepairTo, Harness: st.Harness, Model: st.Model, SettingsSource: st.SettingsSource, State: StageStatePending}
		for _, p := range snap.Profiles {
			if p.ID == st.Profile && st.Kind == pipeline.StageSpecialist {
				sv.AllowedPaths = append([]string{}, p.AllowedPaths...)
			}
		}
		if a, ok := latestByStage[st.ID]; ok {
			switch a.State {
			case domain.PipelineAttemptAccepted:
				sv.State = StageStateAccepted
			case domain.PipelineAttemptFailed:
				sv.State = StageStateFailed
			case domain.PipelineAttemptInterrupted, domain.PipelineAttemptCancelled:
				sv.State = StageStateInterrupted
			case domain.PipelineAttemptHandoff:
				sv.State = StageStateHandoff
			case domain.PipelineAttemptValidating:
				sv.State = StageStateValidating
			case domain.PipelineAttemptActive:
				sv.State = StageStateActive
				if run.State == domain.PipelineRunPaused {
					sv.State = StageStatePaused
				}
			}
		}
		v.Stages = append(v.Stages, sv)
	}
	for _, e := range events { // newest first
		d := parseEventDetail(e.Detail)
		v.Events = append(v.Events, EventView{Kind: e.Kind, AttemptID: e.AttemptID, Message: d.Message, At: e.CreatedAt})
		if v.LastRejection == nil && e.Kind == eventSubmissionRejected && activeAttempt != nil && e.AttemptID == activeAttempt.ID {
			v.LastRejection = &RejectionView{Code: d.Code, Message: d.Message, Paths: d.Paths, At: e.CreatedAt}
		}
	}
	return v
}

func attachedConversation(run domain.PipelineRun, a domain.PipelineStageAttempt) string {
	if a.ExecutorSessionID != "" && a.ExecutorSessionID != run.SessionID {
		return string(a.ExecutorSessionID)
	}
	return ""
}

func commandViews(results []domain.PipelineCommandResult) []CommandResultView {
	out := make([]CommandResultView, 0, len(results))
	for _, r := range results {
		v := CommandResultView{
			Round: r.Round, CommandID: r.CommandID, Command: r.Command, Required: r.Required, Revision: r.Revision,
			Status: string(r.Status), ExitCode: r.ExitCode, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
			Log: r.Log, LogTruncated: r.LogTruncated, Detail: r.Detail,
		}
		if r.FinishedAt != nil {
			v.DurationMs = r.FinishedAt.Sub(r.StartedAt).Milliseconds()
		}
		out = append(out, v)
	}
	return out
}

// buildControlView derives which controls apply from the run and its events.
func buildControlView(run domain.PipelineRun, events []domain.PipelineEvent) ControlView {
	c := ControlView{
		CanPause:        run.State == domain.PipelineRunRunning,
		CanCancel:       run.State.Unfinished(),
		RecoveryOptions: []string{},
	}
	if run.State == domain.PipelineRunPaused {
		exhausted := run.PauseReason == PauseRepairBudgetExhausted && run.RepairsUsed >= run.RepairBudget
		c.NeedsRepairAuthorization = exhausted
		c.CanResume = !exhausted && run.PauseReason != domain.PipelinePauseSessionTerminated
		c.ResumeNeedsUser = humanOnlyPause(run.PauseReason)
		if run.PauseReason == PauseRecoveryDecision {
			c.RecoveryOptions = append(c.RecoveryOptions, RecoveryRestoreConversation)
		}
	}
	for _, e := range events { // newest first
		d := parseEventDetail(e.Detail)
		switch {
		case e.Kind == eventExecutionStop && c.LastStop == nil:
			c.LastStop = &StopView{Requested: true, Confirmed: d.Code == "confirmed", Detail: d.Message}
		case e.Kind == eventRecovery && c.LastRecovery == nil:
			c.LastRecovery = &RecoveryView{Outcome: d.Code, Message: d.Message, At: e.CreatedAt}
		}
	}
	return c
}
