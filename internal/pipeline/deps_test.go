package pipeline

import (
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
