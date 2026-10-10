package gitops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huhen/lampa-web-builder/internal/testutil"
)

// fixture makes a local git repo serving as UPSTREAM_REPO.
func fixture(t *testing.T) (origin, c1 string) {
	t.Helper()
	origin = t.TempDir()
	testutil.InitGitRepo(t, origin)
	testutil.WriteFile(t, origin, "src/app.js", "var a = 1;\n")
	c1 = testutil.CommitAll(t, origin, "first")
	return origin, c1
}

func TestLsRemoteHead(t *testing.T) {
	origin, c1 := fixture(t)
	c := NewClient(origin, "main", testutil.FakeExec{})
	got, err := c.LsRemoteHead(context.Background())
	if err != nil {
		t.Fatalf("ls-remote: %v", err)
	}
	if got != c1 {
		t.Errorf("head = %s, want %s", got, c1)
	}
}

// TestLsRemoteHeadMissingBranch: ls-remote exits 0 with empty output when the
// branch does not exist; the client must report an error, not an empty hash.
func TestLsRemoteHeadMissingBranch(t *testing.T) {
	origin, _ := fixture(t)
	c := NewClient(origin, "nope", testutil.FakeExec{})
	_, err := c.LsRemoteHead(context.Background())
	if err == nil {
		t.Fatal("missing branch: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not mention %q", err, "not found")
	}
}

func TestEnsureCopyClonesOnce(t *testing.T) {
	origin, _ := fixture(t)
	dst := filepath.Join(t.TempDir(), "copy")
	runner := &testutil.FakeRunner{}
	c := NewClient(origin, "main", runner)
	ctx := context.Background()
	if err := c.EnsureCopy(ctx, dst); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); err != nil {
		t.Fatalf("no clone at %s: %v", dst, err)
	}
	// Second call is a no-op (must not fail on an existing directory).
	if err := c.EnsureCopy(ctx, dst); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	n := 0
	for _, call := range runner.CallsSnapshot() {
		if strings.HasPrefix(call, "git clone") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("git clone ran %d times, want 1; calls: %v", n, runner.CallsSnapshot())
	}
}

func TestFetchBringsNewCommit(t *testing.T) {
	origin, _ := fixture(t)
	dst := filepath.Join(t.TempDir(), "copy")
	c := NewClient(origin, "main", testutil.FakeExec{})
	ctx := context.Background()
	if err := c.EnsureCopy(ctx, dst); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, origin, "README.md", "new\n")
	c2 := testutil.CommitAll(t, origin, "second")

	if err := c.Fetch(ctx, dst); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	// The fetched object must exist in the copy (checkout will use it).
	var out bytes.Buffer
	if err := (testutil.FakeExec{}).RunRaw(ctx, dst, &out, "git", "cat-file", "-e", c2); err != nil {
		t.Fatalf("commit %s not fetched: %v", c2, err)
	}
}

// TestEnsureCopyReclonesTruncatedCopy reproduces issue #10: a process killed
// mid-clone leaves a .git directory behind, which the old readiness check (a
// bare os.Stat) accepted as a working copy.
func TestEnsureCopyReclonesTruncatedCopy(t *testing.T) {
	origin, _ := fixture(t)
	dst := filepath.Join(t.TempDir(), "copy")
	// A truncated clone: .git exists and HEAD points at a branch, but no
	// objects were fetched, so HEAD cannot be resolved to a commit.
	if err := os.MkdirAll(filepath.Join(dst, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewClient(origin, "main", &testutil.FakeRunner{})
	if err := c.EnsureCopy(context.Background(), dst); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// The truncated copy must have been replaced by a real, usable one.
	var out bytes.Buffer
	if err := (testutil.FakeExec{}).RunRaw(context.Background(), dst, &out, "git", "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
		t.Fatalf("copy is not usable after EnsureCopy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "src", "app.js")); err != nil {
		t.Errorf("checked-out file missing: %v", err)
	}
}

// TestEnsureCopyReclonesUnpublishedRef pins the other real-world truncated
// state from issue #10: .git exists with its structure but the cloned ref was
// never published, so HEAD cannot resolve. EnsureCopy must re-clone.
func TestEnsureCopyReclonesUnpublishedRef(t *testing.T) {
	origin, _ := fixture(t)
	dst := filepath.Join(t.TempDir(), "copy")
	for _, d := range []string{".git/objects", ".git/refs/heads"} {
		if err := os.MkdirAll(filepath.Join(dst, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dst, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, ".git", "config"), []byte("[core]\n\trepositoryformatversion = 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewClient(origin, "main", &testutil.FakeRunner{})
	if err := c.EnsureCopy(context.Background(), dst); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	var out bytes.Buffer
	if err := (testutil.FakeExec{}).RunRaw(context.Background(), dst, &out, "git", "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
		t.Fatalf("copy is not usable after EnsureCopy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "src", "app.js")); err != nil {
		t.Errorf("checked-out file missing: %v", err)
	}
}

// TestEnsureCopyReclonesEmptyDir: an existing empty directory is not a copy;
// spec §1 requires it to be re-cloned into.
func TestEnsureCopyReclonesEmptyDir(t *testing.T) {
	origin, _ := fixture(t)
	dst := filepath.Join(t.TempDir(), "copy")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewClient(origin, "main", &testutil.FakeRunner{})
	if err := c.EnsureCopy(context.Background(), dst); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "src", "app.js")); err != nil {
		t.Errorf("copy missing: %v", err)
	}
}

// TestEnsureCopyClearsStaleTemp: a temp directory left by a crash must not
// survive the next successful clone.
func TestEnsureCopyClearsStaleTemp(t *testing.T) {
	origin, _ := fixture(t)
	dst := filepath.Join(t.TempDir(), "copy")
	stale := dst + ".tmp"
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "leftover"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewClient(origin, "main", &testutil.FakeRunner{})
	if err := c.EnsureCopy(context.Background(), dst); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale temp must be gone, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "src", "app.js")); err != nil {
		t.Errorf("copy missing: %v", err)
	}
}

// TestEnsureCopyCloneFailureLeavesNothing: a failed clone must not publish a
// partial copy under dir, nor leave its temp behind (issue #10).
func TestEnsureCopyCloneFailureLeavesNothing(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "copy")
	c := NewClient(filepath.Join(t.TempDir(), "missing-origin"), "main", testutil.FakeExec{})
	if err := c.EnsureCopy(context.Background(), dst); err == nil {
		t.Fatal("expected clone failure")
	}
	for _, p := range []string{dst, dst + ".tmp"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s must not exist after a failed clone, stat err = %v", p, err)
		}
	}
}
