# Кэш resolved lockfile по ключу (upstream manifest, seed)

Дата: 2026-10-10
Ветка: `deps-lock-cache`

## Проблема

`package-lock.json` в репозитории — пин зависимостей upstream-фронта: upstream
его игнорирует, а билдер подкладывает свой и ставит зависимости через `npm ci`
(`internal/pipeline/pipeline.go:252-273`). Когда upstream меняет свой
`package.json` так, что пин перестаёт подходить, пайплайн уходит в `fallback`:
`npm ci` падает → probe `npm ci --dry-run` падает → `npm install`
(`pipeline.go:273-290`). Результат этого resolve **выбрасывается**:

- `.bak` хранит старый пин (`pipeline.go:284`), а не новый;
- stamp свежести `node_modules` считается от старого пина
  (`DepsStamp(pkgRaw, lockRaw)`, `pipeline.go:266`), поэтому после `npm install`
  следующий билд считает `node_modules` свежим и установку пропускает
  (`pipeline.go:267-269`);
- resolved lockfile остаётся внутри рабочей копии, где он gitignored upstream и
  на следующем билде перезаписывается пином (`pipeline.go:261`).

Следствия:

1. **Недетерминированность.** Один и тот же upstream-коммит собирается из разных
   деревьев зависимостей: две рабочие копии (`/data/work/repo-<copyID>`,
   `builder.go:635`) resolve'ят независимо и в разное время; снос `/data` или
   `node_modules` запускает resolve заново в другую дату. Это ломает кэш сборок
   по (domain, commit) и закрепление свежей успешной тестовой сборки — пары
   (domain, commit) перестают соответствовать одному артефакту.
2. **Обновление пина остаётся ручной обязанностью.** Единственный способ сдвинуть
   пин — правка репозитория и пересборка образа, хотя фактически нужный resolve
   уже был получен и просто выброшен.

Проверено на npm 11.14.1:

| изменение в upstream `package.json` | `npm ci` | fallback |
|---|---|---|
| диапазон сдвинут, пин в него входит | OK | нет |
| диапазон ушёл выше пина | fail | да |
| добавлена/удалена зависимость | EUSAGE: `package.json and package-lock.json … in sync` | да |

`npm install` поверх существующего lockfile не переезжает целиком: пины, всё ещё
удовлетворяющие диапазонам, сохраняются (проверено — `doctrine@3.0.0` остался на
месте при добавлении зависимости). То есть drift локальный, но момент его
наступления и его результат не контролируются.

Дополнительно, `npm ci` (и `npm ci --dry-run`) **не могут** создать lockfile:
без него оба завершаются EUSAGE. Единственная команда, создающая lockfile, —
`npm install`, то есть seed из репозитория обязателен как вход.

## Цель

Зафиксировать результат resolve так, чтобы для одинаковой пары (байты upstream
`package.json`, байты seed-лока) везде ставился один и тот же набор версий — без
обязанности обновлять репозиторий при каждом изменении upstream-манифеста.

**Гарантия:** одинаковый seed + одинаковый upstream-манифест → одинаковый
resolved lockfile в любой рабочей копии и после любого сноса `node_modules` —
пока запись кэша для этой пары существует. Гарантия перестаёт действовать, если
запись вытеснена (больше `depsCacheKept` разных ревизий манифеста) или
зафиксирована заново: при транзиентном сбое `npm ci`, который провалил и
`--dry-run` probe, `npm install` перезапишет запись свежим resolve (см.
«Отклонения» в плане). Байт-идентичность итогового архива не гарантируется:
детерминизм самого bundler'а вне этой задачи.

## Решение

Пин из репозитория становится **seed**, авторитет — зафиксированный resolve в
`DATA_DIR/deps/`, ключ — `DepsStamp(upstream package.json, seed lockfile)`, то
есть уже существующая функция `pipeline.DepsStamp`. Имя записи — сам ключ
(`<key>.lock.json`); значение, которое пишется в `node_modules/.fe-lock-stamp`,
считается от эффективного lockfile, то есть от того, что реально получил npm.

Ключ включает upstream-манифест, поэтому правка манифеста (новая, удалённая или
сдвинутая зависимость) даёт новый ключ: запись не бывает рассинхронизирована с
манифестом по построению, отдельная валидация не нужна.

Правка seed в репозитории меняет ключ → один холодный resolve → новый
зафиксированный пин. Так seed остаётся рычагом осознанного сдвига пина, но
перестаёт быть обязанностью.

### Поток данных

```
lockSeed  = read(AssetsDir/package-lock.json)
pkgRaw    = read(workDir/package.json)
key       = DepsStamp(pkgRaw, lockSeed)
cached    = read(DepsDir/<key>.lock.json)        # miss — не ошибка
effective = cached, если есть, иначе lockSeed
write(workDir/package-lock.json, effective)      # один раз, до установки

stamp = read(workDir/node_modules/.fe-lock-stamp)
want  = DepsStamp(pkgRaw, effective)             # эффективный, а не seed
if stamp == want: skip                           # node_modules свеж

npm ci
  fail → probe `npm ci --dry-run`
    probe ok  → fail build ("lockfile is in sync")        # как сейчас
    probe err → WARN + .bak (seed) + `npm install`
                resolved = read(workDir/package-lock.json)     # npm его перезаписал
                store(DepsDir/<key>.lock.json, resolved)       # атомарно, best-effort
                want = DepsStamp(pkgRaw, resolved)
write(workDir/node_modules/.fe-lock-stamp, want)
```

Три содержательных сдвига:

1. На cache hit `npm ci` идёт по замороженному lockfile — рассинхрон невозможен,
   fallback не запускается.
2. Stamp считается от эффективного lockfile, то есть описывает то, что реально
   установлено, а не seed.
3. После холодного resolve результат **сохраняется**, а не выбрасывается.

### Совместимость

При пустом кэше `effective == seed`, поэтому `want == DepsStamp(pkgRaw, seed)` —
ровно текущее значение (`pipeline_test.go:102-106`). Апгрейд билдера не вызывает
лишний reinstall.

## Компоненты и проводка

- `internal/pipeline/pipeline.go`
  - `Pipeline` получает поле `DepsDir`; `New(assetsDir, depsDir string, r
    execrun.Runner)` — новый параметр.
  - `stepDeps` переписывается по псевдокоду выше.
  - `loadCachedLock(depsDir, key) ([]byte, bool, error)` — отсутствие файла это
    не ошибка; любая другая ошибка чтения (например, права) логируется как Warn
    и трактуется как miss: сборка продолжается от seed.
  - `storeCachedLock(depsDir, key string, raw []byte) error` — запись через
    временный файл и `rename` (идиома `gitops.EnsureCopy`, issue #10); ошибка
    записи не валит сборку. После успешной записи вызывается `depsEvictPlan` и
    удаляются лишние записи.
  - `depsEvictPlan(entries []depsEntry, keep int) []string`, где
    `depsEntry{name string, modTime time.Time}` — чистая функция, по образцу
    `EvictPlan` (`internal/builder/retention.go:14`), тестируется без файловой
    системы. Порядок: `mtime` по убыванию, при равенстве — имя по возрастанию;
    лишнее удаляется, начиная со старейшего.
  - `depsCacheKept = 20` — константа рядом с `stepDeps`, по образцу
    `failedDirsKept` (`retention.go:61`).
- `main.go:45` — `pipeline.New(cfg.AssetsDir, filepath.Join(cfg.DataDir, "deps"), exe)`.
- Не меняются: `internal/state`, `internal/api`, `internal/config`, `Dockerfile`
  (`/data` уже writable volume, `DATA_DIR` уже есть).

## Edge cases

| случай | поведение |
|---|---|
| cache hit | `npm ci` по замороженному lockfile, fallback не запускается |
| холодный resolve | `npm install`, результат сохраняется как запись кэша |
| `DepsDir` не writable / диск полон | Warn в лог, сборка продолжается от resolved lockfile в рабочей копии; следующий билд снова холодный |
| битая или обрезанная запись | `npm ci` падает → probe падает → resolve → запись перезаписывается (self-healing, отдельная валидация JSON не нужна) |
| правка seed в репозитории | новый ключ → один холодный resolve → новый пин |
| снос `/data` | кэш уходит вместе с `node_modules`; resolve заново — ожидаемо, `/data` и есть носитель фиксации |
| записей больше лимита | eviction по `mtime`; удаление записи безопасно by design — следующий билд под этот ключ просто resolve'ит заново |
| две рабочие копии | читают один и тот же кэш → сходятся на одном дереве |
| upstream-манифест без изменений, новый коммит | ключ тот же → cache hit, установка пропускается по stamp |

## Лог

```
deps: cached lockfile 9f2c8a1b
deps: pinned lockfile 9f2c8a1b — no cached resolution for this manifest/seed pair
WARN: lockfile out of sync with upstream package.json — re-resolving (npm install)
deps: froze the resolved lockfile as 9f2c8a1b
deps: pruned 3 old cached lockfiles
WARN: deps cache read failed: <err> — using the pinned lockfile
WARN: cannot cache the resolved lockfile: <err>
WARN: cannot prune the deps cache: <err>
```

Ключ в строках показан как первые 8 hex-символов (`stampShort`).

Доступно через существующий `GET /api/v1/builds/{id}/logs` (`internal/api/api.go:180`),
изменений в `state`/`api` не требуется.

## Тесты

Правка тест-дубля: `testutil.FakeRunner.fakeInstall`
(`internal/testutil/runner.go:128`) сейчас не перезаписывает lockfile, тогда как
реальный `npm install` — перезаписывает. Без этого холодный путь неотличим от
cache hit. Дубль получает детерминированную имитацию resolve: `npm install`
переписывает `package-lock.json` как функцию от `package.json`. Единственное
изменение в `testutil`.

`internal/pipeline/pipeline_test.go`:

1. cache hit: `npm ci` идёт по байтам записи, `npm install` не вызывался;
2. холодный путь: запись создана, содержимое равно resolved lockfile;
3. третья сборка при живом `node_modules`: установка пропускается по stamp;
4. две рабочие копии сходятся на одной записи;
5. правка seed → новый ключ → холодный resolve;
6. `DepsDir` не writable → сборка успешна + Warn;
7. битая запись → self-healing;
8. `depsEvictPlan`: граница `keep`, порядок при равных `mtime`;
9. регресс: пустой кэш → stamp равен `DepsStamp(pkg, seed)`.

Существующий `TestPipelineLockfileDesyncResolves` (`pipeline_test.go:245`)
дополняется проверкой, что после resolve появилась запись кэша.

## Не входит в объём

- Изменения `state`/API, эндпоинт управления кэшем.
- Обновление seed из кэша.
- Конкурентная защита записи: сегодня сборки идут одним worker-goroutine
  (`builder.go:102-115`), `rename` нужен против падения процесса, не против гонки.
- Пересмотр `.bak` — остаётся как сейчас.
- Детерминизм самого bundler'а.
- Остатки `tmp-*` в `DATA_DIR/deps` после kill процесса между созданием временного
  файла и `rename` не убираются: окно микросекундное, файл безвреден (glob
  `*.lock.json` его не видит). При необходимости — sweep в `pruneDepsCache`.

## Документация

- `README.md`: абзац про кэш зависимостей — что, где, ключ, что снос `/data`
  возвращает холодный resolve.
- `CLAUDE.md`, блок «Ключевые решения дизайна»: строка про то, что авторитет пина
  в `/data`, а файл в репозитории — seed.

## Принятые решения

1. Ключ — `DepsStamp(upstream manifest, seed lockfile)`, то есть seed остаётся
   рычагом осознанного сдвига пина.
2. Observable-состояние — только build.log; `state`/`api` не трогаем.
3. Размер кэша ограничен `depsCacheKept = 20` (newest by `mtime`).
4. Issue не заводится: работа идёт этой веткой и PR.