package main

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// listsFixture is the pinned copy of the lists modification.js is expected to
// mirror (issue #7). Update it in the same PR that changes lampa-go's config
// or the upstream manifest.js.
type listsFixture struct {
	LampaGo struct {
		Repo             string   `json:"repo"`
		Ref              string   `json:"ref"`
		Sha              string   `json:"sha"`
		Field            string   `json:"field"`
		SubdomainMarkers []string `json:"subdomain_markers"`
	} `json:"lampa_go"`
	Upstream struct {
		Repo       string   `json:"repo"`
		Ref        string   `json:"ref"`
		Sha        string   `json:"sha"`
		Field      string   `json:"field"`
		CubMirrors []string `json:"cub_mirrors"`
	} `json:"upstream"`
}

// jsList extracts the string literals of `var <name> = [...]` from src.
func jsList(t *testing.T, src, name string) []string {
	t.Helper()
	re := regexp.MustCompile(`var\s+` + name + `\s*=\s*\[([^\]]*)\]`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("var %s = [...] not found in modification.js", name)
	}
	if n := len(re.FindAllStringIndex(src, -1)); n != 1 {
		t.Fatalf("%s: declaration not unique in modification.js (%d matches)", name, n)
	}
	var out []string
	for _, part := range strings.Split(m[1], ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if len(part) < 2 || (part[0] != '\'' && part[0] != '"') || part[len(part)-1] != part[0] || len(part) == 2 {
			t.Fatalf("%s: unsupported literal %q", name, part)
		}
		out = append(out, part[1:len(part)-1])
	}
	return out
}

// sameSet reports whether both slices hold the same elements, ignoring order.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	return slices.Equal(as, bs)
}

// TestModificationListsMatchPinned mirrors issue #7: MARKERS/MIRRORS in
// overlay/public/plugins/modification.js are duplicated in lampa-go's config
// (subdomain markers) and upstream's src/core/manifest.js (cub_mirrors, which
// patch 020 replaces with {{CUB_DOMAIN}}). Drift silently routes traffic
// around the /cub/ proxy; this test is the cross-repo link.
func TestModificationListsMatchPinned(t *testing.T) {
	raw, err := os.ReadFile("overlay/public/plugins/modification.js")
	if err != nil {
		t.Fatal(err)
	}
	pinnedRaw, err := os.ReadFile("testdata/cub_lists.json")
	if err != nil {
		t.Fatal(err)
	}
	var pinned listsFixture
	if err := json.Unmarshal(pinnedRaw, &pinned); err != nil {
		t.Fatalf("testdata/cub_lists.json: %v", err)
	}

	if got, want := jsList(t, string(raw), "MARKERS"), pinned.LampaGo.SubdomainMarkers; !sameSet(got, want) {
		t.Errorf("MARKERS = %v, want %v (%s@%s, %s); update testdata/cub_lists.json in the same PR that changes lampa-go",
			got, want, pinned.LampaGo.Repo, pinned.LampaGo.Ref, pinned.LampaGo.Field)
	}
	if got, want := jsList(t, string(raw), "MIRRORS"), pinned.Upstream.CubMirrors; !sameSet(got, want) {
		t.Errorf("MIRRORS = %v, want %v (%s@%s, %s); update testdata/cub_lists.json in the same PR that changes the upstream list",
			got, want, pinned.Upstream.Repo, pinned.Upstream.Ref, pinned.Upstream.Field)
	}
}

// TestJSListAndSameSet pins the extractor's own behavior so a later
// simplification of jsList cannot silently weaken the link (issue #7).
func TestJSListAndSameSet(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			"single line",
			"var X = ['a', 'b'];\n",
			[]string{"a", "b"},
		},
		{
			"multi line and trailing comma",
			"var X = [\n\t'tmdb',\n\t'geo',\n];\n",
			[]string{"tmdb", "geo"},
		},
		{
			"double quotes",
			`var X = ["cub.best", "cub.black"];`,
			[]string{"cub.best", "cub.black"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := jsList(t, c.src, "X")
			if !sameSet(got, c.want) {
				t.Errorf("jsList = %v, want %v", got, c.want)
			}
		})
	}

	if !sameSet([]string{"a", "b"}, []string{"b", "a"}) {
		t.Error("sameSet must ignore order")
	}
	if sameSet([]string{"a"}, []string{"a", "b"}) {
		t.Error("sameSet must compare length")
	}
	if sameSet([]string{"a", "a"}, []string{"a", "b"}) {
		t.Error("sameSet must catch a duplicate in place of a different element")
	}
}
