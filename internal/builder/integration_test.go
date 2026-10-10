package builder

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huhen/lampa-web-builder/internal/config"
	"github.com/huhen/lampa-web-builder/internal/gitops"
	"github.com/huhen/lampa-web-builder/internal/pipeline"
	"github.com/huhen/lampa-web-builder/internal/state"
	"github.com/huhen/lampa-web-builder/internal/testutil"
)

// fixtureAssets makes patches/overlay/lockfile with a placeholder in both.
func fixtureAssets(t *testing.T) string {
	t.Helper()
	assets := t.TempDir()
	testutil.WriteFile(t, assets, "patches/010-test.patch", ""+
		"diff --git a/src/app.js b/src/app.js\n"+
		"--- a/src/app.js\n"+
		"+++ b/src/app.js\n"+
		"@@ -1 +1,2 @@\n"+
		" var a = 1;\n"+
		"+var cub = ['{{CUB_DOMAIN}}'];\n")
	testutil.WriteFile(t, assets, "overlay/public/overlay.js", "// mirror {{CUB_DOMAIN}}\n")
	testutil.WriteFile(t, assets, "package-lock.json", `{"lock":true}`+"\n")
	return assets
}

func TestIntegrationUpstreamSwapAndBadCommit(t *testing.T) {
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	// git init needs the directory to exist (git does not create it).
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.InitGitRepo(t, origin)
	testutil.WriteFile(t, origin, "src/app.js", "var a = 1;\n")
	testutil.WriteFile(t, origin, "package.json", `{"name":"lampa"}`+"\n")
	c1 := testutil.CommitAll(t, origin, "c1")
	assets := fixtureAssets(t)

	cfg := config.Config{
		DataDir:       filepath.Join(tmp, "data"),
		AssetsDir:     assets,
		CacheSize:     2,
		DefaultDomain: "test.example",
	}
	store, err := state.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	runner := &testutil.FakeRunner{Stamp: pipeline.DepsStamp}
	pipe, err := pipeline.New(assets, runner)
	if err != nil {
		t.Fatal(err)
	}
	// The gitops client gets a pass-through runner: FakeRunner echoes a
	// "$ cmd" banner into the writer even for git, which would corrupt the
	// captured ls-remote output. FakeExec still runs the real git binary.
	b := New(cfg, store, gitops.NewClient(origin, "main", testutil.FakeExec{}), pipe)
	ctx, cancel := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() { b.RunWorker(ctx); close(workerDone) }()
	// Stop the worker before t.TempDir removal: a live worker racing TempDir's
	// RemoveAll makes the cleanup flake with "directory not empty".
	defer func() {
		cancel()
		<-workerDone
	}()

	// First check: clone, first test build, swap to copy b.
	if _, err := b.CheckNow(ctx); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return store.Get().AvailableCommit == c1 }, "first test build done")
	if st := store.Get(); st.Current != "b" {
		t.Errorf("current = %q, want b", st.Current)
	}
	copyB := filepath.Join(cfg.DataDir, "work", "repo-b")
	app, err := os.ReadFile(filepath.Join(copyB, "src", "app.js"))
	if err != nil || !strings.Contains(string(app), "['test.example']") {
		t.Errorf("patch+substitution missing in copy b: %q (%v)", app, err)
	}
	ov, err := os.ReadFile(filepath.Join(copyB, "public", "overlay.js"))
	if err != nil || !strings.Contains(string(ov), "test.example") {
		t.Errorf("overlay substitution missing: %q (%v)", ov, err)
	}

	// Second commit: benign change, patch still applies -> swap back to a.
	testutil.WriteFile(t, origin, "NOTES.md", "v2\n")
	c2 := testutil.CommitAll(t, origin, "c2")
	if _, err := b.CheckNow(ctx); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return store.Get().AvailableCommit == c2 }, "second test build done")
	if st := store.Get(); st.Current != "a" {
		t.Errorf("current = %q, want a (swapped back)", st.Current)
	}
	// Ordered build runs on the current copy and succeeds; ordering right
	// after the store shows the swap must work — the runner is released
	// before the outcome is recorded.
	id, cached, err := b.OrderBuild("d.example")
	if err != nil || cached {
		t.Fatalf("order = %q,%v,%v", id, cached, err)
	}
	waitBuildStatus(t, store, id, state.StatusSuccess)

	// Third commit breaks the patch -> bad_commit, available stays.
	testutil.WriteFile(t, origin, "src/app.js", "var changed = true;\n")
	c3 := testutil.CommitAll(t, origin, "c3")
	if _, err := b.CheckNow(ctx); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return store.Get().BadCommit == c3 }, "bad commit recorded")
	if st := store.Get(); st.AvailableCommit != c2 {
		t.Errorf("available = %q, want %q", st.AvailableCommit, c2)
	}
	// Repeat check of the same bad head: nothing to do; checking right after
	// the store shows bad_commit must work — the runner is released before
	// the outcome is recorded.
	if triggered, err := b.CheckNow(ctx); err != nil || triggered {
		t.Errorf("check = %v, %v; want false", triggered, err)
	}

	// Failed builds keep their logs for inspection (spec: the operator must be
	// able to read the test build log).
	st := store.Get()
	var lastTest state.BuildInfo
	for _, bd := range st.Builds {
		if bd.Kind == state.KindTest && bd.Status == state.StatusFailed {
			lastTest = bd
		}
	}
	if lastTest.ID == "" {
		t.Fatal("no failed test build recorded")
	}
	logf, err := b.OpenLog(lastTest.ID)
	if err != nil {
		t.Fatal("failed test build log missing")
	}
	defer logf.Close()
	raw, err := io.ReadAll(logf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "010-test.patch") {
		t.Errorf("log does not mention the failing patch:\n%s", raw)
	}
}
