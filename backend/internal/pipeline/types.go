// Package pipeline defines AO's repository-defined specialist profiles and
// sequential pipelines: the on-disk format, discovery, and validation.
//
// Repository files are the only source of truth in v1. Definitions live under
// .ao/pipelines/profiles/*.yaml and .ao/pipelines/workflows/*.yaml. There is no
// visual editor and no global profile inheritance: AO reads, validates, and
// reports what the repository declares.
package pipeline

// Repository layout and format limits.
const (
	// FormatVersion is the only definition format version v1 understands.
	FormatVersion = 1

	// DefinitionDir is the repo-relative root of all pipeline definitions.
	DefinitionDir = ".ao/pipelines"
	// ProfilesDir holds one specialist profile per YAML file.
	ProfilesDir = DefinitionDir + "/profiles"
	// WorkflowsDir holds one sequential workflow per YAML file.
	WorkflowsDir = DefinitionDir + "/workflows"

	// MaxDefinitionBytes bounds one definition file.
	MaxDefinitionBytes = 256 << 10
	// MaxInstructionBytes bounds inline or referenced instruction text.
	MaxInstructionBytes = 64 << 10
	// MaxDefinitionsPerKind bounds discovery work per directory.
	MaxDefinitionsPerKind = 200
	// MaxStages bounds one workflow.
	MaxStages = 12
	// MaxValidationCommands bounds one profile's independent checks.
	MaxValidationCommands = 20
	// MaxAllowedPaths bounds one profile's change-scope globs.
	MaxAllowedPaths = 100
	// MaxCommandBytes bounds one validation command line.
	MaxCommandBytes = 4096

	// DefaultRepairBudget is the shared number of automatic returns to Build.
	DefaultRepairBudget = 3
	// MaxRepairBudget is the largest budget a definition may request.
	MaxRepairBudget = 10

	// DefaultValidationTimeoutSeconds applies when a command omits a timeout.
	DefaultValidationTimeoutSeconds = 600
	// MaxValidationTimeoutSeconds bounds an explicit command timeout.
	MaxValidationTimeoutSeconds = 3600
)

// StageKind is the closed vocabulary of v1 stage kinds.
type StageKind string

const (
	// StageBuild is the regular worker: it owns the task, worktree, branch, PR.
	StageBuild StageKind = "build"
	// StageSpecialist is an attached Chat specialist built from a profile.
	StageSpecialist StageKind = "specialist"
	// StageReview is AO's existing built-in reviewer subsystem.
	StageReview StageKind = "review"
)

// Valid reports whether k is a stage kind v1 supports.
func (k StageKind) Valid() bool {
	switch k {
	case StageBuild, StageSpecialist, StageReview:
		return true
	}
	return false
}

// ValidationCommand is one repository-configured check AO executes
// independently of any agent report.
type ValidationCommand struct {
	ID             string `json:"id"`
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	// Required checks must pass before a stage may advance.
	Required bool `json:"required"`
}

// Profile is a validated specialist profile.
type Profile struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	// InstructionsSource is "inline" or the repo-relative instruction file.
	InstructionsSource string `json:"instructionsSource"`
	// InstructionsSHA256 is the content hash recorded as snapshot provenance.
	InstructionsSHA256 string `json:"instructionsSha256"`
	// InstructionsText is the resolved instruction content. It is never
	// serialized in catalog responses; run snapshots copy it explicitly.
	InstructionsText string `json:"-"`
	// Harness and Model are defaults only. An explicit user choice overrides
	// them; neither route may bypass instructions, path constraints, or gates.
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
	// AllowedPaths are repo-relative globs the specialist may change.
	AllowedPaths []string            `json:"allowedPaths"`
	Validation   []ValidationCommand `json:"validation"`
}

// Stage is one ordered workflow step.
type Stage struct {
	ID          string    `json:"id"`
	Kind        StageKind `json:"kind"`
	Description string    `json:"description,omitempty"`
	// Profile names the specialist profile (specialist stages only).
	Profile string `json:"profile,omitempty"`
	// RepairTo names the earlier Build stage that receives this stage's
	// failures. Empty means a failure pauses for a human instead.
	RepairTo string `json:"repairTo,omitempty"`
}

// Workflow is a validated sequential workflow.
type Workflow struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	// InstructionsSource, InstructionsSHA256, and InstructionsText mirror the
	// Profile fields for optional workflow-wide instructions.
	InstructionsSource string  `json:"instructionsSource,omitempty"`
	InstructionsSHA256 string  `json:"instructionsSha256,omitempty"`
	InstructionsText   string  `json:"-"`
	RepairBudget       int     `json:"repairBudget"`
	Stages             []Stage `json:"stages"`
}

// Diagnostic is one actionable validation problem.
type Diagnostic struct {
	// File is the repo-relative definition file ("" for catalog-level issues).
	File string `json:"file,omitempty"`
	// Field is the dotted/indexed path of the offending field, when known.
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// ProfileEntry is one discovered profile definition, valid or not.
type ProfileEntry struct {
	// ID is the declared id, or the file stem when the file did not declare a
	// usable one.
	ID          string       `json:"id"`
	File        string       `json:"file"`
	Valid       bool         `json:"valid"`
	Profile     *Profile     `json:"profile,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// WorkflowEntry is one discovered workflow definition, valid or not.
type WorkflowEntry struct {
	ID          string       `json:"id"`
	File        string       `json:"file"`
	Valid       bool         `json:"valid"`
	Workflow    *Workflow    `json:"workflow,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// Catalog is everything discovered in one repository.
type Catalog struct {
	Profiles  []ProfileEntry  `json:"profiles"`
	Workflows []WorkflowEntry `json:"workflows"`
	// Diagnostics are catalog-level problems such as an unreadable directory.
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// FindWorkflow returns the workflow entry with the given id.
func (c Catalog) FindWorkflow(id string) (WorkflowEntry, bool) {
	for _, w := range c.Workflows {
		if w.ID == id {
			return w, true
		}
	}
	return WorkflowEntry{}, false
}

// FindProfile returns the profile entry with the given id.
func (c Catalog) FindProfile(id string) (ProfileEntry, bool) {
	for _, p := range c.Profiles {
		if p.ID == id {
			return p, true
		}
	}
	return ProfileEntry{}, false
}
