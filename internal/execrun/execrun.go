// Package execrun runs external commands (git/npm/npx) with combined output
// captured into a writer — in production that writer is the build log.
package execrun

import (
	"context"
	"io"
	"os/exec"
	"time"
)

// Runner runs a command in dir, streaming combined output to w.
//
// An empty dir means the process working directory is inherited. Stdout and
// stderr both go to w, so the two streams interleave nondeterministically.
type Runner interface {
	Run(ctx context.Context, dir string, w io.Writer, name string, args ...string) error
}

// ExecRunner is the production Runner backed by os/exec.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, dir string, w io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = w
	cmd.Stderr = w
	// WaitDelay bounds the pipe drain after the kill: cancellation only
	// SIGKILLs the direct child, and Wait would otherwise block forever on
	// pipes inherited by orphaned children of npm/gulp.
	cmd.WaitDelay = 10 * time.Second
	return cmd.Run()
}
