// Package testutil holds helpers shared by tests of several packages:
// disposable git repositories and a command runner that fakes npm/npx while
// passing git through to the real binary.
package testutil

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// FakeRunner runs real git and fakes npm/npx so pipeline and builder tests
// avoid the network and a real node toolchain. It is safe for concurrent use.
type FakeRunner struct {
	// Stamp computes the node_modules freshness stamp exactly the way the
	// pipeline does; tests inject pipeline.DepsStamp. Injectable so this
	// test-only package does not import pipeline.
	Stamp func(packageJSON, lockJSON []byte) string

	// FailNpmCI, when non-nil, returns the error of an `npm ci` invocation
	// given its arguments — tests distinguish the real install from the
	// --dry-run probe (issue #12). nil means every npm ci succeeds.
	FailNpmCI func(args []string) error

	// SilentGulp makes gulp commands succeed without writing any build output
	// (simulates a build that produces no build/github/lampa directory).
	SilentGulp bool

	// EmptyGulp makes pack_github create an empty build/github/lampa
	// directory with no files in it (simulates gulp packing nothing).
	EmptyGulp bool

	mu    sync.Mutex
	calls []string
}

// Run implements execrun.Runner.
func (f *FakeRunner) Run(ctx context.Context, dir string, w io.Writer, name string, args ...string) error {
	call := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	fmt.Fprintf(w, "$ %s\n", call)
	switch name {
	case "git":
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Stdout = w
		cmd.Stderr = w
		return cmd.Run()
	case "npm":
		if len(args) == 0 {
			return fmt.Errorf("npm: no subcommand")
		}
		switch args[0] {
		case "ci":
			if f.FailNpmCI != nil {
				if err := f.FailNpmCI(args); err != nil {
					return err
				}
			}
			// A --dry-run probe installs nothing, so it must not write the
			// freshness stamp either (real npm ci --dry-run touches nothing).
			for _, a := range args {
				if a == "--dry-run" {
					return nil
				}
			}
			return f.fakeInstall(dir, false)
		case "install":
			// Real npm install re-resolves and rewrites the lockfile; npm ci
			// does not. Tests rely on that difference to tell a re-resolution
			// from an install of a frozen lockfile.
			return f.fakeInstall(dir, true)
		default:
			return fmt.Errorf("fake runner: unexpected npm subcommand %q", args[0])
		}
	case "npx":
		if f.SilentGulp {
			return nil
		}
		if len(args) >= 2 && args[0] == "gulp" && args[1] == "pack_github" {
			out := filepath.Join(dir, "build", "github", "lampa")
			if err := os.MkdirAll(out, 0o755); err != nil {
				return err
			}
			if f.EmptyGulp {
				return nil
			}
			return os.WriteFile(filepath.Join(out, "index.html"), []byte("<html>lampa</html>\n"), 0o644)
		}
		return nil
	default:
		return fmt.Errorf("fake runner: unexpected command %q", name)
	}
}

// CallsSnapshot returns a copy of the recorded "name args..." entries. Safe
// to call while Run may still be executing in another goroutine.
func (f *FakeRunner) CallsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// FakeExec is a plain pass-through Runner for tests that need to run real
// commands themselves.
type FakeExec struct{}

// Run runs the command with os/exec.
func (FakeExec) Run(ctx context.Context, dir string, w io.Writer, name string, args ...string) error {
	return FakeExec{}.RunRaw(ctx, dir, w, name, args...)
}

// RunRaw is Run without the interface, for direct calls in tests.
func (FakeExec) RunRaw(ctx context.Context, dir string, w io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

func (f *FakeRunner) fakeInstall(dir string, rewriteLock bool) error {
	if f.Stamp == nil {
		return fmt.Errorf("FakeRunner.Stamp is not set")
	}
	pkg, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return err
	}
	if rewriteLock {
		// Deterministic stand-in for npm's resolution: derived from the
		// manifest, so tests can tell a resolved lockfile from the seed.
		resolved := fmt.Sprintf("{\"lockfileVersion\":3,\"resolved_from\":%q}\n", pkg)
		if err := os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte(resolved), 0o644); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		return err
	}
	lock, err := os.ReadFile(filepath.Join(dir, "package-lock.json"))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "node_modules", ".fe-lock-stamp"), []byte(f.Stamp(pkg, lock)), 0o644)
}
