package pipeline

import "fmt"

// UnsupportedExecutionReason returns why w cannot be executed by this build,
// or "" when it can. Discovery and selection ship before execution: a workflow
// that is valid but not yet executable is shown as unavailable and must never
// silently degrade into a normal worker.
//
// Slice by slice this widens: Build on an existing worker, then attached Chat
// specialists, then Review.
func UnsupportedExecutionReason(w Workflow) string {
	if len(w.Stages) == 0 {
		return "The workflow has no stages."
	}
	for _, st := range w.Stages {
		if st.Kind == StageReview {
			return fmt.Sprintf("Stage %q (review) is not executable in this build yet.", st.ID)
		}
	}
	return ""
}
