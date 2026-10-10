//go:build e2e

package pipeline

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huhen/lampa-web-builder/internal/execrun"
	"github.com/huhen/lampa-web-builder/internal/testutil"
)

// TestFullBuildE2E runs the real recipe against the real upstream at the
// pinned commit with real npm/gulp. Run manually: make e2e.
func TestFullBuildE2E(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	const pin = "b4a13b6af7fe2f3bbbcb91f4eb434ab3378f8d5d"

	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	cmd := exec.Command("git", "clone", "-q", "https://github.com/yumata/lampa-source.git", repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("upstream clone failed (offline?): %v\n%s", err, out)
	}
	testutil.Git(t, repo, "checkout", "-q", pin)

	// Assets of this repository (run from the repo root).
	assets, err := filepath.Abs("../../")
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(assets, filepath.Join(tmp, "deps"), execrun.ExecRunner{})
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(tmp, "e2e.tar.gz")
	logf, err := os.Create(filepath.Join(tmp, "build.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logf.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := p.Run(ctx, repo, pin, "e2e.example", archive, logf); err != nil {
		t.Fatalf("e2e build failed (see %s): %v", logf.Name(), err)
	}

	// The archive must carry the app bundle with our domain substituted.
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
	var names []string
	var appJS strings.Builder
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
		// pack_github minifies dest/app.js into app.min.js and packs only that
		// bundle (public_github), so the domain substitution lands here.
		if hdr.Name == "app.min.js" {
			buf := &bytes.Buffer{}
			if _, err := io.Copy(buf, tr); err != nil {
				t.Fatal(err)
			}
			appJS.WriteString(buf.String())
		}
	}
	for _, want := range []string{"index.html", "app.min.js"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("archive misses %s; entries: %v", want, names)
		}
	}
	if !strings.Contains(appJS.String(), "e2e.example") {
		t.Error("app.min.js does not contain the substituted CUB domain")
	}
	if strings.Contains(appJS.String(), "{{CUB_DOMAIN}}") {
		t.Error("app.min.js still contains the raw {{CUB_DOMAIN}} token")
	}
}
