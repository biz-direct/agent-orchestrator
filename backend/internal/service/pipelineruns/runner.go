package pipelineruns

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// Log bounds: the head shows how a command started, the tail shows how it
// ended; the middle of a long log is dropped, never the verdict.
const (
	logHeadBytes = 8 << 10
	logTailBytes = 24 << 10
	// processWaitDelay bounds how long Wait lingers on inherited pipes after the
	// process is gone, so a stray grandchild cannot hang a validation round.
	processWaitDelay = 3 * time.Second
)

// CommandSpec is one independently executed validation command.
type CommandSpec struct {
	Dir     string
	Command string
	Timeout time.Duration
	Env     []string
}

// CommandOutcome is what happened, before classification.
type CommandOutcome struct {
	ExitCode int
	// Output is bounded and sanitized combined stdout+stderr.
	Output    string
	Truncated bool
	TimedOut  bool
	Cancelled bool
	// LaunchErr is set when the process could not be started at all.
	LaunchErr error
}

// CommandRunner executes validation commands. Implementations own the process
// tree they start: when Run returns, nothing it launched is still running.
type CommandRunner interface {
	Run(ctx context.Context, spec CommandSpec) CommandOutcome
}

// ExecRunner runs commands through the platform shell in their own process
// group, enforces the timeout, and kills the whole group on timeout or
// cancellation so no command outlives its round.
type ExecRunner struct{}

// Run implements CommandRunner.
func (ExecRunner) Run(ctx context.Context, spec CommandSpec) CommandOutcome {
	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = aoprocess.CommandContext(runCtx, "cmd", "/C", spec.Command)
	} else {
		cmd = aoprocess.CommandContext(runCtx, "/bin/sh", "-c", spec.Command)
	}
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	buf := newBoundedBuffer(logHeadBytes, logTailBytes)
	cmd.Stdout = buf
	cmd.Stderr = buf
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = processWaitDelay

	out := CommandOutcome{ExitCode: -1}
	if err := cmd.Start(); err != nil {
		out.LaunchErr = err
		return out
	}
	err := cmd.Wait()
	// Even after a clean exit, a command may have left children behind; the
	// group is always reaped so nothing outlives the round.
	_ = killProcessGroup(cmd)
	out.Output, out.Truncated = buf.render()
	out.Output = sanitizeLog(out.Output, spec.Env)
	switch {
	case runCtx.Err() != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		out.TimedOut = true
	case ctx.Err() != nil:
		out.Cancelled = true
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
		out.ExitCode = 0
	case errors.As(err, &exit):
		out.ExitCode = exit.ExitCode()
	case out.TimedOut || out.Cancelled:
		// The kill is the explanation; there is no meaningful exit code.
	default:
		out.LaunchErr = err
	}
	return out
}

// boundedBuffer keeps the first head bytes and last tail bytes written.
type boundedBuffer struct {
	mu      sync.Mutex
	headCap int
	tailCap int
	head    bytes.Buffer
	tail    []byte
	total   int
}

func newBoundedBuffer(head, tail int) *boundedBuffer {
	return &boundedBuffer{headCap: head, tailCap: tail}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.total += n
	if b.head.Len() < b.headCap {
		room := b.headCap - b.head.Len()
		take := min(room, len(p))
		b.head.Write(p[:take])
		p = p[take:]
	}
	if len(p) > 0 {
		b.tail = append(b.tail, p...)
		if len(b.tail) > b.tailCap {
			b.tail = append([]byte(nil), b.tail[len(b.tail)-b.tailCap:]...)
		}
	}
	return n, nil
}

func (b *boundedBuffer) render() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.tail) == 0 {
		return strings.ToValidUTF8(b.head.String(), "�"), false
	}
	dropped := b.total - b.head.Len() - len(b.tail)
	var out strings.Builder
	out.WriteString(b.head.String())
	if dropped > 0 {
		out.WriteString("\n… ")
		out.WriteString(itoa(dropped))
		out.WriteString(" bytes omitted …\n")
	}
	out.Write(b.tail)
	return strings.ToValidUTF8(out.String(), "�"), dropped > 0
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d [20]byte
	i := len(d)
	for n > 0 {
		i--
		d[i] = byte('0' + n%10)
		n /= 10
	}
	return string(d[i:])
}

var (
	ansiPattern    = regexp.MustCompile(`\x1b\[[0-9;?]*[\x20-\x2f]*[\x40-\x7e]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)
	secretPatterns = []*regexp.Regexp{
		regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
		regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
		regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{20,}`),
	}
	secretEnvName = regexp.MustCompile(`(?i)(token|secret|password|passwd|credential|api_?key|private_?key)`)
)

// sanitizeLog makes captured output safe to store and show: terminal escape
// sequences and control bytes are removed and anything that looks like a
// credential, including the value of any secret-named environment variable the
// command was given, is redacted.
func sanitizeLog(s string, env []string) string {
	s = ansiPattern.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\r':
			return '\n'
		case r < 0x20 || r == 0x7f || r == utf8.RuneError:
			return -1
		}
		return r
	}, s)
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if ok && len(value) >= 8 && secretEnvName.MatchString(name) {
			s = strings.ReplaceAll(s, value, "[redacted]")
		}
	}
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	return s
}

// commandEnv is the environment validation commands receive: the daemon's own,
// minus every AO-internal variable (session capabilities, run-file paths).
func commandEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "AO_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "AO_PIPELINE_VALIDATION=1")
}
