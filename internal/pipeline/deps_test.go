package pipeline

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestDepsEvictPlan(t *testing.T) {
	cases := []struct {
		name    string
		entries []depsEntry
		keep    int
		want    []string
	}{
		{
			"under the limit evicts nothing",
			[]depsEntry{
				{name: "b.lock.json", modTime: time.Unix(2, 0)},
				{name: "a.lock.json", modTime: time.Unix(1, 0)},
			},
			5,
			nil,
		},
		{
			"beyond the limit evicts the oldest first",
			[]depsEntry{
				{name: "b.lock.json", modTime: time.Unix(2, 0)},
				{name: "a.lock.json", modTime: time.Unix(1, 0)},
				{name: "c.lock.json", modTime: time.Unix(3, 0)},
			},
			2,
			[]string{"a.lock.json"},
		},
		{
			"keep zero evicts everything, oldest first",
			[]depsEntry{
				{name: "b.lock.json", modTime: time.Unix(2, 0)},
				{name: "a.lock.json", modTime: time.Unix(1, 0)},
			},
			0,
			[]string{"a.lock.json", "b.lock.json"},
		},
		{
			"equal mtimes break by name",
			[]depsEntry{
				{name: "b.lock.json", modTime: time.Unix(5, 0)},
				{name: "a.lock.json", modTime: time.Unix(5, 0)},
			},
			1,
			[]string{"b.lock.json"},
		},
		{"empty listing", nil, 3, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := depsEvictPlan(c.entries, c.keep)
			if !slices.Equal(got, c.want) {
				t.Errorf("depsEvictPlan = %v, want %v", got, c.want)
			}
		})
	}
}

func TestLoadAndStoreCachedLock(t *testing.T) {
	deps := t.TempDir()
	const key = "9f2c8a1b"

	if raw, ok, err := loadCachedLock(deps, key); err != nil || ok || raw != nil {
		t.Fatalf("miss on an empty cache = (%q, %v, %v), want (nil, false, nil)", raw, ok, err)
	}

	resolved := []byte("{\"lockfileVersion\":3}\n")
	pruned, err := storeCachedLock(deps, key, resolved)
	if err != nil || pruned != 0 {
		t.Fatalf("store = (%d, %v), want (0, nil)", pruned, err)
	}
	raw, ok, err := loadCachedLock(deps, key)
	if err != nil || !ok {
		t.Fatalf("load after store = (ok %v, err %v), want ok", ok, err)
	}
	if !bytes.Equal(raw, resolved) {
		t.Errorf("loaded = %q, want %q", raw, resolved)
	}
	// The store goes through a temp file: no leftovers in the cache dir.
	names, err := filepath.Glob(filepath.Join(deps, "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 {
		t.Errorf("cache dir = %v, want exactly the entry", names)
	}
}

func TestStoreCachedLockPrunesOldest(t *testing.T) {
	deps := t.TempDir()
	// More entries than the limit, with distinct mtimes so the order is defined.
	for i := 0; i < depsCacheKept+3; i++ {
		key := fmt.Sprintf("%08d", i)
		if _, err := storeCachedLock(deps, key, []byte("{}\n")); err != nil {
			t.Fatal(err)
		}
		when := time.Unix(int64(1000+i), 0)
		if err := os.Chtimes(depsLockPath(deps, key), when, when); err != nil {
			t.Fatal(err)
		}
	}
	names, err := filepath.Glob(filepath.Join(deps, "*.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != depsCacheKept {
		t.Fatalf("cache holds %d entries, want %d: %v", len(names), depsCacheKept, names)
	}
	for _, gone := range []string{"00000000", "00000001", "00000002"} {
		if _, ok, err := loadCachedLock(deps, gone); err != nil || ok {
			t.Errorf("entry %s = (ok %v, err %v), want pruned", gone, ok, err)
		}
	}
	newest := fmt.Sprintf("%08d", depsCacheKept+2)
	if _, ok, err := loadCachedLock(deps, newest); err != nil || !ok {
		t.Errorf("newest entry %s = (ok %v, err %v), want kept", newest, ok, err)
	}
}
