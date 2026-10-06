package pipelineruns

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/pipeline"
)

// SnapshotSchemaVersion versions the persisted snapshot JSON.
const SnapshotSchemaVersion = 1

// Settings sources for a stage's harness/model, in precedence order.
const (
	// SourceUser is an explicit user choice; it alone may override defaults.
	SourceUser = "user"
	// SourceProfile is the specialist profile's repository default.
	SourceProfile = "profile"
	// SourceWorker is the regular worker's own pinned harness/model (Build).
	SourceWorker = "worker"
)

// Snapshot is the frozen input of one run. Repository edits made after the run
// starts never alter it: instruction contents are copied, not referenced, and
// every definition carries the hash of the file it came from.
type Snapshot struct {
	SchemaVersion int               `json:"schemaVersion"`
	CapturedAt    time.Time         `json:"capturedAt"`
	RequestedBy   string            `json:"requestedBy"`
	Workflow      SnapshotWorkflow  `json:"workflow"`
	Profiles      []SnapshotProfile `json:"profiles"`
	Stages        []SnapshotStage   `json:"stages"`
}

// SnapshotWorkflow is the frozen workflow definition.
type SnapshotWorkflow struct {
	ID                 string `json:"id"`
	File               string `json:"file"`
	DefinitionSHA256   string `json:"definitionSha256"`
	Description        string `json:"description"`
	RepairBudget       int    `json:"repairBudget"`
	InstructionsSource string `json:"instructionsSource,omitempty"`
	InstructionsSHA256 string `json:"instructionsSha256,omitempty"`
	Instructions       string `json:"instructions,omitempty"`
}

// SnapshotProfile is a frozen profile including its resolved instruction text
// and validation configuration.
type SnapshotProfile struct {
	ID                 string                       `json:"id"`
	File               string                       `json:"file"`
	DefinitionSHA256   string                       `json:"definitionSha256"`
	Description        string                       `json:"description"`
	InstructionsSource string                       `json:"instructionsSource"`
	InstructionsSHA256 string                       `json:"instructionsSha256"`
	Instructions       string                       `json:"instructions"`
	AllowedPaths       []string                     `json:"allowedPaths"`
	Validation         []pipeline.ValidationCommand `json:"validation"`
}

// SnapshotStage is one frozen stage with its resolved harness/model settings.
type SnapshotStage struct {
	ID       string             `json:"id"`
	Kind     pipeline.StageKind `json:"kind"`
	Profile  string             `json:"profile,omitempty"`
	RepairTo string             `json:"repairTo,omitempty"`
	Harness  string             `json:"harness,omitempty"`
	Model    string             `json:"model,omitempty"`
	// SettingsSource records why Harness/Model have these values.
	SettingsSource string `json:"settingsSource"`
}

// StageOverride is an explicit user harness/model choice for one stage.
type StageOverride struct {
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
}

// snapshotInput carries everything the snapshot freezes.
type snapshotInput struct {
	Workflow    pipeline.WorkflowEntry
	Catalog     pipeline.Catalog
	Worker      domain.SessionRecord
	RequestedBy domain.PipelineRequester
	Overrides   map[string]StageOverride
	Now         time.Time
}

// buildSnapshot freezes the workflow and every profile it references and
// resolves each stage's harness/model. Only an explicit user override changes a
// default; instructions, path constraints, and validation always come from the
// definitions regardless of who started the run.
func buildSnapshot(in snapshotInput) (Snapshot, string, string, error) {
	if in.Workflow.Workflow == nil || !in.Workflow.Valid {
		return Snapshot{}, "", "", fmt.Errorf("workflow %q is not valid", in.Workflow.ID)
	}
	w := in.Workflow.Workflow
	snap := Snapshot{
		SchemaVersion: SnapshotSchemaVersion,
		CapturedAt:    in.Now.UTC(),
		RequestedBy:   string(in.RequestedBy),
		Workflow: SnapshotWorkflow{
			ID: w.ID, File: in.Workflow.File, DefinitionSHA256: w.DefinitionSHA256, Description: w.Description,
			RepairBudget: w.RepairBudget, InstructionsSource: w.InstructionsSource,
			InstructionsSHA256: w.InstructionsSHA256, Instructions: w.InstructionsText,
		},
		Profiles: []SnapshotProfile{},
		Stages:   []SnapshotStage{},
	}
	seen := map[string]bool{}
	for _, st := range w.Stages {
		ss := SnapshotStage{ID: st.ID, Kind: st.Kind, Profile: st.Profile, RepairTo: st.RepairTo}
		var profile *pipeline.Profile
		if st.Profile != "" {
			pe, ok := in.Catalog.FindProfile(st.Profile)
			if !ok || !pe.Valid || pe.Profile == nil {
				return Snapshot{}, "", "", fmt.Errorf("stage %q references unavailable profile %q", st.ID, st.Profile)
			}
			profile = pe.Profile
			if !seen[profile.ID] {
				seen[profile.ID] = true
				snap.Profiles = append(snap.Profiles, SnapshotProfile{
					ID: profile.ID, File: pe.File, DefinitionSHA256: profile.DefinitionSHA256, Description: profile.Description,
					InstructionsSource: profile.InstructionsSource, InstructionsSHA256: profile.InstructionsSHA256,
					Instructions: profile.InstructionsText, AllowedPaths: append([]string{}, profile.AllowedPaths...),
					Validation: append([]pipeline.ValidationCommand{}, profile.Validation...),
				})
			}
		}
		resolveStageSettings(&ss, profile, in.Worker)
		if ov, ok := in.Overrides[st.ID]; ok {
			if st.Kind == pipeline.StageBuild {
				return Snapshot{}, "", "", fmt.Errorf("stage %q is the regular worker and keeps its own harness and model", st.ID)
			}
			if ov.Harness != "" {
				ss.Harness = ov.Harness
			}
			if ov.Model != "" {
				ss.Model = ov.Model
			}
			ss.SettingsSource = SourceUser
		}
		snap.Stages = append(snap.Stages, ss)
	}
	for id := range in.Overrides {
		if !seen[id] && !stageExists(w.Stages, id) {
			return Snapshot{}, "", "", fmt.Errorf("override names unknown stage %q", id)
		}
	}
	sort.Slice(snap.Profiles, func(i, j int) bool { return snap.Profiles[i].ID < snap.Profiles[j].ID })
	data, err := json.Marshal(snap)
	if err != nil {
		return Snapshot{}, "", "", fmt.Errorf("marshal snapshot: %w", err)
	}
	sum := sha256.Sum256(data)
	return snap, string(data), hex.EncodeToString(sum[:]), nil
}

func stageExists(stages []pipeline.Stage, id string) bool {
	for _, s := range stages {
		if s.ID == id {
			return true
		}
	}
	return false
}

// resolveStageSettings applies the default precedence below an explicit user
// choice: Build keeps the worker's pinned harness/model; a specialist uses its
// profile defaults.
func resolveStageSettings(ss *SnapshotStage, profile *pipeline.Profile, worker domain.SessionRecord) {
	switch {
	case ss.Kind == pipeline.StageBuild:
		ss.Harness, ss.Model, ss.SettingsSource = string(worker.Harness), worker.Metadata.Model, SourceWorker
	case profile != nil:
		ss.Harness, ss.Model, ss.SettingsSource = profile.Harness, profile.Model, SourceProfile
	default:
		ss.SettingsSource = SourceProfile
	}
}

// parseSnapshot decodes a persisted snapshot.
func parseSnapshot(raw string) (Snapshot, error) {
	var snap Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return Snapshot{}, fmt.Errorf("decode pipeline snapshot: %w", err)
	}
	return snap, nil
}

func (s Snapshot) stage(id string) (SnapshotStage, int, bool) {
	for i, st := range s.Stages {
		if st.ID == id {
			return st, i, true
		}
	}
	return SnapshotStage{}, -1, false
}
