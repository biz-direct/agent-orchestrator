package daemon

import (
	"log/slog"
	"testing"
)

func TestLogLevelFromEnvDefaultsToInfo(t *testing.T) {
	for _, raw := range []string{"", "  ", "bogus"} {
		if got := logLevelFromEnv(raw); got != slog.LevelInfo {
			t.Fatalf("logLevelFromEnv(%q) = %v, want info", raw, got)
		}
	}
}

func TestLogLevelFromEnvHonorsExplicitLevel(t *testing.T) {
	if got := logLevelFromEnv("debug"); got != slog.LevelDebug {
		t.Fatalf("debug = %v", got)
	}
	if got := logLevelFromEnv(" warn "); got != slog.LevelWarn {
		t.Fatalf("warn = %v", got)
	}
}
