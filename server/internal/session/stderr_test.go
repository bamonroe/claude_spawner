package session

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// A backend that dies without a usable stream says nothing actionable on its own;
// the cause is on stderr. Both must reach the caller in one error.
func TestWithStderrAppendsCause(t *testing.T) {
	base := errors.New("opencode stream ended without a text message")
	err := withStderr(base, "  Error: model qwen3.5-plus not found\n")
	if !errors.Is(err, base) {
		t.Fatalf("wrapped error lost its cause: %v", err)
	}
	if !strings.Contains(err.Error(), "model qwen3.5-plus not found") {
		t.Fatalf("stderr cause missing: %v", err)
	}
	if got := withStderr(base, "   \n"); got != base {
		t.Fatalf("empty stderr should return err unchanged, got %v", got)
	}
}

func TestWithStderrKeepsTail(t *testing.T) {
	long := strings.Repeat("x", stderrCauseMax*2) + "THE-CAUSE"
	err := withStderr(errors.New("boom"), long)
	if !strings.Contains(err.Error(), "THE-CAUSE") {
		t.Fatal("truncation dropped the tail, where the cause lives")
	}
	if len(err.Error()) > stderrCauseMax+64 {
		t.Fatalf("error not truncated: %d bytes", len(err.Error()))
	}
}

func TestStderrTailBounded(t *testing.T) {
	var tail stderrTail
	for i := 0; i < 100; i++ {
		_, _ = tail.Write([]byte(strings.Repeat("a", 100)))
	}
	_, _ = tail.Write([]byte("END"))
	if got := tail.String(); len(got) > stderrTailMax || !strings.HasSuffix(got, "END") {
		t.Fatalf("tail not bounded to the last %d bytes: len=%d", stderrTailMax, len(got))
	}
}

// The host executor must capture a process's stderr rather than dropping it.
func TestHostExecutorCapturesStderr(t *testing.T) {
	proc, err := HostExecutor{Bin: "sh"}.Start(context.Background(),
		&Session{Dir: t.TempDir()}, nil, "", []string{"-c", "echo boom 1>&2"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, proc.Stdout())
	if err := proc.Wait(); err != nil {
		t.Fatal(err)
	}
	if got := proc.Stderr(); got != "boom" {
		t.Fatalf("stderr = %q, want %q", got, "boom")
	}
}
