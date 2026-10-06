package pipeline

import "fmt"

// UnsupportedExecutionReason returns why w cannot be executed by this build,
// or "" when it can. Discovery and selection ship before execution: a workflow
// that is valid but not yet executable is shown as unavailable and must never
// silently degrade into a normal worker.
//
// Slice by slice this widens: a single-stage Build workflow runs on an
// existing worker first; attached specialists, validation, and Review follow.
func UnsupportedExecutionReason(w Workflow) string {
	if len(w.Stages) == 0 {
		return "The workflow has no stages."
	}
	for _, st := range w.Stages {
		if st.Kind != StageBuild {
			return fmt.Sprintf("Stage %q (%s) is not executable in this build yet: only single-stage Build workflows can run.", st.ID, st.Kind)
		}
	}
	if len(w.Stages) > 1 {
		return "Multi-stage execution is not available in this build yet: only single-stage Build workflows can run."
	}
	return ""
}
