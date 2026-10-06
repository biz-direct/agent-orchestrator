package pipeline

// UnsupportedExecutionReason returns why w cannot be executed by this build,
// or "" when it can. Discovery and selection ship before execution: a workflow
// that is valid but not yet executable is shown as unavailable and must never
// silently degrade into a normal worker.
//
// Later slices widen what this returns "" for (single-stage Build first, then
// attached specialists, then review).
func UnsupportedExecutionReason(w Workflow) string {
	return "Workflow execution is not available in this build. The workflow is valid and can be selected, but tasks still start as normal workers until execution support ships."
}
