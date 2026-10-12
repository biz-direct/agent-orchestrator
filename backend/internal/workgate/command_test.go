package workgate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func skipWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell commands")
	}
}

func runCmd(t *testing.T, ctx context.Context, command string, timeout time.Duration, env []string) CommandOutcome {
	t.Helper()
	return ExecRunner{}.Run(ctx, CommandSpec{Dir: t.TempDir(), Command: command, Timeout: timeout, Env: env})
}

func TestClassifySeparatesGenuineFailureFromOperationalProblems(t *testing.T) {
	cases := []struct {
		name string
		in   CommandOutcome
		want CommandStatus
	}{
		{"clean exit passes", CommandOutcome{ExitCode: 0}, CommandPassed},
		{"ordinary non-zero is a genuine failure", CommandOutcome{ExitCode: 1}, CommandFailed},
		{"exit 2 is a genuine failure", CommandOutcome{ExitCode: 2}, CommandFailed},
		{"launch failure is operational", CommandOutcome{ExitCode: -1, LaunchErr: errors.New("boom")}, CommandOperational},
		{"126 not executable is operational", CommandOutcome{ExitCode: 126}, CommandOperational},
		{"127 not found is operational", CommandOutcome{ExitCode: 127}, CommandOperational},
		{"no exit status is operational", CommandOutcome{ExitCode: -1}, CommandOperational},
		{"timeout is not a code defect", CommandOutcome{ExitCode: -1, TimedOut: true}, CommandTimeout},
		{"cancellation is not a code defect", CommandOutcome{ExitCode: -1, Cancelled: true}, CommandCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := Classify(tc.in)
			if got != tc.want || (got != CommandPassed && detail == "") {
				t.Fatalf("got %s %q, want %s", got, detail, tc.want)
			}
		})
	}
}

func TestRunnerReportsPassFailAndLaunchFailure(t *testing.T) {
	skipWindows(t)
	ctx := context.Background()
	if o := runCmd(t, ctx, "echo ok", 10*time.Second, nil); o.ExitCode != 0 || !strings.Contains(o.Output, "ok") {
		t.Fatalf("pass: %+v", o)
	}
	if o := runCmd(t, ctx, "echo bad >&2; exit 3", 10*time.Second, nil); o.ExitCode != 3 || !strings.Contains(o.Output, "bad") {
		t.Fatalf("a genuine failure carries its stderr: %+v", o)
	}
	if st, _ := Classify(runCmd(t, ctx, "exit 127", 10*time.Second, nil)); st != CommandOperational {
		t.Fatalf("exit 127 = %s", st)
	}
	missing := ExecRunner{}.Run(ctx, CommandSpec{Dir: filepath.Join(t.TempDir(), "gone"), Command: "true", Timeout: 5 * time.Second})
	if missing.LaunchErr == nil {
		t.Fatalf("a missing working directory is a launch failure: %+v", missing)
	}
	if st, _ := Classify(missing); st != CommandOperational {
		t.Fatalf("launch failure = %s", st)
	}
}

func TestRunnerTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	skipWindows(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	start := time.Now()
	o := runCmd(t, context.Background(), "sleep 60 & echo $! > "+pidFile+"; wait", 500*time.Millisecond, nil)
	if !o.TimedOut || o.Cancelled || o.LaunchErr != nil {
		t.Fatalf("want a timeout: %+v", o)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("the timeout did not stop the command promptly")
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(raw))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := runCmd(t, context.Background(), "kill -0 "+pid, 5*time.Second, nil).ExitCode; err != 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild %s survived the timeout", pid)
}

func TestRunnerCancellationIsNotATimeout(t *testing.T) {
	skipWindows(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	o := runCmd(t, ctx, "sleep 60", 30*time.Second, nil)
	if !o.Cancelled || o.TimedOut {
		t.Fatalf("want cancelled: %+v", o)
	}
}

func TestRunnerBoundsAndRedactsLogs(t *testing.T) {
	skipWindows(t)
	token := "ghp_" + strings.Repeat("a", 30)
	cmd := "echo start; echo " + token + "; echo $MY_SECRET_TOKEN; printf '\\033[31mred\\033[0m\\n'; i=0; while [ $i -lt 6000 ]; do echo line-$i-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx; i=$((i+1)); done; echo the-verdict"
	o := runCmd(t, context.Background(), cmd, 30*time.Second, []string{"PATH=" + os.Getenv("PATH"), "MY_SECRET_TOKEN=supersecretvalue"})
	if !o.Truncated || len(o.Output) > logHeadBytes+logTailBytes+200 {
		t.Fatalf("log not bounded: truncated=%v len=%d", o.Truncated, len(o.Output))
	}
	if !strings.Contains(o.Output, "start") || !strings.Contains(o.Output, "the-verdict") {
		t.Fatal("the head and the verdict tail must survive")
	}
	for _, leaked := range []string{token, "supersecretvalue", "\x1b["} {
		if strings.Contains(o.Output, leaked) {
			t.Fatalf("output leaks %q", leaked)
		}
	}
	if !strings.Contains(o.Output, "[redacted]") || !strings.Contains(o.Output, "red") {
		t.Fatalf("redaction marker or plain text missing: %q", o.Output[:200])
	}
}

func TestCommandEnvIsAnAllowlistAndNeverCarriesCredentials(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "s1")
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "key")
	t.Setenv("LC_ALL", "C")
	env := CommandEnv("MARK=1")
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, banned := range []string{"AO_SESSION_ID=", "GITHUB_TOKEN=", "AWS_SECRET_ACCESS_KEY="} {
		if strings.Contains(joined, "\n"+banned) {
			t.Fatalf("%s must not be inherited", banned)
		}
	}
	for _, want := range []string{"\nLC_ALL=C\n", "\nMARK=1\n"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %q", want, joined)
		}
	}
	if !strings.Contains(joined, "PATH=") {
		t.Fatal("PATH must be inherited")
	}
}
