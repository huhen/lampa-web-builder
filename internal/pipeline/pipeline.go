// Package pipeline implements the build recipe (spec §6): checkout, patches,
// overlay, deps, placeholders, gulp build, archive. git/npm/gulp run as
// external processes with all output captured into the build log.
package pipeline

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/huhen/lampa-web-builder/internal/execrun"
)

// BuildRunner is the build pipeline interface the orchestrator depends on.
type BuildRunner interface {
	Run(ctx context.Context, workDir, commit, domain, archivePath string, log io.Writer) error
}

// Pipeline executes the recipe on working copies using assetsDir
// (patches/, overlay/, package-lock.json) and freezes resolved dependency
// lockfiles in depsDir.
type Pipeline struct {
	AssetsDir string // absolute
	DepsDir   string // absolute; one .lock.json per (manifest, seed) key
	Runner    execrun.Runner
}

// New resolves assetsDir and depsDir to absolute paths (commands run with a
// different working directory, so relative asset paths would break).
func New(assetsDir, depsDir string, r execrun.Runner) (*Pipeline, error) {
	if depsDir == "" {
		// An empty deps dir would resolve to the process working directory and
		// scatter cache entries into it.
		return nil, errors.New("deps dir is required")
	}
	absAssets, err := filepath.Abs(assetsDir)
	if err != nil {
		return nil, err
	}
	absDeps, err := filepath.Abs(depsDir)
	if err != nil {
		return nil, err
	}
	return &Pipeline{AssetsDir: absAssets, DepsDir: absDeps, Runner: r}, nil
}

// Run executes the whole recipe on workDir for (commit, domain) and writes
// the archive to archivePath. Every step is logged; the first failure aborts.
//
// The caller owns log and must close it when done (if it needs closing).
// Run is not reentrant on the same workDir; different working copies may be
// built concurrently — the orchestrator serializes runs per copy.
func (p *Pipeline) Run(ctx context.Context, workDir, commit, domain, archivePath string, log io.Writer) error {
	step := func(name string, fn func() error) error {
		fmt.Fprintf(log, "\n==> %s\n", name)
		if err := fn(); err != nil {
			fmt.Fprintf(log, "==> %s failed: %v\n", name, err)
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	}

	if err := step("checkout", func() error { return p.stepCheckout(ctx, workDir, commit, log) }); err != nil {
		return err
	}
	var patches []appliedPatch
	if err := step("patches", func() error {
		var err error
		patches, err = p.stepPatches(ctx, workDir, log)
		return err
	}); err != nil {
		return err
	}
	var overlayFiles []string
	if err := step("overlay", func() error {
		var err error
		overlayFiles, err = p.stepOverlay(workDir, log)
		return err
	}); err != nil {
		return err
	}
	if err := step("deps", func() error { return p.stepDeps(ctx, workDir, log) }); err != nil {
		return err
	}
	if err := step("placeholders", func() error {
		sources, touched, err := placeholderInputs(workDir, patches, overlayFiles)
		if err != nil {
			return err
		}
		// Guard first: a malformed token fails the build before any
		// substitution is written into the working copy.
		if err := CheckMalformedTokens(sources); err != nil {
			return err
		}
		return SubstitutePlaceholders(workDir, touched, map[string]string{"CUB_DOMAIN": domain})
	}); err != nil {
		return err
	}
	if err := step("build", func() error { return p.stepBuild(ctx, workDir, log) }); err != nil {
		return err
	}
	return step("archive", func() error { return p.stepArchive(workDir, archivePath, log) })
}

// run executes commands in dir, logging each command line and its output.
func (p *Pipeline) run(ctx context.Context, log io.Writer, dir string, cmds ...[]string) error {
	for _, c := range cmds {
		fmt.Fprintln(log, "$ "+strings.Join(c, " "))
		if err := p.Runner.Run(ctx, dir, log, c[0], c[1:]...); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(c, " "), err)
		}
	}
	return nil
}

func (p *Pipeline) stepCheckout(ctx context.Context, workDir, commit string, log io.Writer) error {
	// build/ and dest/ are gitignored upstream, so git clean -fd keeps them;
	// drop them explicitly or stale artifacts leak between builds.
	for _, d := range []string{"build", "dest"} {
		if err := os.RemoveAll(filepath.Join(workDir, d)); err != nil {
			return err
		}
	}
	return p.run(ctx, log, workDir,
		[]string{"git", "checkout", "-f", commit},
		[]string{"git", "clean", "-fd"},
	)
}

// stepPatches applies patches/NNN-*.patch in lexical order; the first patch
// that does not apply fails the build (git output is already in the log).
func (p *Pipeline) stepPatches(ctx context.Context, workDir string, log io.Writer) ([]appliedPatch, error) {
	matches, err := filepath.Glob(filepath.Join(p.AssetsDir, "patches", "*.patch"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	var applied []appliedPatch
	for _, patchPath := range matches {
		name := filepath.Base(patchPath)
		fmt.Fprintf(log, "==> patch %s\n", name)
		raw, err := os.ReadFile(patchPath)
		if err != nil {
			return nil, err
		}
		if err := p.run(ctx, log, workDir, []string{"git", "apply", "--check", patchPath}); err != nil {
			return nil, fmt.Errorf("patch %s does not apply: %w", name, err)
		}
		if err := p.run(ctx, log, workDir, []string{"git", "apply", patchPath}); err != nil {
			return nil, fmt.Errorf("patch %s failed: %w", name, err)
		}
		applied = append(applied, appliedPatch{name: name, raw: raw, files: PatchFiles(raw)})
	}
	return applied, nil
}

// stepOverlay copies overlay/ recursively over the tree; returns the relative
// paths of the copied files.
func (p *Pipeline) stepOverlay(workDir string, log io.Writer) ([]string, error) {
	root := filepath.Join(p.AssetsDir, "overlay")
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return copyFile(path, filepath.Join(workDir, rel))
	})
	if err != nil {
		return nil, err
	}
	for _, rel := range files {
		fmt.Fprintf(log, "+ %s\n", rel)
	}
	return files, nil
}

// appliedPatch is a patch this build applied: its file name, raw bytes (for
// the malformed-token guard) and the files it touches (for substitution).
type appliedPatch struct {
	name  string
	raw   []byte
	files []string
}

// placeholderInputs assembles what the placeholders step needs: the guard
// sources (added patch lines + whole overlay files) and the de-duplicated list
// of files to substitute.
func placeholderInputs(workDir string, patches []appliedPatch, overlayFiles []string) ([]PlaceholderSource, []string, error) {
	var sources []PlaceholderSource
	var touched []string
	seen := map[string]bool{}
	for _, ap := range patches {
		sources = append(sources, PatchSources(ap.name, ap.raw))
		for _, f := range ap.files {
			if !seen[f] {
				seen[f] = true
				touched = append(touched, f)
			}
		}
	}
	for _, rel := range overlayFiles {
		raw, err := os.ReadFile(filepath.Join(workDir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, nil, err
		}
		// Overlay files are ours entirely, so the whole file is a guard
		// source; the name keeps the "overlay/" prefix for diagnostics.
		sources = append(sources, PlaceholderSource{
			Name:  "overlay/" + rel,
			Lines: strings.Split(string(raw), "\n"),
		})
		if !seen[rel] {
			seen[rel] = true
			touched = append(touched, rel)
		}
	}
	return sources, touched, nil
}

// DepsStamp is the node_modules freshness stamp: md5(package.json + lockfile),
// mirroring the stamp lampa-go's update-frontend.sh used.
func DepsStamp(packageJSON, lockJSON []byte) string {
	h := md5.New()
	h.Write(packageJSON)
	h.Write(lockJSON)
	return hex.EncodeToString(h.Sum(nil))
}

// stampShort shortens a stamp value for single-line log diagnostics.
func stampShort(raw []byte) string {
	v := strings.TrimSpace(string(raw))
	if len(v) > 8 {
		return v[:8]
	}
	return v
}

// stepDeps installs npm dependencies from the pinned lockfile, skipping the
// install entirely when node_modules is fresh (stamp matches).
func (p *Pipeline) stepDeps(ctx context.Context, workDir string, log io.Writer) error {
	lockRaw, err := os.ReadFile(filepath.Join(p.AssetsDir, "package-lock.json"))
	if err != nil {
		return fmt.Errorf("read pinned lockfile: %w", err)
	}
	pkgRaw, err := os.ReadFile(filepath.Join(workDir, "package.json"))
	if err != nil {
		return err
	}
	// Upstream ships no lockfile (it is gitignored), feed ours to npm.
	if err := os.WriteFile(filepath.Join(workDir, "package-lock.json"), lockRaw, 0o644); err != nil {
		return err
	}
	stampPath := filepath.Join(workDir, "node_modules", ".fe-lock-stamp")
	stamp, _ := os.ReadFile(stampPath)
	want := DepsStamp(pkgRaw, lockRaw)
	if strings.TrimSpace(string(stamp)) == want {
		fmt.Fprintln(log, "node_modules is fresh (stamp matches), skipping npm install")
		return nil
	}
	fmt.Fprintf(log, "node_modules stamp mismatch (have %q, want %q) — reinstalling\n",
		stampShort(stamp), stampShort([]byte(want)))
	if err := p.run(ctx, log, workDir, []string{"npm", "ci", "--no-audit", "--no-fund"}); err != nil {
		// Re-resolve only on a real lockfile/package.json desync, not on any
		// npm ci failure (a network error must not silently drift the pin).
		fmt.Fprintln(log, "npm ci failed — checking whether the lockfile is out of sync")
		// The probe is called directly via the Runner, so it does not print
		// its own "$ ..." line — log the command banner here.
		fmt.Fprintln(log, "$ npm ci --dry-run --no-audit --no-fund")
		if dry := p.Runner.Run(ctx, workDir, log, "npm", "ci", "--dry-run", "--no-audit", "--no-fund"); dry == nil {
			return fmt.Errorf("npm ci failed but the lockfile is in sync, see the log above")
		}
		fmt.Fprintln(log, "WARN: lockfile out of sync with upstream package.json — re-resolving (npm install)")
		if err := os.WriteFile(filepath.Join(workDir, "package-lock.json.bak"), lockRaw, 0o644); err != nil {
			return err
		}
		if err := p.run(ctx, log, workDir, []string{"npm", "install", "--no-audit", "--no-fund"}); err != nil {
			return fmt.Errorf("npm install failed: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(stampPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(stampPath, []byte(want), 0o644)
}

func (p *Pipeline) stepBuild(ctx context.Context, workDir string, log io.Writer) error {
	// lampa_go_build comes from our 010-gulp-build-task.patch (upstream has no
	// non-interactive build task); pack_github needs its dest/app.js.
	if err := p.run(ctx, log, workDir,
		[]string{"npx", "gulp", "lampa_go_build"},
		[]string{"npx", "gulp", "pack_github"},
	); err != nil {
		return err
	}
	out := filepath.Join(workDir, "build", "github", "lampa")
	if info, err := os.Stat(out); err != nil || !info.IsDir() {
		return fmt.Errorf("build produced no output at build/github/lampa")
	}
	return nil
}

// stepArchive packs the contents of build/github/lampa/ into archivePath with
// no wrapper directory — convenient for unpacking into versions/<commit>/.
func (p *Pipeline) stepArchive(workDir, archivePath string, log io.Writer) error {
	src := filepath.Join(workDir, "build", "github", "lampa")
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	var files int
	walkErr := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		} else if !info.Mode().IsRegular() {
			return nil // archives carry regular files only
		} else {
			files++
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})
	if walkErr == nil {
		walkErr = tw.Close()
	}
	if walkErr == nil {
		walkErr = gz.Close()
	}
	if walkErr == nil {
		walkErr = f.Close()
	} else {
		f.Close()
	}
	if walkErr != nil {
		os.Remove(archivePath) // never leave a partial archive behind
		return walkErr
	}
	// gulp "succeeded" but packed nothing: an empty archive would look like a
	// valid build downstream, so treat it as a failure.
	if files == 0 {
		os.Remove(archivePath)
		return fmt.Errorf("build output is empty (no files in build/github/lampa)")
	}
	fmt.Fprintf(log, "archive written: %s\n", archivePath)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
