# Кэш resolved lockfile по ключу (upstream manifest, seed) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Зафиксировать результат resolve зависимостей в `DATA_DIR/deps` по ключу `DepsStamp(upstream package.json, seed lockfile)`, чтобы для одинаковой пары (manifest, seed) везде ставился один и тот же набор версий.

**Architecture:** `stepDeps` выбирает эффективный lockfile — запись кэша, если она есть, иначе seed, — и ставит по нему через `npm ci`; stamp свежести `node_modules` считается от эффективного lockfile. После холодного resolve (`npm install`) результат атомарно сохраняется в кэш, поэтому следующий билд под тем же ключом идёт по замороженному lockfile. Кэш обрезается до `depsCacheKept` записей по `mtime`.

**Tech Stack:** Go 1.27 (stdlib only), `os/exec`; тесты — стандартный `testing` + `internal/testutil.FakeRunner` (фейковый npm без сети).

Спецификация: `docs/superpowers/specs/2026-10-10-deps-lock-cache-design.md`.

**Отклонения от spec, внесённые в план:**
1. Кэш-примитивы живут в новом `internal/pipeline/deps.go`, а не в `pipeline.go` (416 строк) — по образцу уже выделенного `placeholders.go` в том же пакете.
2. Лог-строка называет только ключ (`deps: cached lockfile 9f2c8a1b`) вместо пары «manifest + seed»: префиксы обоих хешей не дают операционной ценности, а ключ и есть их свёртка.
3. Если `npm ci` упал, а `npm install` в том же билде прошёл, запись кэша перезаписывается свежим resolve. Это узкое окно (npm ci строже npm install), результат самосогласован, но формально это не «first resolve wins».
4. Консолидация замечаний ревью (коммит `2cb6973`, вне плана): `storeCachedLock`
   разделён на запись и `pruneDepsCache(depsDir) (int, error)`; записи кэша
   пишутся с режимом 0644; неудачный prune логируется отдельным `WARN`, а не как
   ошибка записи; miss-строка называет пару manifest/seed. Кодовые блоки задач
   ниже, где они расходятся с этим, — исторические.
5. Документация вне задач плана: README-раздел «Зависимости фронтенда» и пункт в
   `CLAUDE.md` (коммиты `8b472c1`, `9be6dbd`, `ed59382`).

---

## File Structure

- **Create `internal/pipeline/deps.go`** — cache primitives: path, load, atomic store, eviction policy. Одна ответственность: где и как хранится зафиксированный resolve.
- **Create `internal/pipeline/deps_test.go`** — unit-тесты примитивов, включая чистую `depsEvictPlan`.
- **Modify `internal/pipeline/pipeline.go`** — `DepsDir` в `Pipeline`, `New` с новым параметром, `stepDeps` по новому потоку данных.
- **Modify `internal/pipeline/pipeline_test.go`** — правка call sites `New`, новые тесты cache hit / cold resolve / edge cases.
- **Modify `internal/testutil/runner.go`** — `npm install` перезаписывает lockfile, `npm ci` нет (как настоящий npm).
- **Modify `internal/testutil/testutil_test.go`** — тест на это поведение дубля.
- **Modify `main.go`, `internal/pipeline/e2e_test.go`, `internal/builder/integration_test.go`** — call sites `New`.
- **Modify `README.md`, `CLAUDE.md`** — документация решения.

---

### Task 1: Eviction policy для кэша зависимостей

**Files:**
- Create: `internal/pipeline/deps.go`
- Test: `internal/pipeline/deps_test.go`

- [ ] **Step 1: Написать падающий тест**

Создать `internal/pipeline/deps_test.go`:

```go
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
```

- [ ] **Step 2: Убедиться, что тест не компилируется**

```sh
go test ./internal/pipeline/ -run TestDepsEvictPlan
```

Ожидается: `FAIL ... undefined: depsEvictPlan`.

- [ ] **Step 3: Написать минимальную реализацию**

Создать `internal/pipeline/deps.go` (файл начинается сразу с `package pipeline` — как `placeholders.go`; пояснение к решению живёт в spec, а не в шапке файла, иначе комментарий перехватит package doc):

```go
package pipeline

import (
	"sort"
	"time"
)

// depsCacheKept is how many resolved lockfiles stay in the cache; the oldest by
// mtime are removed first. Deleting an entry is safe by design: the next build
// under that key resolves again.
const depsCacheKept = 20

// depsEntry is one cached resolved lockfile, as listed for eviction.
type depsEntry struct {
	name    string
	modTime time.Time
}

// depsEvictPlan returns the names to delete from a cache listing, oldest first,
// keeping the newest keep entries. Equal mtimes break by name so the plan is
// deterministic. The policy is pure — the same shape as builder.EvictPlan — so
// it is testable without a filesystem.
func depsEvictPlan(entries []depsEntry, keep int) []string {
	sorted := append([]depsEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].modTime.Equal(sorted[j].modTime) {
			return sorted[i].modTime.After(sorted[j].modTime)
		}
		return sorted[i].name < sorted[j].name
	})
	if keep < 0 {
		keep = 0
	}
	// Iterate the sorted slice from the tail so evicted names come back oldest
	// first (the newest keep entries are its prefix).
	var evict []string
	for i := len(sorted) - 1; i >= keep; i-- {
		evict = append(evict, sorted[i].name)
	}
	return evict
}
```

- [ ] **Step 4: Убедиться, что тест проходит**

```sh
go test ./internal/pipeline/ -run TestDepsEvictPlan -v
```

Ожидается: `PASS` по всем пяти подтестам.

- [ ] **Step 5: Commit**

```sh
git add internal/pipeline/deps.go internal/pipeline/deps_test.go
git commit -m "feat(pipeline): eviction policy кэша resolved lockfile"
```

---

### Task 2: load / store / prune кэша

**Files:**
- Modify: `internal/pipeline/deps.go`
- Test: `internal/pipeline/deps_test.go`

- [ ] **Step 1: Написать падающий тест**

Добавить в `internal/pipeline/deps_test.go` (импорты `bytes`, `fmt`, `os`, `path/filepath` к существующим):

```go
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
```

- [ ] **Step 2: Убедиться, что тесты не компилируются**

```sh
go test ./internal/pipeline/ -run 'TestLoadAndStoreCachedLock|TestStoreCachedLockPrunesOldest'
```

Ожидается: `FAIL ... undefined: loadCachedLock` (и аналогично для `storeCachedLock`, `depsLockPath`).

- [ ] **Step 3: Написать реализацию**

Дописать в `internal/pipeline/deps.go`, расширив импорты до `os`, `path/filepath`, `sort`, `time`:

```go
// depsLockPath is where the resolved lockfile for a DepsStamp key lives.
func depsLockPath(depsDir, key string) string {
	return filepath.Join(depsDir, key+".lock.json")
}

// loadCachedLock returns the resolved lockfile frozen for key. A missing file
// is a miss, not an error; any other read failure is returned so the caller can
// warn and fall back to the seed.
func loadCachedLock(depsDir, key string) (raw []byte, ok bool, err error) {
	raw, err = os.ReadFile(depsLockPath(depsDir, key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return raw, true, nil
}

// storeCachedLock freezes raw as the resolved lockfile for key and drops the
// oldest entries beyond depsCacheKept, returning how many it removed. The write
// goes through a temp file and rename so a crash cannot leave a torn entry that
// npm ci would later choke on (the same idiom as gitops.EnsureCopy, issue #10).
func storeCachedLock(depsDir, key string, raw []byte) (pruned int, err error) {
	if err := os.MkdirAll(depsDir, 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(depsDir, "tmp-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return 0, err
	}
	if err := os.Rename(tmpName, depsLockPath(depsDir, key)); err != nil {
		os.Remove(tmpName)
		return 0, err
	}
	evict, err := depsCacheEvictPlan(depsDir)
	if err != nil {
		return 0, err
	}
	for _, name := range evict {
		if err := os.Remove(filepath.Join(depsDir, name)); err != nil {
			return 0, err
		}
	}
	return len(evict), nil
}

// depsCacheEvictPlan lists the entries the cache must drop to stay within
// depsCacheKept, oldest first.
func depsCacheEvictPlan(depsDir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(depsDir, "*.lock.json"))
	if err != nil {
		return nil, err
	}
	entries := make([]depsEntry, 0, len(matches))
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // vanished under us; nothing to plan for it
			}
			return nil, err
		}
		entries = append(entries, depsEntry{name: filepath.Base(path), modTime: info.ModTime()})
	}
	return depsEvictPlan(entries, depsCacheKept), nil
}
```

Временные файлы создаются под именем `tmp-*` (без суффикса `.lock.json`), поэтому
glob `*.lock.json` их не видит и они не претендуют на место в лимите.

- [ ] **Step 4: Убедиться, что тесты проходят**

```sh
go test ./internal/pipeline/ -run 'TestDepsEvictPlan|TestLoadAndStoreCachedLock|TestStoreCachedLockPrunesOldest' -v
```

Ожидается: `PASS`.

- [ ] **Step 5: Commit**

```sh
git add internal/pipeline/deps.go internal/pipeline/deps_test.go
git commit -m "feat(pipeline): load, atomic store и prune кэша resolved lockfile"
```

---

### Task 3: Проводка DepsDir в Pipeline

**Files:**
- Modify: `internal/pipeline/pipeline.go` (структура `Pipeline`, `New`)
- Modify: `main.go:45`
- Modify: `internal/pipeline/e2e_test.go:46`
- Modify: `internal/builder/integration_test.go:58`
- Modify: `internal/pipeline/pipeline_test.go:45-53`, `:211`, `:372`, `:388`

- [ ] **Step 1: Написать падающий тест на пустой deps dir**

Добавить в `internal/pipeline/pipeline_test.go`:

```go
// An empty deps dir would resolve to the process working directory and scatter
// cache entries into it, so New rejects it outright.
func TestNewRejectsEmptyDepsDir(t *testing.T) {
	if _, err := New(t.TempDir(), "", &testutil.FakeRunner{}); err == nil {
		t.Error("empty deps dir must be rejected")
	}
}
```

- [ ] **Step 2: Убедиться, что тест падает**

```sh
go test ./internal/pipeline/ -run TestNewRejectsEmptyDepsDir
```

Ожидается: `FAIL` — `too many arguments in call to New` (на текущей сигнатуре).

- [ ] **Step 3: Поменять структуру и конструктор**

В `internal/pipeline/pipeline.go` заменить

```go
// Pipeline executes the recipe on working copies using assetsDir
// (patches/, overlay/, package-lock.json).
type Pipeline struct {
	AssetsDir string // absolute
	Runner    execrun.Runner
}

// New resolves assetsDir to an absolute path (commands run with a different
// working directory, so relative asset paths would break).
func New(assetsDir string, r execrun.Runner) (*Pipeline, error) {
	abs, err := filepath.Abs(assetsDir)
	if err != nil {
		return nil, err
	}
	return &Pipeline{AssetsDir: abs, Runner: r}, nil
}
```

на

```go
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
```

Добавить `"errors"` в импорт-блок `pipeline.go`.

- [ ] **Step 4: Обновить все call sites**

`main.go:45` — было `pipe, err := pipeline.New(cfg.AssetsDir, exe)`, стало:

```go
	pipe, err := pipeline.New(cfg.AssetsDir, filepath.Join(cfg.DataDir, "deps"), exe)
```

и добавить `"path/filepath"` в импорт-блок `main.go`.

`internal/pipeline/e2e_test.go:46` — было `p, err := New(assets, execrun.ExecRunner{})`, стало:

```go
	p, err := New(assets, filepath.Join(tmp, "deps"), execrun.ExecRunner{})
```

`internal/builder/integration_test.go:58` — было `pipe, err := pipeline.New(assets, runner)`, стало:

```go
	pipe, err := pipeline.New(assets, filepath.Join(cfg.DataDir, "deps"), runner)
```

`internal/pipeline/pipeline_test.go:45-53` — заменить хелпер целиком:

```go
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
```

`internal/pipeline/pipeline_test.go:211` — было `p, err := New(assets, broken)`, стало:

```go
	p, err := New(assets, t.TempDir(), broken)
```

`internal/pipeline/pipeline_test.go:372` и `:388` — было `p, err := New(assets, fake)`, стало (в обоих местах одинаково):

```go
	p, err := New(assets, t.TempDir(), fake)
```

- [ ] **Step 5: Убедиться, что весь пакет зелёный**

```sh
go build ./... && go vet ./... && go test ./...
```

Ожидается: сборка без ошибок, `ok` по всем пакетам. `stepDeps` пока не трогали — кэш не используется, поведение прежнее.

- [ ] **Step 6: Commit**

```sh
git add internal/pipeline/pipeline.go internal/pipeline/pipeline_test.go main.go \
        internal/pipeline/e2e_test.go internal/builder/integration_test.go
git commit -m "feat(pipeline): DepsDir в Pipeline и New"
```

---

### Task 4: FakeRunner имитирует перезапись lockfile на npm install

**Files:**
- Modify: `internal/testutil/runner.go` (`Run`, `fakeInstall`)
- Test: `internal/testutil/testutil_test.go`

- [ ] **Step 1: Написать падающий тест**

Добавить в `internal/testutil/testutil_test.go`:

```go
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
```

- [ ] **Step 2: Убедиться, что тест падает**

```sh
go test ./internal/testutil/ -run TestFakeRunnerInstallRewritesLockfile
```

Ожидается: `FAIL` — `npm install must rewrite the lockfile`.

- [ ] **Step 3: Реализовать**

В `internal/testutil/runner.go` в методе `Run` заменить ветки `case "ci"` / `case "install"`:

```go
		case "ci":
			if f.FailNpmCI != nil {
				if err := f.FailNpmCI(args); err != nil {
					return err
				}
			}
			// A --dry-run probe installs nothing, so it must not write the
			// freshness stamp either (real npm ci --dry-run touches nothing).
			for _, a := range args {
				if a == "--dry-run" {
					return nil
				}
			}
			return f.fakeInstall(dir, false)
		case "install":
			// Real npm install re-resolves and rewrites the lockfile; npm ci
			// does not. Tests rely on that difference to tell a re-resolution
			// from an install of a frozen lockfile.
			return f.fakeInstall(dir, true)
```

и заменить `fakeInstall` целиком:

```go
func (f *FakeRunner) fakeInstall(dir string, rewriteLock bool) error {
	if f.Stamp == nil {
		return fmt.Errorf("FakeRunner.Stamp is not set")
	}
	pkg, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return err
	}
	if rewriteLock {
		// Deterministic stand-in for npm's resolution: derived from the
		// manifest, so tests can tell a resolved lockfile from the seed.
		resolved := fmt.Sprintf("{\"lockfileVersion\":3,\"resolved_from\":%q}\n", pkg)
		if err := os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte(resolved), 0o644); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		return err
	}
	lock, err := os.ReadFile(filepath.Join(dir, "package-lock.json"))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "node_modules", ".fe-lock-stamp"), []byte(f.Stamp(pkg, lock)), 0o644)
}
```

- [ ] **Step 4: Убедиться, что тесты проходят**

```sh
go test ./...
```

Ожидается: `ok` по всем пакетам. Существующие тесты pipeline фигурируют на успешном `npm ci`
(lockfile не перезаписывается) либо на пресетах `FailNpmCI`, поэтому поведение не меняется.

- [ ] **Step 5: Commit**

```sh
git add internal/testutil/runner.go internal/testutil/testutil_test.go
git commit -m "test(testutil): FakeRunner переписывает lockfile на npm install"
```

---

### Task 5: stepDeps ставит по записи кэша (cache hit)

**Files:**
- Modify: `internal/pipeline/pipeline.go` (`stepDeps`)
- Modify: `internal/pipeline/deps.go` (`effectiveLock`)
- Test: `internal/pipeline/pipeline_test.go`

- [ ] **Step 1: Написать падающие тесты**

Добавить в `internal/pipeline/pipeline_test.go`:

```go
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
	if _, err := storeCachedLock(deps, DepsStamp(pkg, seed), cached); err != nil {
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
	if _, err := storeCachedLock(deps, DepsStamp(pkg, seed), cached); err != nil {
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
```

- [ ] **Step 2: Убедиться, что тесты падают**

```sh
go test ./internal/pipeline/ -run 'TestPipelineUsesCachedLockfile|TestPipelineSeedChangeMissesCache'
```

Ожидается: `FAIL` — `installed lockfile` равен seed, а не записи кэша (`stepDeps` ещё не смотрит в кэш).

- [ ] **Step 3: Реализовать выбор эффективного lockfile**

Дописать в `internal/pipeline/deps.go` (добавить `fmt` и `io` в импорты):

```go
// effectiveLock picks the lockfile npm is given: the resolution frozen in the
// cache for key when one exists, the seed otherwise. A cache read failure warns
// and falls back to the seed — that costs a resolve, it does not fail a build.
func (p *Pipeline) effectiveLock(seed []byte, key string, log io.Writer) []byte {
	cached, ok, err := loadCachedLock(p.DepsDir, key)
	switch {
	case err != nil:
		fmt.Fprintf(log, "WARN: deps cache read failed: %v — using the pinned lockfile\n", err)
		return seed
	case ok:
		fmt.Fprintf(log, "deps: cached lockfile %s\n", stampShort([]byte(key)))
		return cached
	default:
		fmt.Fprintf(log, "deps: pinned lockfile %s — no cached resolution for this manifest\n",
			stampShort([]byte(key)))
		return seed
	}
}
```

В `internal/pipeline/pipeline.go` в `stepDeps` заменить блок от чтения lockfile до записи stamp на:

```go
	lockSeed, err := os.ReadFile(filepath.Join(p.AssetsDir, "package-lock.json"))
	if err != nil {
		return fmt.Errorf("read pinned lockfile: %w", err)
	}
	pkgRaw, err := os.ReadFile(filepath.Join(workDir, "package.json"))
	if err != nil {
		return err
	}
	// The key pins both the upstream manifest and our seed: a change to either
	// is a different dependency set, so an entry is never out of sync with the
	// manifest it was resolved for.
	key := DepsStamp(pkgRaw, lockSeed)
	effective := p.effectiveLock(lockSeed, key, log)
	// Upstream ships no lockfile (it is gitignored), feed npm ours.
	if err := os.WriteFile(filepath.Join(workDir, "package-lock.json"), effective, 0o644); err != nil {
		return err
	}
	stampPath := filepath.Join(workDir, "node_modules", ".fe-lock-stamp")
	stamp, _ := os.ReadFile(stampPath)
	want := DepsStamp(pkgRaw, effective)
```

и в блоке `npm ci failed` (строки с `.bak` и `npm install`) заменить `lockRaw` на `lockSeed`:

```go
		if err := os.WriteFile(filepath.Join(workDir, "package-lock.json.bak"), lockSeed, 0o644); err != nil {
```

Остальное в `stepDeps` (probe, `npm install`, запись stamp) не меняется.

- [ ] **Step 4: Убедиться, что тесты проходят**

```sh
go test ./internal/pipeline/ -run 'TestPipelineUsesCachedLockfile|TestPipelineSeedChangeMissesCache' -v
```

Ожидается: `PASS`. Плюс полный прогон `go test ./...` — зелёный: `TestPipelineRunFull`
пинит stamp = `DepsStamp(pkg, seed)`, а при пустом кэше эффективный lockfile равен seed.

- [ ] **Step 5: Commit**

```sh
git add internal/pipeline/pipeline.go internal/pipeline/deps.go internal/pipeline/pipeline_test.go
git commit -m "feat(pipeline): stepDeps ставит по записи кэша resolved lockfile"
```

---

### Task 6: Холодный resolve сохраняется в кэш

**Files:**
- Modify: `internal/pipeline/pipeline.go` (`stepDeps`, блок fallback)
- Modify: `internal/pipeline/pipeline_test.go` (`TestPipelineLockfileDesyncResolves`, новый тест)

- [ ] **Step 1: Написать падающий тест**

Заменить `TestPipelineLockfileDesyncResolves` целиком (он переходит на хелпер с доступом к deps dir и дополняется проверкой записи кэша):

```go
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
```

Добавить новый тест на steady state:

```go
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
```

- [ ] **Step 2: Убедиться, что тесты падают**

```sh
go test ./internal/pipeline/ -run 'TestPipelineLockfileDesyncResolves|TestPipelineSecondBuildSkipsInstallAfterResolve'
```

Ожидается: `FAIL` — `cold resolve must freeze the lockfile: ok=false`, и во втором тесте
`second build must skip the install` (сейчас второй билд идёт в fallback заново).

- [ ] **Step 3: Реализовать сохранение резолва**

В `internal/pipeline/pipeline.go` в блоке `npm ci failed` после успешного `npm install` дописать:

```go
		if err := p.run(ctx, log, workDir, []string{"npm", "install", "--no-audit", "--no-fund"}); err != nil {
			return fmt.Errorf("npm install failed: %w", err)
		}
		// npm rewrote the lockfile: freeze that resolution under the key so
		// every later build of this (manifest, seed) pair installs the same
		// tree instead of resolving again.
		resolved, err := os.ReadFile(filepath.Join(workDir, "package-lock.json"))
		if err != nil {
			return err
		}
		want = DepsStamp(pkgRaw, resolved)
		pruned, err := storeCachedLock(p.DepsDir, key, resolved)
		if err != nil {
			fmt.Fprintf(log, "WARN: cannot cache the resolved lockfile: %v\n", err)
		} else {
			fmt.Fprintf(log, "deps: froze the resolved lockfile as %s\n", stampShort([]byte(key)))
			if pruned > 0 {
				fmt.Fprintf(log, "deps: pruned %d old cached lockfiles\n", pruned)
			}
		}
	}
```

(`want` уже объявлен выше через `:=`, поэтому здесь только присваивание.)

- [ ] **Step 4: Убедиться, что тесты проходят**

```sh
go test ./internal/pipeline/ -run 'TestPipelineLockfileDesyncResolves|TestPipelineSecondBuildSkipsInstallAfterResolve' -v
```

Ожидается: `PASS`.

- [ ] **Step 5: Commit**

```sh
git add internal/pipeline/pipeline.go internal/pipeline/pipeline_test.go
git commit -m "feat(pipeline): холодный resolve фиксируется в кэше"
```

---

### Task 7: Edge cases — недоступный кэш и битая запись

**Files:**
- Modify: `internal/pipeline/pipeline_test.go`

- [ ] **Step 1: Написать тесты**

Добавить в `internal/pipeline/pipeline_test.go`:

```go
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
	testutil.WriteFile(t, repo, "package.json", "{\"name\":\"lampa\",\"version\":\"0.0.2\"}\n")
	testutil.CommitAll(t, repo, "bump deps")
	log := &bytes.Buffer{}
	if err := p.Run(context.Background(), repo, "HEAD", "test.example", filepath.Join(t.TempDir(), "a.tar.gz"), log); err != nil {
		t.Fatalf("a failed cache write must not fail the build: %v\nlog:\n%s", err, log)
	}
	if !strings.Contains(log.String(), "cannot cache the resolved lockfile") {
		t.Errorf("expected a cache write warning:\n%s", log)
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
	if _, err := storeCachedLock(deps, key, []byte(corrupt)); err != nil {
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
```

- [ ] **Step 2: Убедиться, что тесты проходят**

```sh
go test ./internal/pipeline/ -run 'TestPipelineSurvivesUnusableDepsDir|TestPipelineColdResolveWithUnusableDepsDir|TestPipelineReplacesCorruptCachedLock' -v
```

Ожидается: `PASS` без правок продакшн-кода — Task 5 и 6 уже дают это поведение
(Warn на чтении, Warn на записи, перезапись записи после resolve). Если какой-то тест
падает, он указывает на реальный пробел в обработке ошибок из Task 5/6 — чинить там, а не здесь.

- [ ] **Step 3: Commit**

```sh
git add internal/pipeline/pipeline_test.go
git commit -m "test(pipeline): edge cases кэша resolved lockfile"
```

---

### Task 8: Документация

**Files:**
- Modify: `README.md` (таблица env, новый раздел)
- Modify: `CLAUDE.md` (блок «Ключевые решения дизайна»)

- [ ] **Step 1: Обновить README**

В таблице env (строка 38) описание `DATA_DIR` было
`каталог состояния, сборок и рабочих копий`, стало:

```
| `DATA_DIR`        | `/data`                                      | каталог состояния, сборок, рабочих копий и кэша зависимостей                                                                                     |
```

Добавить новый раздел между строкой 82 (конец раздела «Запуск») и `## Разработка`:

````markdown
## Зависимости фронтенда

Upstream не хранит `package-lock.json` (он у него в `.gitignore`), поэтому билдер
подкладывает свой — он лежит в `ASSETS_DIR` рядом с `patches/` и `overlay/`.
Этот файл — только **seed**: зависимости upstream меняются по диапазонам
(`^7.15.8`), и seed быстро расходится с манифестом.

Авторитет — зафиксированный resolve в `DATA_DIR/deps/`, по одному файлу на ключ
`md5(upstream package.json + seed lockfile)`. Первый билд после изменения
манифеста (или seed) уходит в `npm install`, и результат сохраняется под этим
ключом; все последующие сборки — в любой рабочей копии — ставят зависимости
через `npm ci` по замороженному lockfile и получают один и тот же набор версий.
Кэш обрезается до 20 записей по `mtime`.

Следствия:

- обновлять seed в репозитории нужно только осознанно — например, чтобы двинуть
  пин вручную; при обычных обновлениях upstream это не требуется;
- снос `DATA_DIR` возвращает холодный resolve: кэш живёт там же, где состояние и
  рабочие копии;
- какие версии фактически поставлены, видно в `build.log` сборки — строки
  `deps: cached lockfile <key>` и `deps: pinned lockfile <key>`.
````

- [ ] **Step 2: Обновить CLAUDE.md**

В блоке «Ключевые решения дизайна (см. spec)» добавить пункт:

```markdown
- Авторитет пина зависимостей — кэш resolved lockfile в `DATA_DIR/deps` по ключу
  (upstream-манифест, seed); `package-lock.json` в репозитории — только seed.
```

- [ ] **Step 3: Commit**

```sh
git add README.md CLAUDE.md
git commit -m "docs: кэш resolved lockfile в DATA_DIR/deps"
```

---

### Task 9: Полная проверка и PR

**Files:** —

- [ ] **Step 1: Прогнать всё, что гоняет CI**

```sh
gofmt -l . && go vet ./... && go test ./...
```

Ожидается: `gofmt -l` печатает пустой список, `go vet` молчит, `go test ./...` — `ok` по всем пакетам.

- [ ] **Step 2: Проверить e2e-путь (требует node и сети)**

```sh
make e2e
```

Ожидается: полная сборка против реального upstream проходит. Если сеть или node
недоступны — зафиксировать это в описании PR как непроверенное, не выдавать за успех.

- [ ] **Step 3: Убедиться, что в коммитах нет приписок**

```sh
git log --format='%H %s%n%b' main..HEAD | grep -inE 'co-authored-by|generated with|claude' || echo "clean"
```

Ожидается: `clean`.

- [ ] **Step 4: Push и PR в main**

```sh
git push -u origin deps-lock-cache
gh pr create --base main --title "Кэш resolved lockfile по ключу (upstream manifest, seed)" \
  --body-file - <<'EOF'
Зависимости фронтенда фиксируются в `DATA_DIR/deps` по ключу
`md5(upstream package.json + seed lockfile)`.

Проблема: результат fallback-резолва выбрасывался, из-за чего пары
(domain, commit) собирались из разных деревьев зависимостей, а сдвиг пина
оставался ручной обязанностью.

- `stepDeps` ставит по записи кэша, если она есть, иначе по seed;
- stamp свежести `node_modules` считается от эффективного lockfile;
- холодный resolve (`npm install`) сохраняется атомарно и переиспользуется
  всеми рабочими копиями;
- кэш обрезается до 20 записей по `mtime`;
- spec: `docs/superpowers/specs/2026-10-10-deps-lock-cache-design.md`.
EOF
```

Ожидается: PR создан, CI зелёный.

---

## Self-Review

**Spec coverage:**
- «Решение: effective = cached ?? seed, ключ DepsStamp(manifest, seed)» → Task 5.
- «Cold resolve сохраняется, а не выбрасывается» → Task 6.
- «Stamp от эффективного lockfile» → Task 5 (Step 3), регресс-пин сохранён в `TestPipelineRunFull`.
- «Совместимость: пустой кэш → прежнее значение stamp» → Task 5 Step 4 (полный прогон, `TestPipelineRunFull` не меняется).
- «Компоненты: DepsDir, New, loadCachedLock, storeCachedLock, depsEvictPlan, depsCacheKept» → Tasks 1-3.
- «Edge cases» → Tasks 5 (seed change), 6 (steady state), 7 (невалидный dir, битая запись); «снос /data» и «две рабочие копии» — следствия ключа и общего `DepsDir`, отдельных механизмов не требуют.
- «Лог» → Tasks 5, 6 (строки `cached lockfile`, `pinned lockfile`, `froze`, `pruned`).
- «Тест-дубль переписывает lockfile» → Task 4.
- «Расширить TestPipelineLockfileDesyncResolves» → Task 6 Step 1.
- «Документация: README + CLAUDE.md» → Task 8.
- «Не входит: state/api, эндпоинт, обновление seed из кэша, конкурентная защита» → не реализуется нигде, включая Task 3 (DepsDir только читается/пишется пайплайном).

**Пропущенные тесты из spec:** пункт «две рабочие копии сходятся на одной записи»
не выделен в отдельный тест: две рабочие копии различаются только путём workDir,
общий `DepsDir` даёт сходимость по построению, и она уже покрыта
`TestPipelineSecondBuildSkipsInstallAfterResolve` (второй билд берёт запись).
Отдельный тест дублировал бы его без новой информации.

**Placeholder scan:** плейсхолдеров нет; каждый шаг с кодом содержит полный код,
каждый шаг с командой — ожидаемый результат.

**Type consistency:** `depsEntry{name, modTime}`, `depsEvictPlan(entries, keep)`,
`depsCacheEvictPlan(depsDir)`, `depsLockPath(depsDir, key)`,
`loadCachedLock(depsDir, key) ([]byte, bool, error)`, `storeCachedLock(depsDir, key, raw) (int, error)`,
`(p *Pipeline) effectiveLock(seed, key, log) []byte`, `Pipeline.DepsDir`,
`New(assetsDir, depsDir, r)`, `newPipeline` / `newPipelineDeps`,
`fakeInstall(dir, rewriteLock)` — имена одинаковы во всех задачах и шагах.