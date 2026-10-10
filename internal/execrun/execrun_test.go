package execrun

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestExecRunnerStreamsOutput(t *testing.T) {
	var out bytes.Buffer
	r := ExecRunner{}
	if err := r.Run(context.Background(), "", &out, "git", "--version"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "git version") {
		t.Errorf("output = %q, want git version", out.String())
	}
}

func TestExecRunnerWorkingDirAndFailure(t *testing.T) {
	var out bytes.Buffer
	r := ExecRunner{}
	dir := t.TempDir()
	// Failing command surfaces as error, output still lands in the writer.
	if err := r.Run(context.Background(), dir, &out, "git", "rev-parse", "HEAD"); err == nil {
		t.Error("expected error for rev-parse in repo-less dir")
	}
	if out.Len() == 0 {
		t.Error("expected non-empty output from the failing command")
	}
}

func TestExecRunnerContextTimeout(t *testing.T) {
	var out bytes.Buffer
	r := ExecRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := r.Run(ctx, "", &out, "sleep", "5")
	if err == nil {
		t.Error("expected error for timed-out command")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Run returned after %v, want a fast return well before 5s", elapsed)
	}
}
