# Hindsight — agent working agreement

Binding detail lives in `ARCHITECTURE.md` (layout, route contract, SSE
protocol, auth, error contract, DB rules, test seams, locked decisions).
If this file and that one conflict, `ARCHITECTURE.md` wins.

## Gates (run before handing back)
- Full gate: `make check` = `fmt-check` + `generate` + `vet` + `test` + `lint`.
- Single gates: `make build`, `make test` (`go test -race -count=1 ./...`),
  `make vet`, `make fmt-check`, `make lint`.
- `gofmt` + `go vet` must be clean with zero warnings; linter is
  golangci-lint v2 (config in `.golangci.yml`).
- Layering (section below) is machine-checked by `depguard` inside
  `make lint`, not just prose — a forbidden import fails the gate.

## Workflow
- TDD per change: failing test → run → minimal code → run.
- Commit after each task with `feat:` / `fix:` prefixes. Push only when asked.
- No code, test, or comment may reference plan phase numbers.

## Single-binary containment (binding)
- The binary must serve everything from embedded bytes. At runtime the
  process reads from disk ONLY the SQLite DB file.
- Three embed surfaces, all mandatory:
  - `static/*` via `//go:embed` (htmx, SSE ext, Alpine, PicoCSS, app JS/CSS).
  - `db/migrations/*.sql` via `goose.SetBaseFS` — never from disk.
  - Templates: `templ` compiles to Go. If `html/template` is ever used,
    parse `.tmpl` files from an embedded FS, never from disk.
- No CDN links anywhere. No filesystem reads for assets, migrations,
  or templates.

## Architecture
- `main.go`: embed directives, ServeMux wiring, middleware, startup
  (`goose.Up`), nothing else.
- `handlers/`: HTTP only — parse input, call `db`, publish to `realtime`,
  render a `templates` partial. No SQL here. Depend on consumer-side
  interfaces defined in `handlers`, never concrete store/broker types
  (fakes live in `handlers/*_test.go`).
- `db/`: schema versioning (goose owns `goose_db_version`; no hand-rolled
  runner) plus `store.go`. Every mutation runs in `WithTx`
  (`BEGIN IMMEDIATE`); cap checks (votes) happen inside the same tx as the
  write. PRAGMAs: `journal_mode=WAL`, `busy_timeout=5000`,
  `synchronous=NORMAL`, `foreign_keys=ON`.
- `realtime/`: per-board SSE broker. Publish only after DB commit. Every
  message carries a per-board monotonic `id:`; keep a ~100-event ring
  buffer; honor `Last-Event-ID` with replay, else emit `sync-required`.
  Heartbeat 10–15s. SSE headers plus explicit `Flush()` on every write.
- `templates/`: one component per fragment (`Card` → `id="card-{id}"`,
  `Column` → `id="column-{id}"`). Pages compose fragments; mutations
  return fragments, never full pages.
- Routing: stdlib ServeMux with `METHOD /pattern` forms
  (e.g. `GET /b/{bid}`). Facilitator-only routes go through the
  token middleware (constant-time hash compare against
  `boards.facilitator_token_hash`).

## Frontend rules
- Client→server is plain htmx requests only. Exactly one `EventSource`
  per board page (`sse-connect`), listeners per column/card
  (`sse-swap`), OOB deletes for removals.
- Granular swaps only: `card-updated:{id}` / `vote-changed:{id}` swap
  `#card-{id}` outerHTML. Full-column swaps are for explicit facilitator
  actions (sort, reveal), never for routine votes.
- All Alpine `x-data` scopes sit ABOVE swap targets (board shell / column
  container, never inside `#card-{id}`). Re-init via a single
  `htmx:afterSwap → Alpine.initTree` hook. Timer countdown lives outside
  any SSE-swapped subtree; the server owns `timer_ends_at`.
- Blind collection redacts server-side: non-facilitator fragments carry
  `body='•••'` until reveal. Never hide bodies with CSS alone.

## Identity (v1, no accounts)
- Share URL carries `boards.public_id` (128-bit entropy). Two HMAC-signed
  cookies, `hindsight_fac` (`{boardID: facilitatorToken}`) and
  `hindsight_parts` (`{boardID: participantToken}`); only SHA-256 hashes
  persist, compared with `subtle.ConstantTimeCompare`. Server secret lives
  in `app_meta(secret)`. Display names map to a `participants` row;
  votes key to that row. Duplicate names get suffixed. Gameability is
  accepted for v1 and documented, not hardened. Full scheme:
  `ARCHITECTURE.md` section 7.

## Errors
- Status + OOB flash fragment, always together (`ARCHITECTURE.md`
  section 8): 403/429 carry `<div id="flash" hx-swap-oob="true">…</div>`
  and nothing else; 404 is plain `http.NotFound`; over-length input is
  422 + flash. `templ.Unsafe` / `template.HTML` forbidden.

## Testing
- `db/store_test.go` style: temp-dir DBs (`t.TempDir()`), goose up on a
  fresh DB, re-run is a no-op. Cover double-vote rejection, cap races
  (N goroutines, cap holds), and concurrent writers (no `SQLITE_BUSY`).
- Handler tests assert post-commit broadcast payload + sequence ordering.
- `e2e/`: Playwright, chromium only, two-context specs for realtime
  (A acts, B sees it <1s). No manual-click verification.
- Golden files for export output (`testdata/`).
