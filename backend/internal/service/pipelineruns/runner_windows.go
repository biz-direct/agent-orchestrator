//go:build windows

package pipelineruns

import "os/exec"

func configureProcessGroup(_ *exec.Cmd) {}

// killProcessGroup terminates the command's process. Windows has no process
// group signal without a job object; descendants that outlive the shell are a
// known limitation of validation on this platform.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
