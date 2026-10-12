// Package crew defines AO's repository-defined crew roles and crew workflows:
// the on-disk format, discovery, and validation.
//
// Repository files are the only source of truth. Definitions live under
// .ao/crew/roles/*.yaml and .ao/crew/workflows/*.yaml. There is no visual
// editor and no global inheritance: AO reads, validates, and reports what the
// repository declares.
//
// A crew workflow is a policy over who may hold the write baton next: a forward
// order and the earlier roles each role may return it to. It is not a stage
// engine, so parallel and graph features are rejected.
package crew

// Repository layout and format limits.
const (
	// FormatVersion is the only definition format version understood.
	FormatVersion = 1

	// DefinitionDir is the repo-relative root of all crew definitions.
	DefinitionDir = ".ao/crew"
	// RolesDir holds one role profile per YAML file.
	RolesDir = DefinitionDir + "/roles"
	// WorkflowsDir holds one crew workflow per YAML file.
	WorkflowsDir = DefinitionDir + "/workflows"

	// ReviewStep is the reserved name of the optional final review step. It
	// may only be last in a workflow's order and is never a role id.
	ReviewStep = "review"

	// MaxDefinitionBytes bounds one definition file.
	MaxDefinitionBytes = 256 << 10
	// MaxInstructionBytes bounds inline or referenced instruction text.
	MaxInstructionBytes = 64 << 10
	// MaxDefinitionsPerKind bounds discovery work per directory.
	MaxDefinitionsPerKind = 200
	// MaxWorkflowRoles bounds the roles in one workflow.
	MaxWorkflowRoles = 12
	// MaxValidationCommands bounds one role's independent checks.
	MaxValidationCommands = 20
	// MaxAllowedPaths bounds one role's change-scope globs.
	MaxAllowedPaths = 100
	// MaxCommandBytes bounds one validation command line.
	MaxCommandBytes = 4096

	// DefaultLoopCap is the number of baton returns a workflow allows when it
	// does not declare a cap.
	DefaultLoopCap = 3
	// MaxLoopCap is the largest cap a definition may request.
	MaxLoopCap = 10

	// DefaultValidationTimeoutSeconds applies when a command omits a timeout.
	DefaultValidationTimeoutSeconds = 600
	// MaxValidationTimeoutSeconds bounds an explicit command timeout.
	MaxValidationTimeoutSeconds = 3600
)

// ValidationCommand is one repository-configured check AO executes
// independently of any agent report.
type ValidationCommand struct {
	ID             string `json:"id"`
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	// Required checks must pass before the baton may be handed on.
	Required bool `json:"required"`
}

// Role is a validated crew role profile.
type Role struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	// InstructionsSource is "inline" or the repo-relative instruction file.
	InstructionsSource string `json:"instructionsSource"`
	// InstructionsSHA256 is the content hash recorded as snapshot provenance.
	InstructionsSHA256 string `json:"instructionsSha256"`
	// DefinitionSHA256 is the hash of the definition file bytes.
	DefinitionSHA256 string `json:"definitionSha256"`
	// InstructionsText is the resolved instruction content. It is never
	// serialized in catalog responses.
	InstructionsText string `json:"-"`
	// Harness and Model are defaults only; an explicit user choice overrides
	// them.
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
	// AllowedPaths are repo-relative globs the role may change.
	AllowedPaths []string            `json:"allowedPaths"`
	Validation   []ValidationCommand `json:"validation"`
}

// Return is one allowed baton return: From may hand the baton back to any of
// the earlier roles in To.
type Return struct {
	From string   `json:"from"`
	To   []string `json:"to"`
}

// Workflow is a validated crew workflow.
type Workflow struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	// DefinitionSHA256 is the hash of the definition file bytes.
	DefinitionSHA256 string `json:"definitionSha256"`
	// InstructionsSource, InstructionsSHA256, and InstructionsText mirror the
	// Role fields for optional workflow-wide instructions.
	InstructionsSource string `json:"instructionsSource,omitempty"`
	InstructionsSHA256 string `json:"instructionsSha256,omitempty"`
	InstructionsText   string `json:"-"`
	// Roles are the declared role ids, in declaration order.
	Roles []string `json:"roles"`
	// Order is the forward order of the baton: every role once, optionally
	// followed by ReviewStep.
	Order []string `json:"order"`
	// Returns lists, per role (or the review step), the earlier roles it may
	// return the baton to. Sorted by the position of From in Order.
	Returns []Return `json:"returns"`
	// LoopCap bounds how many returns a crew run may use before it pauses for
	// a person.
	LoopCap int `json:"loopCap"`
}

// Diagnostic is one actionable validation problem.
type Diagnostic struct {
	// File is the repo-relative definition file ("" for catalog-level issues).
	File string `json:"file,omitempty"`
	// Field is the dotted/indexed path of the offending field, when known.
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// RoleEntry is one discovered role definition, valid or not.
type RoleEntry struct {
	// ID is the declared id, or the file stem when the file did not declare a
	// usable one.
	ID          string       `json:"id"`
	File        string       `json:"file"`
	Valid       bool         `json:"valid"`
	Role        *Role        `json:"role,omitempty"`
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
	Roles     []RoleEntry     `json:"roles"`
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

// FindRole returns the role entry with the given id.
func (c Catalog) FindRole(id string) (RoleEntry, bool) {
	for _, r := range c.Roles {
		if r.ID == id {
			return r, true
		}
	}
	return RoleEntry{}, false
}
