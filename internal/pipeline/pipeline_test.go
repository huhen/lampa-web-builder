package pipeline

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/huhen/lampa-web-builder/internal/testutil"
)

// fixtureRepo creates a git repo imitating the upstream layout and an assets
// dir with one patch, an overlay file and a pinned lockfile.
func fixtureRepo(t *testing.T) (repo, assets, c1 string) {
	t.Helper()
	repo = t.TempDir()
	assets = t.TempDir()
	testutil.InitGitRepo(t, repo)
	// Upstream ignores node_modules/, so git clean -fd keeps it (and the
	// freshness stamp inside) between builds.
	testutil.WriteFile(t, repo, ".gitignore", "node_modules/\n")
	testutil.WriteFile(t, repo, "src/app.js", "var a = 1;\n")
	testutil.WriteFile(t, repo, "package.json", `{"name":"lampa","version":"0.0.1"}`+"\n")
	c1 = testutil.CommitAll(t, repo, "initial")

	testutil.WriteFile(t, assets, "patches/010-test.patch", ""+
		"diff --git a/src/app.js b/src/app.js\n"+
		"--- a/src/app.js\n"+
		"+++ b/src/app.js\n"+
		"@@ -1 +1,2 @@\n"+
		" var a = 1;\n"+
		"+var cub = ['{{CUB_DOMAIN}}'];\n")
	testutil.WriteFile(t, assets, "overlay/public/overlay.js", "// mirror {{CUB_DOMAIN}}\n")
	testutil.WriteFile(t, assets, "package-lock.json", `{"lockfile":true}`+"\n")
	return repo, assets, c1
}

// newPipeline builds a pipeline whose deps cache lives in a throwaway dir;
// tests that assert on the cache use newPipelineDeps with their own.
func newPipeline(t *testing.T, assets string) (*Pipeline, *testutil.FakeRunner) {
	t.Helper()
	return newPipelineDeps(t, assets, t.TempDir())
}

func newPipelineDeps(t *testing.T, assets, deps string) (*Pipeline, *testutil.FakeRunner) {
	t.Helper()
	fake := &testutil.FakeRunner{Stamp: DepsStamp}
	p, err := New(assets, deps, fake)
	if err != nil {
		t.Fatal(err)
	}
	return p, fake
}

// An empty deps dir would resolve to the process working directory and scatter
// cache entries into it, so New rejects it outright.
func TestNewRejectsEmptyDepsDir(t *testing.T) {
	if _, err := New(t.TempDir(), "", &testutil.FakeRunner{}); err == nil {
		t.Error("empty deps dir must be rejected")
	}
}

func TestPipelineRunFull(t *testing.T) {
	repo, assets, c1 := fixtureRepo(t)
	p, _ := newPipeline(t, assets)
	archive := filepath.Join(t.TempDir(), "b1", "archive.tar.gz")
	log := &bytes.Buffer{}

	if err := p.Run(context.Background(), repo, c1, "test.example", archive, log); err != nil {
		t.Fatalf("run: %v\nlog:\n%s", err, log)
	}

	// Archive: the contents of build/github/lampa/ at the tar root.
	f, err := os.Open(archive)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	var names []string
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
	if len(names) != 1 || names[0] != "index.html" {
		t.Errorf("tar entries = %v, want [index.html]", names)
	}

	// Placeholders substituted in patched and overlay files.
	app, _ := os.ReadFile(filepath.Join(repo, "src", "app.js"))
	if !strings.Contains(string(app), "['test.example']") {
		t.Errorf("patched file not substituted: %s", app)
	}
	ov, _ := os.ReadFile(filepath.Join(repo, "public", "overlay.js"))
	if !strings.Contains(string(ov), "test.example") {
		t.Errorf("overlay file not substituted: %s", ov)
	}
	// Deps installed with the pinned lockfile.
	stamp, err := os.ReadFile(filepath.Join(repo, "node_modules", ".fe-lock-stamp"))
	if err != nil {
		t.Fatalf("no stamp: %v", err)
	}
	want := DepsStamp([]byte("{\"name\":\"lampa\",\"version\":\"0.0.1\"}\n"), []byte("{\"lockfile\":true}\n"))
	if string(stamp) != want {
		t.Errorf("stamp = %q, want %q", stamp, want)
	}
	if !strings.Contains(log.String(), "==> patches") || !strings.Contains(log.String(), "==> archive") {
		t.Errorf("steps not logged:\n%s", log)
	}
}

func TestPipelineRunSecondTimeSkipsNpm(t *testing.T) {
	repo, assets, c1 := fixtureRepo(t)
	p, fake := newPipeline(t, assets)
	log := &bytes.Buffer{}
	if err := p.Run(context.Background(), repo, c1, "test.example", filepath.Join(t.TempDir(), "a.tar.gz"), log); err != nil {
		t.Fatal(err)
	}
	callsAfterFirst := len(fake.CallsSnapshot())
	if err := p.Run(context.Background(), repo, c1, "other.example", filepath.Join(t.TempDir(), "b.tar.gz"), log); err != nil {
		t.Fatal(err)
	}
	calls := fake.CallsSnapshot()
	for _, c := range calls[callsAfterFirst:] {
		if strings.HasPrefix(c, "npm ci") || strings.HasPrefix(c, "npm install") {
			t.Errorf("npm ran again despite fresh stamp: %q", c)
		}
	}
}

func TestPipelinePatchConflict(t *testing.T) {
	repo, assets, _ := fixtureRepo(t)
	// New upstream commit changes the patched line -> patch must fail.
	testutil.WriteFile(t, repo, "src/app.js", "var a = 999;\n")
	testutil.CommitAll(t, repo, "conflict")
	p, _ := newPipeline(t, assets)
	log := &bytes.Buffer{}
	err := p.Run(context.Background(), repo, "HEAD", "test.example", filepath.Join(t.TempDir(), "a.tar.gz"), log)
	if err == nil {
		t.Fatal("expected patch failure")
	}
	if !strings.Contains(err.Error(), "patches") {
		t.Errorf("error must name the failing step, got: %v", err)
	}
	if !strings.Contains(log.String(), "does not apply") {
		t.Errorf("git output missing from log:\n%s", log)
	}
}

// A failed run leaves the first patch already applied (a dirty tree). The next
// run on the same copy must reset it via checkout -f + clean -fd and apply
// each patch exactly once (issue #12).
func TestRerunAfterPatchConflict(t *testing.T) {
	repo, assets, c1 := fixtureRepo(t)
	// 010 applies to c1; 020 cannot (src/other.js does not exist yet), so the
	// run fails after 010 has already dirtied the tree.
	testutil.WriteFile(t, assets, "patches/020-second.patch", ""+
		"diff --git a/src/other.js b/src/other.js\n"+
		"--- a/src/other.js\n"+
		"+++ b/src/other.js\n"+
		"@@ -1 +1,2 @@\n"+
		" var o = 2;\n"+
		"+var mirror = '{{CUB_DOMAIN}}';\n")
	p, _ := newPipeline(t, assets)
	log := &bytes.Buffer{}
	archive := filepath.Join(t.TempDir(), "a.tar.gz")

	err := p.Run(context.Background(), repo, c1, "test.example", archive, log)
	if err == nil || !strings.Contains(err.Error(), "020-second.patch") {
		t.Fatalf("first run must fail on the second patch, got: %v", err)
	}
	dirty, _ := os.ReadFile(filepath.Join(repo, "src", "app.js"))
	if !strings.Contains(string(dirty), "var cub") {
		t.Fatalf("the first patch should have dirtied the tree: %q", dirty)
	}

	// Upstream catches up: src/other.js now has the expected content. Stage
	// only the new file — the tree is still dirty from the failed run, and
	// CommitAll would bake the first patch into the "upstream" commit, which
	// an upstream push never contains.
	testutil.WriteFile(t, repo, "src/other.js", "var o = 2;\n")
	testutil.Git(t, repo, "add", "src/other.js")
	testutil.Git(t, repo, "commit", "-q", "-m", "add src/other.js")
	c2 := testutil.Git(t, repo, "rev-parse", "HEAD")

	if err := p.Run(context.Background(), repo, c2, "test.example", archive, log); err != nil {
		t.Fatalf("rerun: %v\nlog:\n%s", err, log)
	}
	app, _ := os.ReadFile(filepath.Join(repo, "src", "app.js"))
	if n := strings.Count(string(app), "var cub"); n != 1 {
		t.Errorf("patch applied %d times, want 1: %q", n, app)
	}
	other, _ := os.ReadFile(filepath.Join(repo, "src", "other.js"))
	if !strings.Contains(string(other), "test.example") {
		t.Errorf("second patch result missing: %q", other)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Errorf("archive after rerun: %v", err)
	}
}

// A run that fails after the placeholders step (gulp produces nothing) leaves
// substituted files behind; rerunning the same commit with a working runner
// must reset, succeed and produce identical content (issue #12).
func TestRerunAfterBuildFailure(t *testing.T) {
	repo, assets, c1 := fixtureRepo(t)
	broken := &testutil.FakeRunner{Stamp: DepsStamp, SilentGulp: true}
	p, err := New(assets, t.TempDir(), broken)
	if err != nil {
		t.Fatal(err)
	}
	log := &bytes.Buffer{}
	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := p.Run(context.Background(), repo, c1, "test.example", archive, log); err == nil {
		t.Fatal("the broken run must fail (no build output)")
	}
	dirty, err := os.ReadFile(filepath.Join(repo, "src", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dirty), "test.example") {
		t.Fatalf("placeholders should have rewritten the tree: %q", dirty)
	}

	// Same Pipeline, working runner: the checkout step must reset the tree.
	p.Runner = &testutil.FakeRunner{Stamp: DepsStamp}
	if err := p.Run(context.Background(), repo, c1, "test.example", archive, log); err != nil {
		t.Fatalf("rerun: %v\nlog:\n%s", err, log)
	}
	again, err := os.ReadFile(filepath.Join(repo, "src", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(dirty) {
		t.Errorf("content after rerun = %q, want %q", again, dirty)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Errorf("archive after rerun: %v", err)
	}
}

func TestPipelineLockfileDesyncResolves(t *testing.T) {
	repo, assets, _ := fixtureRepo(t)
	deps := t.TempDir()
	p, fake := newPipelineDeps(t, assets, deps)
	// The preset fails both the install and the --dry-run probe, so npm ci
	// fails for a desync and the pipeline re-resolves.
	fake.FailNpmCI = testutil.NpmCIFailAlways
	pkg := []byte("{\"name\":\"lampa\",\"version\":\"0.0.2\"}\n")
	testutil.WriteFile(t, repo, "package.json", string(pkg))
	testutil.CommitAll(t, repo, "bump deps")
	log := &bytes.Buffer{}
	if err := p.Run(context.Background(), repo, "HEAD", "test.example", filepath.Join(t.TempDir(), "a.tar.gz"), log); err != nil {
		t.Fatalf("run: %v\nlog:\n%s", err, log)
	}
	var hasInstall bool
	for _, c := range fake.CallsSnapshot() {
		if strings.HasPrefix(c, "npm install") {
			hasInstall = true
		}
	}
	if !hasInstall {
		t.Errorf("re-resolve (npm install) did not run, calls: %v", fake.CallsSnapshot())
	}
	if _, err := os.Stat(filepath.Join(repo, "package-lock.json.bak")); err != nil {
		t.Errorf("no lockfile backup: %v", err)
	}
	if !strings.Contains(log.String(), "re-resolving") {
		t.Errorf("warning missing from log:\n%s", log)
	}

	// The resolution is frozen: it is what npm was given and what the cache
	// now holds, so the next build never resolves again.
	installed, err := os.ReadFile(filepath.Join(repo, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := os.ReadFile(filepath.Join(assets, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(installed, seed) {
		t.Fatal("npm install was expected to rewrite the lockfile")
	}
	cached, ok, err := loadCachedLock(deps, DepsStamp(pkg, seed))
	if err != nil || !ok {
		t.Fatalf("cold resolve must freeze the lockfile: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(cached, installed) {
		t.Errorf("cached = %q, want the resolved %q", cached, installed)
	}
}

// After a cold resolve the frozen entry is the effective lockfile, and the
// stamp written for it makes the next build skip the install entirely.
func TestPipelineSecondBuildSkipsInstallAfterResolve(t *testing.T) {
	repo, assets, _ := fixtureRepo(t)
	deps := t.TempDir()
	p, fake := newPipelineDeps(t, assets, deps)
	fake.FailNpmCI = testutil.NpmCIFailAlways
	testutil.WriteFile(t, repo, "package.json", "{\"name\":\"lampa\",\"version\":\"0.0.2\"}\n")
	testutil.CommitAll(t, repo, "bump deps")
	if err := p.Run(context.Background(), repo, "HEAD", "test.example", filepath.Join(t.TempDir(), "a.tar.gz"), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}

	// A working npm now: the entry is in sync, so npm ci would succeed — the
	// point is that neither it nor npm install runs at all.
	fake.FailNpmCI = nil
	before := len(fake.CallsSnapshot())
	log := &bytes.Buffer{}
	if err := p.Run(context.Background(), repo, "HEAD", "test.example", filepath.Join(t.TempDir(), "b.tar.gz"), log); err != nil {
		t.Fatalf("second build: %v\nlog:\n%s", err, log)
	}
	for _, c := range fake.CallsSnapshot()[before:] {
		if strings.HasPrefix(c, "npm ci") || strings.HasPrefix(c, "npm install") {
			t.Errorf("second build must skip the install, calls: %v", fake.CallsSnapshot()[before:])
		}
	}
	if !strings.Contains(log.String(), "cached lockfile") {
		t.Errorf("second build must use the frozen lockfile:\n%s", log)
	}
}

// npm ci failed for a reason other than a desync (the --dry-run probe passes):
// the build must fail without re-resolving the lockfile — a silent npm install
// would drift the pin (issue #12).
func TestNpmCIFailureWithInSyncLockfile(t *testing.T) {
	repo, assets, _ := fixtureRepo(t)
	p, fake := newPipeline(t, assets)
	fake.FailNpmCI = testutil.NpmCIFailDryRunPasses
	log := &bytes.Buffer{}
	err := p.Run(context.Background(), repo, "HEAD", "test.example", filepath.Join(t.TempDir(), "a.tar.gz"), log)
	if err == nil || !strings.Contains(err.Error(), "lockfile is in sync") {
		t.Fatalf("err = %v, want the in-sync failure\nlog:\n%s", err, log)
	}
	var hasProbe bool
	for _, c := range fake.CallsSnapshot() {
		if strings.HasPrefix(c, "npm install") {
			t.Errorf("re-resolve must not run: %q", c)
		}
		if strings.HasPrefix(c, "npm ci --dry-run") {
			hasProbe = true
		}
	}
	if !hasProbe {
		t.Errorf("desync probe did not run, calls: %v", fake.CallsSnapshot())
	}
	if _, err := os.Stat(filepath.Join(repo, "package-lock.json.bak")); !os.IsNotExist(err) {
		t.Errorf("lockfile backup must not be created: %v", err)
	}
}

// A malformed token in a patched file fails the build before any archive is
// written (issue #11); the error names the patch and the line in it.
func TestMalformedTokenInPatchFailsPipeline(t *testing.T) {
	repo, assets, c1 := fixtureRepo(t)
	testutil.WriteFile(t, assets, "patches/010-test.patch", ""+
		"diff --git a/src/app.js b/src/app.js\n"+
		"--- a/src/app.js\n"+
		"+++ b/src/app.js\n"+
		"@@ -1 +1,3 @@\n"+
		" var a = 1;\n"+
		"+var cub = ['{{ CUB_DOMAIN }}'];\n"+
		"+var ok = '{{CUB_DOMAIN}}';\n")
	p, _ := newPipeline(t, assets)
	log := &bytes.Buffer{}
	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	err := p.Run(context.Background(), repo, c1, "test.example", archive, log)
	if err == nil || !strings.Contains(err.Error(), "malformed placeholder") {
		t.Fatalf("expected a malformed-placeholder error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "patch 010-test.patch:6") {
		t.Errorf("error must name the patch and line, got: %v", err)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Errorf("no archive must be written for a failed build: %v", err)
	}
	// The guard must fail before substitution: the well-formed token in the
	// patched file is still un-substituted. (Asserting the malformed token
	// alone would be vacuous — tokenPattern never matches it.)
	app, err := os.ReadFile(filepath.Join(repo, "src", "app.js"))
	if err != nil {
		t.Fatalf("read patched file: %v", err)
	}
	if !strings.Contains(string(app), "{{CUB_DOMAIN}}") {
		t.Errorf("well-formed token must survive un-substituted:\n%s", app)
	}
	if strings.Contains(string(app), "test.example") {
		t.Errorf("substitution ran despite the guard:\n%s", app)
	}
}

// The same guard covers overlay files, which are copied whole.
func TestMalformedTokenInOverlayFailsPipeline(t *testing.T) {
	repo, assets, c1 := fixtureRepo(t)
	testutil.WriteFile(t, assets, "overlay/public/overlay.js", "// typo {{ CUB_DOMAIN }}\n// mirror {{CUB_DOMAIN}}\n")
	p, _ := newPipeline(t, assets)
	log := &bytes.Buffer{}
	err := p.Run(context.Background(), repo, c1, "test.example", filepath.Join(t.TempDir(), "a.tar.gz"), log)
	if err == nil || !strings.Contains(err.Error(), "overlay/public/overlay.js:1") {
		t.Fatalf("expected an overlay malformed-placeholder error, got: %v", err)
	}
	// The guard must fail before substitution: the well-formed token in the
	// copied overlay file is still un-substituted.
	ov, err := os.ReadFile(filepath.Join(repo, "public", "overlay.js"))
	if err != nil {
		t.Fatalf("read overlay file: %v", err)
	}
	if !strings.Contains(string(ov), "{{CUB_DOMAIN}}") {
		t.Errorf("well-formed token must survive un-substituted:\n%s", ov)
	}
	if strings.Contains(string(ov), "test.example") {
		t.Errorf("substitution ran despite the guard:\n%s", ov)
	}
}

func TestPipelineMissingBuildOutput(t *testing.T) {
	repo, assets, c1 := fixtureRepo(t)
	// SilentGulp: gulp "succeeds" but writes nothing -> the recipe must fail
	// with the missing-output error.
	fake := &testutil.FakeRunner{Stamp: DepsStamp, SilentGulp: true}
	p, err := New(assets, t.TempDir(), fake)
	if err != nil {
		t.Fatal(err)
	}
	log := &bytes.Buffer{}
	err = p.Run(context.Background(), repo, c1, "test.example", filepath.Join(t.TempDir(), "a.tar.gz"), log)
	if err == nil || !strings.Contains(err.Error(), "no output") {
		t.Fatalf("expected missing-output error, got: %v", err)
	}
}

func TestPipelineEmptyBuildOutputFails(t *testing.T) {
	repo, assets, c1 := fixtureRepo(t)
	// EmptyGulp: gulp "succeeds" but packs nothing -> an empty archive would
	// look like a valid build downstream, so the recipe must fail.
	fake := &testutil.FakeRunner{Stamp: DepsStamp, EmptyGulp: true}
	p, err := New(assets, t.TempDir(), fake)
	if err != nil {
		t.Fatal(err)
	}
	log := &bytes.Buffer{}
	err = p.Run(context.Background(), repo, c1, "test.example", filepath.Join(t.TempDir(), "a.tar.gz"), log)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-output error, got: %v\nlog:\n%s", err, log)
	}
}

// Archive properties: no wrapper directory, nested subdirectories, empty
// directories kept as entries, symlinks skipped, contents intact (issue #12).
func TestArchiveProperties(t *testing.T) {
	work := t.TempDir()
	src := filepath.Join(work, "build", "github", "lampa")
	testutil.WriteFile(t, src, "index.html", "<html>root</html>\n")
	testutil.WriteFile(t, src, "sub/deep/file.txt", "nested\n")
	if err := os.MkdirAll(filepath.Join(src, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("index.html", filepath.Join(src, "link.html")); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	p := &Pipeline{}
	if err := p.stepArchive(work, archive, &bytes.Buffer{}); err != nil {
		t.Fatalf("stepArchive: %v", err)
	}

	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()

	entries := map[string]*tar.Header{}
	contents := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries[hdr.Name] = hdr
		if hdr.Typeflag == tar.TypeReg {
			var buf bytes.Buffer
			if _, err := io.Copy(&buf, tr); err != nil {
				t.Fatal(err)
			}
			contents[hdr.Name] = buf.String()
		}
	}

	// The exact entry set: contents of build/github/lampa at the tar root,
	// with intermediate and empty directories kept as entries.
	want := []string{"empty/", "index.html", "sub/", "sub/deep/", "sub/deep/file.txt"}
	if got := entryNames(entries); !slices.Equal(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}

	if _, ok := entries["index.html"]; !ok {
		t.Errorf("index.html missing, entries: %v", entryNames(entries))
	}
	if contents["index.html"] != "<html>root</html>\n" {
		t.Errorf("index.html content = %q", contents["index.html"])
	}
	if _, ok := entries["sub/deep/file.txt"]; !ok {
		t.Errorf("nested file missing (slash-separated names expected), entries: %v", entryNames(entries))
	}
	if contents["sub/deep/file.txt"] != "nested\n" {
		t.Errorf("nested content = %q", contents["sub/deep/file.txt"])
	}
	hdr, ok := entries["empty/"]
	if !ok || hdr.Typeflag != tar.TypeDir {
		t.Errorf("empty dir must be a directory entry, entries: %v", entryNames(entries))
	}
	for name := range entries {
		if strings.Contains(name, "link.html") {
			t.Errorf("symlink must be skipped, got entry %q", name)
		}
	}
}

func entryNames(entries map[string]*tar.Header) []string {
	var out []string
	for n := range entries {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func TestPipelineUsesCachedLockfile(t *testing.T) {
	repo, assets, c1 := fixtureRepo(t)
	deps := t.TempDir()
	p, fake := newPipelineDeps(t, assets, deps)

	seed, err := os.ReadFile(filepath.Join(assets, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	pkg := []byte("{\"name\":\"lampa\",\"version\":\"0.0.1\"}\n")
	cached := []byte("{\"lockfileVersion\":3,\"frozen\":true}\n")
	if err := storeCachedLock(deps, DepsStamp(pkg, seed), cached); err != nil {
		t.Fatal(err)
	}

	log := &bytes.Buffer{}
	if err := p.Run(context.Background(), repo, c1, "test.example", filepath.Join(t.TempDir(), "a.tar.gz"), log); err != nil {
		t.Fatalf("run: %v\nlog:\n%s", err, log)
	}

	// The frozen entry is what npm was given — byte for byte.
	lock, err := os.ReadFile(filepath.Join(repo, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lock, cached) {
		t.Errorf("installed lockfile = %q, want the cached %q", lock, cached)
	}
	for _, c := range fake.CallsSnapshot() {
		if strings.HasPrefix(c, "npm install") {
			t.Errorf("a cache hit must not re-resolve: %v", fake.CallsSnapshot())
		}
	}
	if !strings.Contains(log.String(), "cached lockfile") {
		t.Errorf("log does not name the cached lockfile:\n%s", log)
	}
	// The stamp describes the cached tree, so the next build skips the install.
	stamp, err := os.ReadFile(filepath.Join(repo, "node_modules", ".fe-lock-stamp"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stamp) != DepsStamp(pkg, cached) {
		t.Errorf("stamp = %q, want %q", stamp, DepsStamp(pkg, cached))
	}
}

// A seed change is a deliberate pin bump: it must miss the cache and install
// the new pin instead of the entry frozen for the old one.
func TestPipelineSeedChangeMissesCache(t *testing.T) {
	repo, assets, c1 := fixtureRepo(t)
	deps := t.TempDir()
	p, _ := newPipelineDeps(t, assets, deps)

	seed, err := os.ReadFile(filepath.Join(assets, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	pkg := []byte("{\"name\":\"lampa\",\"version\":\"0.0.1\"}\n")
	cached := []byte("{\"lockfileVersion\":3,\"frozen\":true}\n")
	if err := storeCachedLock(deps, DepsStamp(pkg, seed), cached); err != nil {
		t.Fatal(err)
	}
	newSeed := "{\"lockfile\":true,\"bump\":1}\n"
	testutil.WriteFile(t, assets, "package-lock.json", newSeed)

	log := &bytes.Buffer{}
	if err := p.Run(context.Background(), repo, c1, "test.example", filepath.Join(t.TempDir(), "a.tar.gz"), log); err != nil {
		t.Fatalf("run: %v\nlog:\n%s", err, log)
	}
	lock, err := os.ReadFile(filepath.Join(repo, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(lock) != newSeed {
		t.Errorf("installed lockfile = %q, want the new seed", lock)
	}
	if !strings.Contains(log.String(), "pinned lockfile") {
		t.Errorf("log must name the pinned lockfile:\n%s", log)
	}
}

// A cache that cannot be read or written must cost a resolve, never a build.
func TestPipelineSurvivesUnusableDepsDir(t *testing.T) {
	repo, assets, c1 := fixtureRepo(t)
	// A file where the cache dir should be: reads and MkdirAll both fail, and
	// unlike a chmod this holds even for a root test run.
	blocked := filepath.Join(t.TempDir(), "deps")
	if err := os.WriteFile(blocked, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, _ := newPipelineDeps(t, assets, blocked)
	log := &bytes.Buffer{}
	if err := p.Run(context.Background(), repo, c1, "test.example", filepath.Join(t.TempDir(), "a.tar.gz"), log); err != nil {
		t.Fatalf("an unusable cache must not fail the build: %v\nlog:\n%s", err, log)
	}
	if !strings.Contains(log.String(), "deps cache read failed") {
		t.Errorf("expected a cache read warning:\n%s", log)
	}
}

// The same, on the path that tries to write: the resolve still succeeds.
func TestPipelineColdResolveWithUnusableDepsDir(t *testing.T) {
	repo, assets, _ := fixtureRepo(t)
	blocked := filepath.Join(t.TempDir(), "deps")
	if err := os.WriteFile(blocked, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, fake := newPipelineDeps(t, assets, blocked)
	fake.FailNpmCI = testutil.NpmCIFailAlways
	pkg := []byte("{\"name\":\"lampa\",\"version\":\"0.0.2\"}\n")
	testutil.WriteFile(t, repo, "package.json", string(pkg))
	testutil.CommitAll(t, repo, "bump deps")
	log := &bytes.Buffer{}
	if err := p.Run(context.Background(), repo, "HEAD", "test.example", filepath.Join(t.TempDir(), "a.tar.gz"), log); err != nil {
		t.Fatalf("a failed cache write must not fail the build: %v\nlog:\n%s", err, log)
	}
	if !strings.Contains(log.String(), "cannot cache the resolved lockfile") {
		t.Errorf("expected a cache write warning:\n%s", log)
	}
	// The failed cache write must not hide the truth: the stamp still describes
	// the tree npm actually installed, computed the way production does.
	resolved, err := os.ReadFile(filepath.Join(repo, "package-lock.json"))
	if err != nil {
		t.Fatalf("read resolved lockfile: %v", err)
	}
	seed, err := os.ReadFile(filepath.Join(assets, "package-lock.json"))
	if err != nil {
		t.Fatalf("read seed lockfile: %v", err)
	}
	if bytes.Equal(resolved, seed) {
		t.Fatalf("npm install was expected to rewrite the lockfile, got the seed")
	}
	stamp, err := os.ReadFile(filepath.Join(repo, "node_modules", ".fe-lock-stamp"))
	if err != nil {
		t.Fatalf("no stamp: %v", err)
	}
	if string(stamp) != DepsStamp(pkg, resolved) {
		t.Errorf("stamp = %q, want %q (the tree npm installed)", stamp, DepsStamp(pkg, resolved))
	}
}

// A corrupt entry makes npm ci fail; the pipeline resolves and replaces it
// rather than failing the build forever.
func TestPipelineReplacesCorruptCachedLock(t *testing.T) {
	repo, assets, c1 := fixtureRepo(t)
	deps := t.TempDir()
	p, fake := newPipelineDeps(t, assets, deps)

	seed, err := os.ReadFile(filepath.Join(assets, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	pkg := []byte("{\"name\":\"lampa\",\"version\":\"0.0.1\"}\n")
	key := DepsStamp(pkg, seed)
	const corrupt = "not json\n"
	if err := storeCachedLock(deps, key, []byte(corrupt)); err != nil {
		t.Fatal(err)
	}
	// npm ci cannot install the corrupt entry, so the probe fails too and the
	// pipeline resolves.
	fake.FailNpmCI = testutil.NpmCIFailAlways
	log := &bytes.Buffer{}
	if err := p.Run(context.Background(), repo, c1, "test.example", filepath.Join(t.TempDir(), "a.tar.gz"), log); err != nil {
		t.Fatalf("run: %v\nlog:\n%s", err, log)
	}
	raw, ok, err := loadCachedLock(deps, key)
	if err != nil || !ok {
		t.Fatalf("load after heal = (ok %v, err %v)", ok, err)
	}
	if string(raw) == corrupt {
		t.Error("the corrupt entry must be replaced by the fresh resolution")
	}
	installed, err := os.ReadFile(filepath.Join(repo, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, installed) {
		t.Errorf("cached = %q, want the resolved %q", raw, installed)
	}
}
