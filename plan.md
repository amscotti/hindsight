# Hindsight — Retro Board App Implementation Plan

> **For Hermes:** Use subagent-driven-development skill to implement this plan task-by-task.

**Goal:** Build a self-hosted, single-binary team retrospective web app (EasyRetro-class features) in Go.

**Architecture:** Server-rendered HTML (templ components) + htmx for mutations + SSE broadcast per board for realtime updates + Alpine.js for local UI state. SQLite for persistence. All static assets vendored via `go:embed`. One `docker` image (`scratch` + single binary).

**Tech Stack:** Go 1.24+, `a-h/templ` (templates), htmx + SSE extension + Alpine.js + PicoCSS (vendored JS/CSS), `modernc.org/sqlite` (pure-Go, no CGO), `pressly/goose/v3` (migrations, `database/sql`-native), stdlib `net/http` ServeMux (no web framework).

---

## Exec summary

EasyRetro's core loop: create board from template → share link → team adds cards per column → vote to prioritize → group/merge → discuss → action items → export/archive. This plan replicates that loop with no accounts (join via unguessable link + display name, facilitator controls gated by a per-board token), realtime sync for all viewers via SSE, and packaging as one binary + one container backed by a single SQLite file. Phases go: skeleton → data layer → board CRUD → cards/votes realtime core → facilitation (grouping, timers, hide/reveal) → kudos/comments/action items → export/archive → polish (Docker, ngrok docs). No build without explicit go-ahead; this plan is discuss-only until approved.

## Tech overview

- **Rendering:** `templ` components compile to Go; pages render full HTML, mutations return partials. Static fallback: stdlib `html/template` if templ codegen is rejected at review gate P1.
- **Realtime:** One SSE broker per board (`map[boardID]*Broker`, mutex-guarded subscriber sets). Mutations (POST/PUT/DELETE via htmx) write to SQLite then broadcast a named event; clients use the htmx SSE extension (`sse-connect`, `sse-swap`) to swap updated column/card fragments. No websockets: all client→server traffic is plain htmx requests. SSE plays fine through ngrok (long-lived HTTP, no special config).
- **Local interactivity (Alpine.js):** column timer countdown display, drag-and-drop card moves (POST on drop), optimistic vote-button state, facilitation panel toggles.
- **Styling:** PicoCSS (classless + minimal utilities), vendored.
- **DB:** `modernc.org/sqlite` + `database/sql`. Migrations via `pressly/goose/v3`, embedded into the binary with `//go:embed db/migrations/*.sql` + `goose.SetBaseFS`; run `goose.Up` at startup (goose tracks version in its own `goose_db_version` table — no hand-rolled runner). Single DB file `./data/hindsight.db`, WAL mode.
- **Routing:** stdlib ServeMux with `GET /b/{id}` style patterns. Middleware: request logging, facilitator-token check.
- **Identity (v1, no accounts):** `boards.public_id` (unguessable, in share URL), `boards.facilitator_token` (shown once at creation, stored in facilitator's cookie). Participants pick a display name stored in a signed cookie; votes keyed to participant row so limits are enforceable-ish (YAGNI: no auth hardening v1).
- **Single binary (mandatory, see binding section):** `go:embed` for `static/*` (htmx, alpine, pico, app JS/CSS), `db/migrations/*.sql`, and templates (templ output is compiled Go; if `html/template` fallback is taken, its `.tmpl` files must be embedded, never read from disk). Version pin vendored JS with hashes in `static/vendor/VERSIONS.md` (CI verifies with `sha256sum -c`).

## Single-binary containment (binding)

Everything the server needs at runtime MUST live inside the one binary. At runtime the process reads from disk ONLY the SQLite DB file (a write target, external by design) — nothing else. There is no CDN dependency, no external template/template resolver, no migration file read from the filesystem.

The three embed surfaces, all mandatory:

- **Static web assets** — `//go:embed static/*` (htmx, SSE ext, idiomorph, Alpine, PicoCSS, app JS/CSS), served via `http.FS`.
- **SQL migrations** — `//go:embed db/migrations/*.sql`, read through `goose.SetBaseFS` (embedded FS), never from disk. goose applies them at startup and tracks version in `goose_db_version`.
- **Templates** — `templ` `.templ` files compile to Go (in the binary); if the Phase-1 review gate falls back to `html/template`, the `.tmpl`/`.html` files MUST be `//go:embed templates/*` and parsed from the embedded FS, never from disk.

**Containment test (no-filesystem smoke):** after `go build`, copy the binary to a clean temp dir (no source, no `static/`, no `migrations/`), `cd` elsewhere, run it, and confirm the landing page renders AND migrations apply. If either needs a file next to the binary, the embed is wrong. Assert this in Phase 1 DoD (static + landing) and Phase 8 (full path: create board + migrations + assets from a bare binary).

## Conventions (binding on implementation)

- Go 1.24+, `gofmt` + `go vet` clean, zero warnings.
- TDD per task: failing test → run → minimal code → run → commit (`feat:`, `fix:` prefixes).
- Commits after every task; push only when asked.
- No code/test/comment may reference plan phase numbers (plan is deleted after implementation).

---

## Phase 1 — Skeleton: server, vendored assets, health check

**Goal:** `go build ./...` produces one binary that serves a PicoCSS-styled landing page with vendored htmx/Alpine; `/healthz` returns ok.

**TODO:**
1. `go mod init hindsight` (+ `go 1.24.x` + toolchain pin), add `a-h/templ` (pinned via `tool` directive), `modernc.org/sqlite`, `pressly/goose/v3`. Root `//go:generate go run github.com/a-h/templ/cmd/templ generate` so Docker builds reproduce.
2. Download htmx (+ SSE ext + idiomorph ext), Alpine.js, PicoCSS into `static/vendor/`; record versions/hashes in `static/vendor/VERSIONS.md` (CI verifies with `sha256sum -c`).
3. `main.go` (embed `static/`, ServeMux, `GET /healthz`, `GET /` landing page with empty-state CTA copy).
4. Templ `Base` layout component with PicoCSS + script tags.
5. Playwright e2e scaffold: `e2e/` (Node, `@playwright/test`, chromium only), `e2e/playwright.config.ts` (baseURL `http://localhost:8080`, webServer starts `./hindsight`), one smoke spec (landing renders, `/healthz` ok). Every later phase adds its specs here; two-browser checks run as two-context Playwright tests, not manual clicks.

**DoD:** `go build -o hindsight . && ./hindsight` → landing page renders styled, no CDN requests (devtools network check); `/healthz` → 200 `ok`; `go vet ./...` clean; `npx playwright test` → smoke spec passes; **containment check** — copy the binary alone to a bare temp dir, run it from a different cwd, landing page still renders. (Definitive Dockerfile + compose land in Phase 8; Phase 1 proves only `go build` + `go run`.)

**Review gate:** Approve templ (keep) vs drop to `html/template` if codegen friction is high.

## Phase 2 — Data layer: schema + migrations + store

**Goal:** Versioned SQLite schema with CRUD store functions covered by tests (temp-file DBs).

**TODO:**
1. `db/migrations/00001_init.sql` (goose file naming: `NNNNN_name.sql`, or `.up.sql`/`.down.sql` if a rollback is wanted — v1 is up-only): tables `boards (id, public_id UNIQUE, name, context, facilitator_token_hash, phase, votes_per_person, timer_ends_at NULL, voting_locked INTEGER DEFAULT 0, cards_locked INTEGER DEFAULT 0, cards_hidden INTEGER DEFAULT 0, archived_at, created_at)`, `columns (id, board_id REFERENCES boards ON DELETE CASCADE, title, color, position CHECK(position>=0))`, `cards (id, column_id, board_id, body, author_name, votes DEFAULT 0, group_id REFERENCES cards(id) ON DELETE SET NULL, discussed INTEGER DEFAULT 0, position, hidden, created_at)`, `votes (participant_id REFERENCES participants ON DELETE CASCADE, card_id REFERENCES cards ON DELETE CASCADE, created_at, PRIMARY KEY(participant_id, card_id)))`, `comments`, `kudos`, `actions (..., carried_from NULL)`, `participants (id, board_id, name, token, UNIQUE(board_id, name))`. Indexes on all FK columns. CHECK `votes_per_person > 0`. Grouping rule: group leader row holds members via `group_id = leader_id`; group vote sum = SUM over members.
2. `db/db.go`: `sql.Open("sqlite", path)` (modernc registers driver `sqlite`); PRAGMAs `journal_mode=WAL`, `busy_timeout=5000`, `synchronous=NORMAL`, `foreign_keys=ON`; `//go:embed migrations/*.sql` → `goose.SetBaseFS(fs)`, `goose.SetDialect("sqlite3")`, `goose.Up(db, "migrations")` at startup — goose owns schema versioning (its own `goose_db_version` table), so no hand-rolled runner. `db/store.go`: every mutation wrapped in `WithTx` (`BEGIN IMMEDIATE`); vote/unvote checks cap via `SELECT COUNT(*)` + existing-row check + counter update in one tx.
3. Tests `db/store_test.go` (t.TempDir DB): goose up applies all migrations on fresh DB; re-running `goose.Up` is a no-op; board create/get, column ordering, card move, double-vote rejected, unvote decrements, concurrent vote-cap race (N goroutines, cap holds), 20 concurrent writers with no `SQLITE_BUSY`.

**DoD:** `go test ./db/ -v` all pass; re-running migrations on existing DB is a no-op; no CGO in build (`CGO_ENABLED=0 go build`).

**Review gate:** Schema must support grouping (self-FK), archive (flag not delete), carry-over actions before leaving phase.

## Phase 3 — Boards: templates, create, share, list, archive

**Goal:** User can create a board from a retro template, get a share link + facilitator token, view dashboard of boards, archive/unarchive.

**TODO:**
1. **Route table (Method-prefixed ServeMux patterns, facilitator-cookie middleware with constant-time hash compare — reused by Phases 4/5):** `GET /` dashboard, `GET /new`, `POST /boards`, `GET /b/{bid}`, `POST /b/{bid}/join` (display name → participant row + signed cookie; duplicate names suffixed), `POST /b/{bid}/phase`, `POST /b/{bid}/reveal`, `POST /b/{bid}/lock`, `POST /b/{bid}/timer`, `POST /b/{bid}/focus`, `POST /boards/{bid}/archive`, `POST /boards/{bid}/unarchive`, `POST /boards/{bid}/duplicate` (copy columns + optionally open actions).
2. `templates/` components: `BoardNew` (name, context, template picker, optional carry-from selector — stub until Phase 6), `Dashboard` (active/archived lists), `Board` shell, join-gate name modal.
3. Templates seed ≥4 classic formats (Mad/Sad/Glad, Start/Stop/Continue, 4Ls, Kudos variant); columns customizable later (Phase 5).
4. Share UX: copy-participant-link + copy-facilitator-link buttons, facilitator-token regenerate; token shown once at creation.
5. Empty states (acceptance criteria, verified in Phase 5 dry-run): dashboard CTA, new-board invite hint, empty-column add prompt, blind-mode waiting notice.
6. Tests: create → columns seeded in order; share id ≥128-bit entropy; join creates row / dupes suffixed / voteless-without-join rejected; non-facilitator control POSTs → 403; archive hides but preserves, unarchive restores, duplicate copies columns.

**DoD:** Manual: create board → redirected to `/b/{public_id}` with columns; facilitator token shown once; archive → board leaves dashboard, content preserved, unarchive restores.

**Review gate:** Confirm no-login model + token-once UX is acceptable; confirm template list.

## Phase 4a — SSE broker with replay/resync

**Goal:** Per-board broadcast with no silent divergence on reconnect or slow clients.

**TODO:**
1. `realtime/broker.go`: per-board subscriber registry; every message carries per-board monotonic `id:`; ring buffer of last ~100 events per board; `GET` honors `Last-Event-ID` by replaying missed events, else emits `sync-required`; slow client → mark-dirty + `sync-required` instead of silent drop; heartbeat 10–15s; publish to broker only after DB tx commit.
2. Server headers every SSE write: `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `X-Accel-Buffering: no`, explicit `Flush()`.
3. Tests: two subscribers both receive; disconnect cleanup leaks no goroutines (`NumGoroutine` stable at 50 subscribers); reconnect with `Last-Event-ID` replays gap; dropped slow client receives `sync-required`.

**DoD:** Kill stream mid-session (ngrok restart / sleep) → client resyncs automatically with no stale state.

**Review gate:** Resync protocol locked here; all later phases reuse it.

## Phase 4b — Cards CRUD with granular SSE events

**Goal:** Card add/edit/move/delete with targeted swaps that never clobber in-flight local state.

**TODO:**
1. `handlers/cards.go`: `POST /b/{bid}/cards`, `PUT /cards/{cid}` (edit/move), `DELETE /cards/{cid}`. Broadcast granular events with per-board seq: `card-added` (append into `#column-{id} .card-list`), `card-updated:{id}` / `vote-changed:{id}` (swap `#card-{id}` outerHTML only), `card-removed:{id}` (OOB delete). Column-level events (`column-sorted`, `cards-revealed`) only for explicit facilitator actions.
2. `handlers/events.go`: `GET /b/{bid}/events`; board shell holds single `sse-connect='/b/{bid}/events'`; one listener per column (`sse-swap='column-{colID}'`, server sends `event: column-{colID}`) plus per-card listeners/OOB swaps. Exactly one EventSource per board page.
3. Templ partials: `Card` (`id="card-{id}"`), `Column` (`id="column-{id}"`).
4. Tests: broadcast emitted post-commit (fake subscriber asserts payload + seq ordering; stale seq ignored client-side).

**DoD:** Playwright two-context spec: add/move/edit in context A appears in context B <1s; typing an edit in B while A votes does not lose the draft.

## Phase 4c — Votes with cap + client FSM

**Goal:** `POST /cards/{cid}/vote|unvote` enforced per participant row; honest UI states.

**TODO:**
1. Server: cap check inside vote tx (Phase 2); 403 with message on cap/double-vote; vote broadcasts `vote-changed:{id}`.
2. Client: vote button FSM idle→pending→confirmed/denied, server message rendered on 403/429; connection badge from SSE open/error events with polling fallback (`hx-get` refresh every 5s while disconnected).
3. Tests: cap enforced, double-vote rejected, unvote decrements (Phase 2 store tests + handler tests).

**DoD:** 4th vote at cap 3 rejected with visible message; offline banner appears when stream drops.

## Phase 4d — Alpine wiring: DnD, morph swaps, blind redaction

**Goal:** Local interactivity that survives remote swaps; blind collection that does not leak.

**TODO:**
1. All `x-data` scopes strictly ABOVE swap targets (board shell / column container, never inside `#card-{id}`); card fragments carry only bindings. Swaps use idiomorph (`hx-ext='morph'`, `hx-swap='morph:outerHTML'`); single `htmx:afterSwap → Alpine.initTree` re-init hook. Timer countdown lives outside any SSE-swapped subtree.
2. Drag-and-drop: HTML5 DnD, `PUT` on drop only; remote update mid-drag loses gracefully (snap-back), never corrupts.
3. Blind mode: server sends redacted fragments (`body='•••'`) to non-facilitators during hidden collect; facilitator stream gets full bodies; `reveal` broadcasts `cards-revealed` forcing full column refetch.
4. Tests: participant SSE payloads contain zero card bodies pre-reveal; morph swap preserves an in-progress draft (handler-level test with draft marker).

**DoD:** Playwright specs: blind collect → participant sees only placeholders until reveal; drag in context A unaffected by remote votes landing mid-drag; ngrok share (`ngrok http 8080`) smoke-checked manually once for a remote viewer.

**Review gate:** SSE-vs-websocket decision locked; load sanity (50 subscribers, no goroutine growth).

## Phase 5 — Facilitation: phases, hide/reveal, grouping, timer

**Goal:** Facilitator can run the retro end-to-end: collect → vote → discuss with controls EasyRetro parity.

**TODO:**
1. Board `phase` state machine: `collect → vote → discuss → done` (facilitator-only transitions, middleware from Phase 3).
2. Controls (facilitator panel, Alpine toggles): hide/reveal all cards (blind collection per Phase 4d redaction), lock/unlock voting, lock adding cards, sort column by votes, timer (`POST /b/{bid}/timer` sets server `timer_ends_at`; Alpine only renders countdown and re-syncs on `visibilitychange`; server broadcasts `timer-ended` event — never trust client clock).
3. Grouping: drag card onto card → `group_id = leader_id` (Phase 2 rule, vote sum = SUM over members); ungroup button.
4. Discuss mechanics: `focused_card_id` on board + `POST /b/{bid}/focus {card_id|null}`, next/prev endpoints (vote-desc order), `discussed` flag on cards, SSE `focus` event with spotlight UI — all viewers follow the facilitator.
5. Tests: non-facilitator POST to controls → 403; phase transitions broadcast SSE event; group vote sum correct; timer end fires server event even with slept client.

**DoD:** Playwright full-loop spec (facilitator context + participant context): blind collect → reveal → vote → sort → group → spotlight discuss (viewers follow focus, mark-discussed advances queue) → done, all live in both contexts; empty-state copy asserted on each surface.

**Review gate:** Confirm phase list and default vote cap; timer direction (server `ends_at` vs pure client) approved.

## Phase 6 — Discussion depth: comments, kudos, action items

**Goal:** Cards support comments, standalone kudos wall, action items with carry-over.

**TODO:**
1. Comments: `POST /cards/{cid}/comments`, rendered under card, SSE-updated with card fragment.
2. Kudos: board-level wall (`kudos` table), kudo cards (`to`, `body`, `from`), add/delete, included in export.
3. Action items: `POST /b/{id}/actions` (text, owner), check/uncheck, edit, delete; on new board creation offer "carry over open actions from board X" (copies with `carried_from`).
4. Tests: comment count, kudos CRUD, carry-over copies only open items with provenance + Playwright spec (comment appears live in second context).

**DoD:** Card with 2 comments, 3 kudos on wall, 2 actions (1 done) — realtime across two Playwright contexts.

**Review gate:** Kudos as column-type vs wall — confirm wall + template variant.

## Phase 7 — Export, archive UX, dashboard filters

**Goal:** Close the loop: export board, browse/filter past boards.

**TODO:**
1. Single export endpoint: `GET /b/{bid}/export.md` (markdown: columns → cards by votes → comments, kudos, open/done actions). Copy-to-clipboard button (Alpine + `navigator.clipboard`). No CSV, no Jira format — keep it simple.
2. Printable board view (print stylesheet → PDF via browser print, zero deps).
3. Dashboard: filter active/archived, search by name, card/action counts per board.
4. Tests: Go golden-file test on markdown content + Playwright spec (export downloads, markdown contains seeded card text).

**DoD:** Export a populated board; markdown pastes cleanly into Slack/Confluence; Playwright spec asserts download + content.

**Review gate:** Export formats sufficient (defer PDF/PNG/DOCX — YAGNI v1).

## Phase 8 — Ship: Docker, docs, hardening

**Goal:** One-container deploy a teammate can run; README covers local + ngrok sharing.

**TODO:**
1. Multi-stage `Dockerfile` (build: `go mod download` → `go generate ./...` → `CGO_ENABLED=0 go build -trimpath -ldflags='-s -w'`; final: `FROM scratch`, `WORKDIR /data` + `VOLUME`, `ENV PORT=8080 DB_PATH=/data/hindsight.db`, `EXPOSE`, `ENTRYPOINT`), `.dockerignore`, `docker-compose.yml` (volume + port).
2. `README.md`: quickstart (`docker run -v hindsight-data:/data -p 8080:8080 hindsight`), local `go run`, ngrok sharing (10–15s heartbeat survives idle; one EventSource per board page; client auto-resyncs), facilitator-token model, backup (copy the SQLite file), restore.
3. Hardening pass: input length caps, HTML escaping audit (templ escapes by default — verify no `Unsafe`), rate-limit vote/add endpoints (simple per-IP token bucket), `DB_PATH` default `./data/hindsight.db`.
4. Final `go vet`, full `go test ./...`, fresh-container smoke test.

**DoD:** From zero: `docker run` → create → share via ngrok → second person adds/votes live; `docker stop/rm` with volume → data survives restart; **containment assert** — the `scratch` image contains only the binary (no `static/`, no `migrations/`, no templates on disk) yet serves full UI + applies migrations + creates a board, proving zero filesystem dependency besides `/data`.

**Review gate:** Final go/no-go on v1 scope. Explicit v1 backlog (deferred, not built): native PDF/PNG/DOCX rendering, Jira/Slack API integrations, surveys/ROTI health checks, teams/orgs, custom CSS, accounts/auth hardening.

---

## Risks & tradeoffs

- **templ codegen friction** (extra `templ generate` step, editor support) vs `html/template` zero-deps: mitigated by Phase 1 review gate; both embed into one binary either way.
- **SSE connection limits** (browsers cap ~6 EventSources per origin; one stream per board page = fine) and ngrok free-tier connection timeouts: heartbeat + client auto-reconnect via SSE extension.
- **No-auth vote integrity:** display-name + cookie model is gameable (EasyRetro free boards share this trait); accepted for v1, documented; accounts deferred.
- **modernc.org/sqlite** is slower than CGO sqlite on heavy write loads — irrelevant for retro-sized traffic; buys pure-Go single binary.
- **Drag-and-drop + SSE swap races** (remote update while dragging): re-render wins, dragged card snaps back; acceptable v1, Alpine keeps drag local-only until drop POST succeeds.

## Refs

- EasyRetro features: https://easyretro.io/features (teams, templates, customize columns, comments, votes w/ per-person cap, drag-drop + merge, exports incl. Jira/Confluence, surveys, Slack notify)
- EasyRetro templates: https://easyretro.io/retrospective-ideas (Mad/Sad/Glad, Start/Stop/Continue, 4Ls, Kudos variants)
- htmx SSE extension: https://htmx.org/extensions/sse (`sse-connect`, `sse-swap`)
- Templ + HTMX: https://callistaenterprise.se/blogg/teknik/2024/01/08/htmx-with-go-templ
- Alex Edwards htmx+Go (stdlib patterns): https://www.alexedwards.net/blog/how-i-use-htmx-with-go
- Go frontend architecture 2026 (templ assessment): https://rajnandan.com/posts/go-frontend-architecture-2026
- SSE broker pattern in Go: standard mutex + per-client channel + Flusher (see Phase 4 task 1)
- pressly/goose embedded migrations: https://pkg.go.dev/github.com/pressly/goose/v3 and https://pressly.github.io/goose/blog/2021/embed-sql-migrations (`SetBaseFS` + `SetDialect("sqlite3")` + `Up`)
