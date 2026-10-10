package testutil

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFakeRunnerNpmAndGulp(t *testing.T) {
	dir := t.TempDir()
	WriteFile(t, dir, "package.json", `{"name":"x"}`)
	WriteFile(t, dir, "package-lock.json", `{}`)

	f := &FakeRunner{Stamp: func(pkg, lock []byte) string { return "stamp-1" }}
	var out bytes.Buffer
	if err := f.Run(context.Background(), dir, &out, "npm", "ci", "--no-audit"); err != nil {
		t.Fatalf("npm ci: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", ".fe-lock-stamp")); err != nil {
		t.Errorf("stamp not created: %v", err)
	}

	if err := f.Run(context.Background(), dir, &out, "npx", "gulp", "pack_github"); err != nil {
		t.Fatalf("gulp: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "build", "github", "lampa", "index.html")); err != nil {
		t.Errorf("fake build output missing: %v", err)
	}
	if calls := f.CallsSnapshot(); len(calls) != 2 {
		t.Errorf("calls = %v, want 2 entries", calls)
	}
}

func TestFakeRunnerFailNpmCI(t *testing.T) {
	dir := t.TempDir()
	WriteFile(t, dir, "package.json", `{}`)
	WriteFile(t, dir, "package-lock.json", `{}`)
	f := &FakeRunner{Stamp: func(pkg, lock []byte) string { return "s" }, FailNpmCI: NpmCIFailAlways}
	if err := f.Run(context.Background(), dir, &bytes.Buffer{}, "npm", "ci"); err == nil {
		t.Error("expected npm ci failure")
	}
	// A failed install must leave no freshness stamp behind: a stale-looking
	// fresh stamp is the silent-drift failure mode the pipeline guards.
	if _, err := os.Stat(filepath.Join(dir, "node_modules", ".fe-lock-stamp")); !os.IsNotExist(err) {
		t.Errorf("failed npm ci must not write a stamp: %v", err)
	}
}

func TestNpmCIPresets(t *testing.T) {
	if err := NpmCIFailAlways([]string{"ci", "--no-audit"}); err == nil {
		t.Error("NpmCIFailAlways must fail the install")
	}
	if err := NpmCIFailAlways([]string{"ci", "--dry-run"}); err == nil {
		t.Error("NpmCIFailAlways must fail the probe too (desync)")
	}
	if err := NpmCIFailDryRunPasses([]string{"ci", "--dry-run", "--no-audit"}); err != nil {
		t.Errorf("probe must pass, got %v", err)
	}
	if err := NpmCIFailDryRunPasses([]string{"ci", "--no-audit"}); err == nil {
		t.Error("install must fail")
	}
}

func TestFakeRunnerGitIsReal(t *testing.T) {
	dir := t.TempDir()
	f := &FakeRunner{}
	if err := f.Run(context.Background(), dir, &bytes.Buffer{}, "git", "--version"); err != nil {
		t.Fatalf("git passthrough: %v", err)
	}
	if err := f.Run(context.Background(), dir, &bytes.Buffer{}, "npm", "wat"); err == nil {
		t.Error("unknown npm subcommand must fail")
	}
}

func TestFakeRunnerUnknownCommand(t *testing.T) {
	dir := t.TempDir()
	f := &FakeRunner{}
	if err := f.Run(context.Background(), dir, &bytes.Buffer{}, "make"); err == nil {
		t.Error("unknown command must fail")
	}
}

func TestInitGitRepoAndCommit(t *testing.T) {
	dir := t.TempDir()
	InitGitRepo(t, dir)
	WriteFile(t, dir, "a.txt", "hello\n")
	c1 := CommitAll(t, dir, "first")
	if len(c1) != 40 {
		t.Errorf("commit hash length = %d, want 40", len(c1))
	}
	WriteFile(t, dir, "a.txt", "changed\n")
	c2 := CommitAll(t, dir, "second")
	if c1 == c2 {
		t.Error("second commit produced the same hash")
	}
	if Git(t, dir, "status", "--porcelain") != "" {
		t.Error("working tree not clean after commit")
	}
}

func TestWaitForReturnsOnTrue(t *testing.T) {
	WaitFor(t, func() bool { return true }, "immediately true condition")
}

// Real npm install re-resolves and rewrites the lockfile; npm ci installs the
// given one and leaves it alone. The pipeline relies on that difference to tell
// a re-resolution from an install of a frozen lockfile.
func TestFakeRunnerInstallRewritesLockfile(t *testing.T) {
	dir := t.TempDir()
	WriteFile(t, dir, "package.json", `{"name":"x"}`)
	const seed = `{"lockfile":true}`
	WriteFile(t, dir, "package-lock.json", seed)

	// The stamp records the lockfile npm was given, so a rewrite is visible.
	f := &FakeRunner{Stamp: func(pkg, lock []byte) string { return string(lock) }}
	if err := f.Run(context.Background(), dir, &bytes.Buffer{}, "npm", "install"); err != nil {
		t.Fatalf("npm install: %v", err)
	}
	lock, err := os.ReadFile(filepath.Join(dir, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(lock) == seed {
		t.Error("npm install must rewrite the lockfile, as real npm does")
	}
	stamp, err := os.ReadFile(filepath.Join(dir, "node_modules", ".fe-lock-stamp"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stamp) != string(lock) {
		t.Errorf("stamp = %q, want the resolved lockfile %q", stamp, lock)
	}

	WriteFile(t, dir, "package-lock.json", seed)
	if err := f.Run(context.Background(), dir, &bytes.Buffer{}, "npm", "ci"); err != nil {
		t.Fatalf("npm ci: %v", err)
	}
	lock, err = os.ReadFile(filepath.Join(dir, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(lock) != seed {
		t.Errorf("npm ci must not rewrite the lockfile, got %q", lock)
	}
}

// As with ci, a --dry-run probe writes nothing: no lockfile rewrite, no
// freshness stamp (real npm install --dry-run touches nothing).
func TestFakeRunnerInstallDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	WriteFile(t, dir, "package.json", `{"name":"x"}`)
	const seed = `{"lockfile":true}`
	WriteFile(t, dir, "package-lock.json", seed)

	// The stamp records the lockfile npm was given, so any rewrite would show.
	f := &FakeRunner{Stamp: func(pkg, lock []byte) string { return string(lock) }}
	if err := f.Run(context.Background(), dir, &bytes.Buffer{}, "npm", "install", "--dry-run"); err != nil {
		t.Fatalf("npm install --dry-run: %v", err)
	}
	lock, err := os.ReadFile(filepath.Join(dir, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(lock) != seed {
		t.Errorf("npm install --dry-run must not rewrite the lockfile, got %q", lock)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", ".fe-lock-stamp")); !os.IsNotExist(err) {
		t.Errorf("npm install --dry-run must not write a stamp: %v", err)
	}
}
