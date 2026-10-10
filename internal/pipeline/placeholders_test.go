package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchFiles(t *testing.T) {
	raw := []byte("" +
		"diff --git a/src/a.js b/src/a.js\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/src/a.js\n" +
		"+++ b/src/a.js\n" +
		"@@ -1 +1,2 @@\n" +
		" x\n" +
		"+y\n" +
		"diff --git a/b.js b/b.js\n" +
		"--- a/b.js\n" +
		"+++ b/b.js\t2026-10-07\n" + // trailing tab+timestamp must be trimmed
		"@@ -1 +1 @@\n" +
		"-old\n" +
		"+new\n")
	got := PatchFiles(raw)
	want := []string{"b.js", "src/a.js"}
	if len(got) != len(want) {
		t.Fatalf("PatchFiles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("PatchFiles = %v, want %v", got, want)
		}
	}
}

// C-quoted header edge cases (core.quotePath): a non-ASCII name arrives as
// "src/\320\244...", a literal quote in a name as \". An invalid escape
// inside a quoted literal must be kept verbatim — a bogus header must not
// fail the build, only skip the file as before (issue #11).
func TestPatchFilesUnquotesCQuoted(t *testing.T) {
	cases := []struct {
		name string
		line string // the `+++ b/` header line
		want string
	}{
		{"non-ascii", `+++ b/"src/\320\244.js"`, `src/Ф.js`},
		{"escaped quote", `+++ b/"a\"b.js"`, `a"b.js`},
		{"invalid escape kept verbatim", `+++ b/"\z"`, `"\z"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PatchFiles([]byte(tc.line + "\n"))
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("PatchFiles = %q, want %q", got, tc.want)
			}
		})
	}
}

// A quote that does not open a valid C-quoted literal must not break parsing:
// the (bogus) path is kept verbatim, as before.
func TestPatchFilesMalformedQuoteKept(t *testing.T) {
	raw := []byte("diff --git a/x b/x\n+++ b/\"unterminated\n")
	got := PatchFiles(raw)
	if len(got) != 1 || got[0] != `"unterminated` {
		t.Fatalf("PatchFiles = %q, want the raw path kept", got)
	}
}

// PatchSources keeps only added lines, and keeps their numbering aligned with
// the patch file so diagnostics point at a real line.
func TestPatchSources(t *testing.T) {
	raw := []byte("" +
		"diff --git a/src/a.js b/src/a.js\n" + // 1
		"--- a/src/a.js\n" + // 2
		"+++ b/src/a.js\n" + // 3
		"@@ -1 +1,2 @@\n" + // 4
		" var a = 1;\n" + // 5
		"+var b = 'x';\n") // 6
	src := PatchSources("010-x.patch", raw)
	if src.Name != "patch 010-x.patch" {
		t.Errorf("Name = %q, want patch 010-x.patch", src.Name)
	}
	if len(src.Lines) != 6 {
		t.Fatalf("len(Lines) = %d, want 6 (numbered like the patch file)", len(src.Lines))
	}
	if src.Lines[5] != "+var b = 'x';" {
		t.Errorf("Lines[5] = %q, want the added line", src.Lines[5])
	}
	for i, want := range []string{"", "", "", "", "", "+var b = 'x';"} {
		if src.Lines[i] != want {
			t.Errorf("Lines[%d] = %q, want %q (only added lines carry text)", i, src.Lines[i], want)
		}
	}
}

// An added line whose content starts with "++" appears as "+++z" in the
// patch; it is content, not a header, and must be kept. The real header is
// "+++ <path>" — marker plus space — and is still skipped (issue #11).
func TestPatchSourcesKeepsContentStartingWithPlusPlus(t *testing.T) {
	raw := []byte("" +
		"diff --git a/src/a.js b/src/a.js\n" + // 1
		"--- a/src/a.js\n" + // 2
		"+++ b/src/a.js\n" + // 3 — real header
		"@@ -1 +1 @@\n" + // 4
		"+++z\n") // 5 — added line, content "++z"
	src := PatchSources("010-x.patch", raw)
	if len(src.Lines) != 5 {
		t.Fatalf("len(Lines) = %d, want 5", len(src.Lines))
	}
	if src.Lines[2] != "" {
		t.Errorf("Lines[2] = %q, want \"\" (the +++ header must be skipped)", src.Lines[2])
	}
	if src.Lines[4] != "+++z" {
		t.Errorf("Lines[4] = %q, want \"+++z\" (added content must be kept)", src.Lines[4])
	}
}

func TestCheckMalformedTokens(t *testing.T) {
	// The malformed fragment is quoted with its surroundings, and the report
	// names the source and the line number.
	cases := []struct {
		name string
		line string
		want string
	}{
		{"spaces", "+var d = '{{ CUB_DOMAIN }}';", `malformed placeholder "{{ CUB_DOMAIN }}"`},
		{"hyphen", "+var d = '{{CUB-DOMAIN}}';", `malformed placeholder "{{CUB-DOMAIN}}"`},
		{"unclosed", "+var d = '{{CUB_DOMAIN';", `malformed placeholder "{{CUB_DOMAIN"`},
		{"doubled", "+var d = '{{{{CUB_DOMAIN}}';", `malformed placeholder "{{"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := PlaceholderSource{Name: "patch 010-x.patch", Lines: []string{"diff --git a/a b/a", c.line}}
			err := CheckMalformedTokens([]PlaceholderSource{src})
			if err == nil {
				t.Fatalf("expected an error for %q", c.line)
			}
			if !strings.Contains(err.Error(), "patch 010-x.patch:2") {
				t.Errorf("error must name source and line, got: %v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestCheckMalformedTokensClean(t *testing.T) {
	// Well-formed tokens, plain JS and empty filler lines are all fine.
	src := PlaceholderSource{Name: "overlay/public/overlay.js", Lines: []string{
		"// mirror {{CUB_DOMAIN}}",
		"",
		"var a = 1; // no braces here",
	}}
	if err := CheckMalformedTokens([]PlaceholderSource{src}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Upstream context ("-" and " " lines) is not our content: a literal {{ }} in
// it must never fail the build.
func TestCheckMalformedTokensIgnoresContext(t *testing.T) {
	raw := []byte("" +
		"diff --git a/src/a.js b/src/a.js\n" +
		"--- a/src/a.js\n" +
		"+++ b/src/a.js\n" +
		"@@ -1 +1 @@\n" +
		"-var tpl = '{{ not ours }}';\n" +
		"+var cub = '{{CUB_DOMAIN}}';\n")
	if err := CheckMalformedTokens([]PlaceholderSource{PatchSources("010-x.patch", raw)}); err != nil {
		t.Fatalf("upstream context must not be scanned: %v", err)
	}
}

// Every violating line is reported, not just the first.
func TestCheckMalformedTokensReportsAll(t *testing.T) {
	src := PlaceholderSource{Name: "overlay/x.js", Lines: []string{
		"a {{ BAD_ONE }}",
		"b {{BAD-TWO}}",
	}}
	err := CheckMalformedTokens([]PlaceholderSource{src})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"{{ BAD_ONE }}", "{{BAD-TWO}}"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %s", err, want)
		}
	}
}

// Violations from every source are joined into one error, each naming its
// own source and line, in source order.
func TestCheckMalformedTokensCrossSource(t *testing.T) {
	patch := PatchSources("010-x.patch", []byte(""+
		"diff --git a/src/a.js b/src/a.js\n"+ // 1
		"--- a/src/a.js\n"+ // 2
		"+++ b/src/a.js\n"+ // 3
		"@@ -1 +1 @@\n"+ // 4
		"+var d = '{{ BAD_A }}';\n")) // 5
	overlay := PlaceholderSource{Name: "overlay/x.js", Lines: []string{
		"var d = '{{BAD-B}}';", // 1
	}}
	err := CheckMalformedTokens([]PlaceholderSource{patch, overlay})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{
		`patch 010-x.patch:5: malformed placeholder "{{ BAD_A }}"`,
		`overlay/x.js:1: malformed placeholder "{{BAD-B}}"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
	// errors.Join must keep the source order.
	if i, j := strings.Index(err.Error(), "patch 010-x.patch:5"), strings.Index(err.Error(), "overlay/x.js:1"); i > j {
		t.Errorf("errors must keep source order, got: %v", err)
	}
}

// An empty token name is not a token: "{{}}" is malformed and is reported.
func TestCheckMalformedTokensEmptyToken(t *testing.T) {
	src := PlaceholderSource{Name: "overlay/x.js", Lines: []string{
		"var d = '{{}}';",
	}}
	err := CheckMalformedTokens([]PlaceholderSource{src})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "overlay/x.js:1") {
		t.Errorf("error must name source and line, got: %v", err)
	}
	if !strings.Contains(err.Error(), `malformed placeholder "{{}}"`) {
		t.Errorf("error must quote the empty token, got: %v", err)
	}
}

func TestSubstitutePlaceholders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "src", "core", "manifest.js")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("let lampa = ['{{CUB_DOMAIN}}']\nlet keep = 'plain'\n"), 0o644)

	err := SubstitutePlaceholders(dir, []string{"src/core/manifest.js"}, map[string]string{"CUB_DOMAIN": "d.example"})
	if err != nil {
		t.Fatalf("substitute: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "['d.example']") || !strings.Contains(string(raw), "'plain'") {
		t.Errorf("substitution wrong: %s", raw)
	}
}

func TestSubstituteUnknownTokenIsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "src/new.js")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("var x = '{{NEW_TOKEN}}';\n"), 0o644)

	err := SubstitutePlaceholders(dir, []string{"src/new.js"}, map[string]string{"CUB_DOMAIN": "d.example"})
	if err == nil {
		t.Fatal("expected error for unknown token")
	}
	if !strings.Contains(err.Error(), "src/new.js") || !strings.Contains(err.Error(), "NEW_TOKEN") {
		t.Errorf("error must name file and token, got: %v", err)
	}
}

func TestSubstituteMissingFileSkipped(t *testing.T) {
	dir := t.TempDir()
	// A deleted file (patch removed it) must not fail the build.
	if err := SubstitutePlaceholders(dir, []string{"gone.js"}, map[string]string{"CUB_DOMAIN": "d"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSubstituteNoTokensUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.js")
	os.WriteFile(path, []byte("var a = 1;\n"), 0o644)
	before, _ := os.ReadFile(path)
	if err := SubstitutePlaceholders(dir, []string{"plain.js"}, map[string]string{"CUB_DOMAIN": "d"}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("file rewritten without need")
	}
}
