package testutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// InitGitRepo prepares dir as a git repository on branch main with a local
// committer identity. Commit signing is disabled so a global gitconfig with
// commit.gpgsign cannot break CommitAll on machines without a signing key.
func InitGitRepo(t *testing.T, dir string) {
	t.Helper()
	Git(t, dir, "init", "-b", "main")
	Git(t, dir, "config", "user.email", "test@example.com")
	Git(t, dir, "config", "user.name", "Test")
	Git(t, dir, "config", "commit.gpgsign", "false")
}

// CommitAll stages everything and commits; returns the full commit hash.
func CommitAll(t *testing.T, dir, msg string) string {
	t.Helper()
	Git(t, dir, "add", "-A")
	Git(t, dir, "commit", "-q", "-m", msg)
	return Git(t, dir, "rev-parse", "HEAD")
}

// Git runs a git command in dir and returns trimmed stdout, failing the test
// on error (stderr lands in the fatal message).
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	stdout, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("git %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout, exitErr.Stderr)
		}
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(stdout))
}

// WriteFile writes a file inside dir, creating parent directories as needed
// and failing the test on error.
func WriteFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
