package pipeline

// UnsupportedExecutionReason returns why w cannot be executed by this build,
// or "" when it can. Discovery and selection ship before execution: a workflow
// that is valid but not yet executable is shown as unavailable and must never
// silently degrade into a normal worker.
//
// Every v1 stage kind executes: Build on the existing worker, attached Chat
// specialists, and Review through AO's built-in reviewer. Whether the running
// daemon can actually drive a stage (an executor, a review gateway) is checked
// when a run starts, so an unavailable stage fails visibly instead of degrading.
func UnsupportedExecutionReason(w Workflow) string {
	if len(w.Stages) == 0 {
		return "The workflow has no stages."
	}
	return ""
}
