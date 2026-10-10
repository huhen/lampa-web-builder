package pipeline

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// tokenPattern matches {{NAME}} placeholders.
var tokenPattern = regexp.MustCompile(`\{\{([A-Za-z0-9_]+)\}\}`)

// malformedTokenPattern pins the fragment to quote in diagnostics: a stray
// "{{" plus the token-like characters that follow it (letters, digits,
// underscore, spaces and hyphens — the forms typos take) and an optional
// closing "}}" sitting right after them. Whatever tokenPattern does not
// match, this finds.
var malformedTokenPattern = regexp.MustCompile(`\{\{[A-Za-z0-9_ -]*(\}\})?`)

// patchFilePattern extracts target paths from `+++ b/<path>` headers.
var patchFilePattern = regexp.MustCompile(`(?m)^\+\+\+ b/(.+)$`)

// PatchFiles lists the files a git patch touches, in lexical order.
func PatchFiles(patch []byte) []string {
	var files []string
	seen := map[string]bool{}
	for _, m := range patchFilePattern.FindAllStringSubmatch(string(patch), -1) {
		p := unquotePath(strings.TrimSpace(m[1]))
		// git may append a tab and a timestamp after unusual names.
		if i := strings.IndexByte(p, '\t'); i >= 0 {
			p = p[:i]
		}
		if p == "" || p == "/dev/null" || seen[p] {
			continue
		}
		seen[p] = true
		files = append(files, p)
	}
	sort.Strings(files)
	return files
}

// unquotePath decodes a git C-quoted path: with core.quotePath on (the
// default) non-ASCII and control characters appear as `"src/\320\244.js"`.
// Anything that does not open a valid quoted literal is returned unchanged —
// a bogus header must not fail the build, only skip the file as before.
func unquotePath(p string) string {
	if !strings.HasPrefix(p, `"`) {
		return p
	}
	// The literal ends at the first unescaped closing quote; a tab+timestamp
	// suffix may follow it, so do not Unquote the whole line.
	end := -1
	for i := 1; i < len(p); i++ {
		if p[i] == '\\' {
			i++
			continue
		}
		if p[i] == '"' {
			end = i
			break
		}
	}
	if end < 0 {
		return p
	}
	s, err := strconv.Unquote(p[:end+1])
	if err != nil {
		return p
	}
	return s
}

// PlaceholderSource is newly added content scanned for malformed {{ tokens:
// the added lines of a patch (labelled with the patch file name) or a whole
// overlay file (labelled with its path). Lines are numbered from 1 in
// diagnostics; entries without added text are empty so the numbering stays
// aligned with the patch file.
type PlaceholderSource struct {
	Name  string
	Lines []string
}

// PatchSources returns the lines a patch adds ("+" lines, excluding the "+++"
// header), with empty entries for every other line so a reported line number
// matches the patch file.
func PatchSources(patchName string, raw []byte) PlaceholderSource {
	rawLines := strings.Split(string(raw), "\n")
	// A trailing newline does not start a new line.
	if rawLines[len(rawLines)-1] == "" {
		rawLines = rawLines[:len(rawLines)-1]
	}
	lines := make([]string, len(rawLines))
	for i, line := range rawLines {
		// Git always writes the new-file header as "+++ <path>" — marker plus
		// space. An added line is "+" followed directly by its content, so a
		// line like "+++z" (content "++z") is content, not a header.
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++ ") {
			lines[i] = line
		}
	}
	return PlaceholderSource{Name: "patch " + patchName, Lines: lines}
}

// CheckMalformedTokens reports the first malformed fragment on every line
// whose added content contains one — a `{{` that is not a well-formed
// {{NAME}} token. tokenPattern does not match such text, so without this
// guard a typo in a patch ({{ CUB_DOMAIN }}, {{CUB-DOMAIN}}, an unclosed
// {{CUB_DOMAIN) would reach users as a literal. Only added content is passed
// in: upstream context lines in patches are not ours to police. All
// violating lines are reported at once so typos can be fixed in one go.
func CheckMalformedTokens(sources []PlaceholderSource) error {
	var errs []error
	for _, src := range sources {
		for i, line := range src.Lines {
			// Strip the well-formed tokens (a literal replacement: nothing is
			// substituted); a "{{" that survives it starts a malformed token.
			rest := tokenPattern.ReplaceAllString(line, "")
			if m := malformedTokenPattern.FindString(rest); m != "" {
				errs = append(errs, fmt.Errorf(
					"%s:%d: malformed placeholder %q — tokens must be written as {{NAME}} (no spaces, only [A-Za-z0-9_])",
					src.Name, i+1, malformedSnippet(m)))
			}
		}
	}
	return errors.Join(errs...)
}

// malformedSnippet caps a malformed fragment for diagnostics at 40 runes so
// a very long broken line cannot flood the report.
func malformedSnippet(fragment string) string {
	r := []rune(fragment)
	if len(r) > 40 {
		r = append(r[:40:40], '…')
	}
	return string(r)
}

// SubstitutePlaceholders replaces every known {{TOKEN}} in the given files
// (paths relative to workDir, slash-separated). An unknown token in a touched
// file is a build error naming the file and the token — a guard against a
// forgotten mapping after a new placeholder is introduced. Files that do not
// exist (e.g. deleted by a patch) are skipped.
func SubstitutePlaceholders(workDir string, files []string, values map[string]string) error {
	for _, rel := range files {
		path := filepath.Join(workDir, filepath.FromSlash(rel))
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		var unknown []string
		replaced := tokenPattern.ReplaceAllStringFunc(string(raw), func(tok string) string {
			name := tok[2 : len(tok)-2]
			v, ok := values[name]
			if !ok {
				unknown = append(unknown, name)
				return tok
			}
			return v
		})
		if len(unknown) > 0 {
			return fmt.Errorf("%s: unknown placeholder(s) {{%s}} — add a mapping for it",
				rel, strings.Join(dedupStrings(unknown), "}}, {{"))
		}
		if replaced != string(raw) {
			if err := os.WriteFile(path, []byte(replaced), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func dedupStrings(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
