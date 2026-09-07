# Hindsight — architecture (binding)

This document locks layout, interaction, and contract decisions for the
one-shot build. `AGENTS.md` is the daily working agreement; this file is
the detail behind it. The two must agree — if they conflict, this file wins
and `AGENTS.md` gets fixed.

## 1. System overview

```
browser ── htmx (mutations) ──────────────▶ net/http ServeMux
  │  ▲                                      │ handlers/ (parse, auth, render)
  │  │ SSE (one EventSource per board page) │   │            │
  │  └──────── realtime/ broker ◀───────────┘   │            ▼
  │      (publish only after DB commit)         ▼      db/ (transactions)
  │                        templates/ (partials) ◀── goose migrations at boot
  └────── Alpine.js (local UI state only, never authoritative)

SQLite file: the ONLY runtime disk read/write. Everything else is embedded.
```

## 2. Package layout and dependency directions

- `main.go` (package main, repo root): static-asset embed directive,
  config, `goose.Up`, broker registry, route wiring. Migration SQL embeds
  in `db/` via `goose.SetBaseFS`; together both embeds keep the binary
  self-contained. May import everything. Nothing imports it.
- `handlers/`: HTTP only. May import `db`, `realtime`, `templates`, stdlib.
  Must NOT import `database/sql`, the sqlite driver, or `text/template`.
- `db/`: persistence only. May import `database/sql`, driver, goose, stdlib.
  Must NOT import `handlers`, `realtime`, `templates`, or `net/http`.
- `realtime/`: SSE broker only, persistence-free. May import stdlib only.
  Must NOT import `db`, `handlers`, or `templates`.
- `templates/`: pure rendering. May import stdlib (+ `a-h/templ` runtime)
  and shared view-model types only. Must NOT import `db`, `realtime`,
  `database/sql`, or `net/http`.
- Shared request/response view models live in `templates` (e.g. `CardView`);
  `handlers` maps store rows to view models at the boundary.

These directions are machine-checked by `depguard` in `.golangci.yml`
(`make lint`, CI). Prose agreement alone is not enforcement.

## 3. Startup sequence and config

Order in `main()`: read env → `mkdir -p` for DB dir → `sql.Open` + PRAGMAs
(`journal_mode=WAL`, `busy_timeout=5000`, `synchronous=NORMAL`,
`foreign_keys=ON`) → `goose.Up` (embedded FS) → load-or-create server
secret (section 7) → build broker registry → wire routes + middleware →
`ListenAndServe`.

Config (env, all optional):

- `PORT` (default `8080`): HTTP listen port.
- `DB_PATH` (default `./data/hindsight.db`): SQLite file path. Use an
  absolute path in production; the DB directory is created `0700` because
  the file holds secret hashes.
- No other env. No config file. No flags v1.

## 4. Route contract

Auth column: `-` none, `P` participant cookie, `F` facilitator cookie.
`{bid}` is `boards.public_id`. Every mutation returns an HTML partial
(unless noted); full-page redirects only where stated. Swap targets use
`#card-{id}` / `#column-{id}` ids from section 6.

Boards and dashboard:

- `GET /` (`-`): `Dashboard` page (active + archived lists, counts, search).
- `GET /new` (`-`): `BoardNew` page (name, context, template picker,
  carry-over selector).
- `POST /boards` (`-`): create board + seed template columns. Sets both
  auth cookies (section 7), 303 redirect to `/b/{bid}`. The facilitator
  token is rendered exactly once on the landing response.
- `GET /b/{bid}` (`-`): join gate (name modal) when no participant cookie,
  else `Board` shell.
- `POST /b/{bid}/join` (`-`): upsert participant row (duplicate names get
  `name-2`, `name-3`…), set participant cookie, return board columns HTML.
- `POST /boards/{bid}/archive` (`F`): set `archived_at`. Returns dashboard
  row partial (board leaves active list).
- `POST /boards/{bid}/unarchive` (`F`): clear `archived_at`. Same partial.
- `POST /boards/{bid}/duplicate` (`F`): copy columns (+ optionally open
  actions with `carried_from`). 303 to the new `/b/{bid}`. The duplicator
  holds facilitator rights on the copy via the new facilitator cookie but
  has no participant row until joining, so they render facilitator-only
  (voteless, no display name) like any facilitator who never joined.
- `POST /boards/{bid}/regenerate` (`F`): rotate the facilitator token.
  The gate compares the presented (old) token in constant time; the
  swap is one conditional write, so concurrent rotations mint exactly
  one live token. Sets the new facilitator cookie and renders the raw
  token exactly once (same once-only fragment as creation).

Facilitation (`F` all; non-facilitator POSTs → 403 + flash, section 8):

- `POST /b/{bid}/phase` (`phase=collect|vote|discuss|done`): `phase` event.
- `POST /b/{bid}/reveal`: clear `cards_hidden`, broadcast `cards-revealed`
  (full column refetch, section 5).
- `POST /b/{bid}/lock` (`target=voting|cards`, `locked=0|1`): `column-*`
  refresh for affected columns.
- `POST /b/{bid}/timer` (`seconds=N`, `0` cancels): sets server
  `timer_ends_at`; clients render countdown only. Expiry is detected
  server-side (checked on each board read, on every SSE subscribe and
  heartbeat tick, plus one in-memory fire per armed board re-armed
  from the DB at startup) and broadcasts `timer-ended`. Never trust
  the client clock.
- `POST /b/{bid}/focus` (`card_id` or empty): sets `focused_card_id`,
  broadcasts `focus`. Viewers spotlight the card.
- `POST /b/{bid}/focus/next`, `/focus/prev` (vote-desc order, skipping
  `discussed=1`; marks current discussed): `focus` event.
- `POST /b/{bid}/sort` (`column_id`): reorders one column by vote
  totals (leaders rank by group sum); `column-{id}` refresh. Refused
  with 403 + flash while `cards_hidden=1`: vote order would leak the
  ranking through positions despite hidden counts.

Cards (`P`; state-gated: add requires `cards_locked=0`, vote requires
`voting_locked=0` and phase `vote`; violations → 403 + flash):

- `POST /b/{bid}/cards` (`column_id`, `body` ≤2000 chars): `card-added`.
- `PUT /cards/{cid}` (`body?`, `column_id?`+`position?` for moves,
  `group_id?` or `group_id=` empty to ungroup): `card-updated`.
- `DELETE /cards/{cid}`: `card-removed` (OOB delete).
- `POST /cards/{cid}/vote`, `/unvote`: phase/lock gates, cap, and
  double-vote checked inside the vote tx (section 9); broadcasts
  `vote-changed`. Cap/double violations → 403 + flash with the reason.
- `POST /cards/{cid}/comments` (`comment` ≤1000 chars): `card-updated`
  (comments render inside the card fragment).

Kudos, actions, export:

- `POST /b/{bid}/kudos` (`P`; `to` ≤200, `body` ≤500 chars, `from?`
  ≤100 chars and equal to the writer's display name): `kudos-added`.
- `DELETE /kudos/{kid}` (`P` author or `F`): `kudos-removed` (OOB delete).
- `POST /b/{bid}/actions` (`P`; `text` ≤500 chars, `owner?`): `action-added`.
- `PUT /actions/{aid}` (`P` author or `F`; `text?`, `owner?`, `done?`):
  `action-updated`.
- `DELETE /actions/{aid}` (`F`): `action-removed` (OOB delete).
- `GET /b/{bid}/export.md` (`F` always; `P` only when revealed):
  markdown download (columns → cards by votes → comments, kudos,
  open/done actions). Facilitator-cookie holders are admitted with no
  participant row; participant-cookie holders are admitted only when
  `cards_hidden=0`, else 403 + flash (section 8).
- `GET /b/{bid}/events` (`P` or `F`): SSE stream (section 5).
  Facilitator-only viewers (no participant cookie) stream too.

## 5. SSE protocol

- Exactly one `EventSource` per board page: the board shell holds
  `sse-connect="/b/{bid}/events"`. One listener per column
  (`sse-swap="column-{colID}"`, server sends `event: column-{colID}`),
  plus board-level listeners for the control events below.
- Every message carries a per-board monotonic `id:`. Broker keeps a
  ~100-event ring buffer per board. `GET` with `Last-Event-ID` replays the
  gap; with no (or too-old) id the server emits `sync-required` and the
  client refetches all columns via `hx-get`.
- Slow subscriber: mark-dirty + `sync-required`, never silent drop.
  Heartbeat comment (`:ping`) every 12s. Headers on every write:
  `Content-Type: text/event-stream`, `Cache-Control: no-cache`,
  `X-Accel-Buffering: no`, plus explicit `Flush()`. The stream extends
  its write deadline (`ResponseController.SetWriteDeadline`, two
  heartbeats ahead) on every write, so the server-wide `WriteTimeout`
  bounds slow writers without cutting healthy long-lived streams.
- Publish to the broker only after the DB tx commits, in this order:
  commit → publish → return HTTP response.

Event catalog (name → payload → client effect):

- `column-{colID}` → rendered `Column` HTML → morph-swap `#column-{colID}`.
  Used for `card-added`, and any column-affecting change.
- `card-updated:{id}` / `vote-changed:{id}` → rendered `Card` HTML →
  morph-swap `#card-{id}` outerHTML only (never clobbers sibling state).
- `card-removed:{id}` → empty body with
  `<div id="card-{id}" hx-swap-oob="delete"></div>` → node deleted.
- `kudos-added` → rendered `KudosList` HTML → morph-swap `#kudos-list`
  (whole wall, so actor and viewers converge on one entry).
- `kudos-removed` → empty body with
  `<div id="kudo-{id}" hx-swap-oob="delete"></div>` → node deleted.
- `action-added` → rendered `ActionsList` HTML → morph-swap
  `#actions-list` (whole wall, same convergence).
- `action-updated` → rendered `ActionsList` HTML → morph-swap
  `#actions-list`; the actor's own response is the single `ActionItem`
  node (`#action-{id}`) morphed in place.
- `action-removed` → empty body with
  `<div id="action-{id}" hx-swap-oob="delete"></div>` → node deleted.
- `cards-revealed` → client refetches all columns (`hx-get`); ends blind mode.
- `timer` → `{"ends_at":…}` (null on cancel) → viewers render the
  countdown to the instant, or idle.
- `phase`, `focus`, `timer-ended` → small JSON payloads (`{"phase":"vote"}`,
  `{"card_id":12}`, `{"at":"…"}`) → Alpine store update + spotlight UI.
- `sync-required` → `data: reload` → client refetches all columns.
- Stale `id:` values are ignored client-side (last-applied-wins per board).

## 6. Frontend rules

- Client→server traffic is plain htmx requests only. Column-level refreshes
  are for explicit facilitator actions (sort, reveal, lock); routine votes
  use per-card swaps.
- All swaps use idiomorph (`hx-ext="morph"`, `hx-swap="morph:outerHTML"`).
  Single global hook: `htmx:afterSwap → Alpine.initTree`. Timer countdown
  lives outside any SSE-swapped subtree and re-syncs on `visibilitychange`.
- All Alpine `x-data` scopes sit ABOVE swap targets (board shell / column
  container, never inside `#card-{id}`); card fragments carry bindings only.
- Vote button FSM: `idle → pending → confirmed | denied`. `denied` renders
  the flash message from section 8. Connection badge follows SSE
  open/error events; while disconnected, poll `hx-get` every 5s.
- Drag-and-drop is HTML5 DnD with a single `PUT` on drop. A remote update
  landing mid-drag loses gracefully (snap-back via column refetch), never
  corrupts: the drop `PUT` carries the pre-drag `position`, and a 409 on
  stale position triggers refetch + retry hint.
- Blind mode redacts server-side: while `cards_hidden=1`, non-facilitator
  streams and `GET` fragments render `body="•••"` with real ids/positions.
  Exactly what redacts: card body/author/vote count, the viewer's voted
  and discussed flags, comment bodies/authors, and kudo bodies/senders.
  What stays visible by design: card ids/positions/counts, comment
  counts/ids, grouping structure, kudo recipients, and the action wall
  (never redacted). Facilitator stream gets full bodies on a separate
  filtered publish path. Never hide bodies with CSS alone.

## 7. Identity and cookies (v1, no accounts)

Tokens are 128-bit `crypto/rand`, base64url-encoded. Only hashes persist:
`SHA-256 hex` of the raw token in `boards.facilitator_token_hash` and
`participants.token_hash`. Facilitator-token comparisons use
`subtle.ConstantTimeCompare`; participant resolution looks up the
`token_hash` equality in SQLite (hash comparison, not a raw-secret
compare). Gameability (cookie replay, name impersonation) is accepted for v1 and
stated in the README; no hardening beyond this section.

- Server secret: 256-bit random, persisted in `app_meta(secret)` (created
  on first boot, cached in memory). Used ONLY for cookie HMACs, never as
  an auth token itself.
- Two cookies, both `Path=/`, `HttpOnly`, `SameSite=Lax`, `Secure` when
  served over TLS (ngrok counts), 1-year expiry, values HMAC-signed with
  the server secret (`base64url(json).base64url(hmac)`):
  - `hindsight_fac`: `{boardID: facilitatorToken}` map. Presence of a
    matching token grants facilitator rights for that board.
  - `hindsight_parts`: `{boardID: participantToken}` map. Token resolves
    to a `participants` row; votes key to that row.
  - `hindsight_reveal`: `{boardID: facilitatorToken}` map, single-use,
    10-minute expiry. Written at board creation/duplication so the landing
    response can render the facilitator token exactly once; consumed and
    cleared on the next `GET /b/{bid}`. Same HMAC format, no extra rights:
    it only re-presents the facilitator token the holder was just issued.
- Cookie hardening: values are size-capped (~8KB reject), entries capped
  (50 boards per cookie; a full map resets to the single new entry), and
  `Secure` follows `r.TLS` only — `X-Forwarded-Proto` is ignored, so proxy
  deployments must forward over TLS. A tampered cookie fails closed and
  the next write/drop logs out every board in that cookie.
- `boards.public_id`: 128-bit random, appears in `/b/{bid}` URLs.
- Display name: chosen at join, stored on the participant row; duplicates
  suffixed (`ana`, `ana-2`, …). Requests without a participant cookie are
  voteless: any `P` route without one returns 403 + flash (join first).
- Facilitator token is shown once at board creation (the `POST /boards`
  response). Regeneration issues a new token and invalidates the old hash;
  the regeneration response is the only other place the raw value appears.

## 8. Error contract

- Machine status + human fragment, always together. On 403/429 the handler
  returns the status code with an HTML body containing exactly one OOB node:
  `<div id="flash" hx-swap-oob="true" class="flash-error">…reason…</div>`
  and nothing else. The vote-button FSM treats any 4xx as `denied` and
  renders the flash text. No JSON error envelopes v1.
- 404s (unknown board/card id) return plain `http.NotFound`, no flash.
- 409 (stale-position drop, section 6) returns the fresh `Column` partial
  with status 409 so the client refetches and shows a retry hint.
- Input caps (section 4 lengths) are enforced server-side; over-length
  bodies → 422 + flash. All rendering escapes by default; `templ.Unsafe`
  (or `template.HTML`) is forbidden — `grep -r Unsafe` must be empty.

## 9. DB conventions

- goose owns versioning (`goose_db_version`); migrations are up-only SQL,
  `NNNNN_name.sql` naming, embedded via `goose.SetBaseFS`. No hand-rolled
  runner, no `DOWN` files v1.
- Every mutation goes through `WithTx` (`BEGIN IMMEDIATE`). The vote tx does
  cap check + existing-row check + counter update atomically; double-vote
  is rejected by the `PRIMARY KEY(participant_id, card_id)` constraint as
  the backstop, mapped to 403.
- Grouping: `cards.group_id = leader_id` (self-FK, `ON DELETE SET NULL`).
  Group vote sum = `SUM(votes)` over members, computed in the read query,
  never stored. Ungroup = set `group_id` NULL.
- Archive is a flag (`archived_at`), never a delete. `duplicate` copies
  columns and optionally open actions (setting `carried_from`); it never
  copies votes, comments, or kudos.
- Key tables: `boards`, `columns`, `cards`, `votes`, `comments`, `kudos`,
  `actions`, `participants`, `app_meta`. Indexes on all FK columns.
  `CHECK(votes_per_person > 0)`.

## 10. Test seams

- `handlers` depends on consumer-side interfaces defined in `handlers`,
  implemented by `db` and `realtime` — never on concrete store/broker types
  directly. Minimum seams: a `cardStore`-style interface per aggregate and
  an `eventPublisher` with `Publish(boardID string, name string, html string)
  error`. Fake implementations live in `handlers/*_test.go`.
- Broker surface (implemented in `realtime`, faked in handler tests):
  `Subscribe(boardID string, lastID uint64) (<-chan Event, replay []Event)`,
  `Publish(boardID, name, html string) uint64` (returns the assigned `id:`),
  `Unsubscribe(boardID string, ch <-chan Event)`. `Event` carries
  `{ID uint64, Name, HTML string}`.
- Store tests use `t.TempDir()` DBs with `goose.Up` on each; must cover:
  fresh-apply, re-run no-op, double-vote rejection, cap race (N goroutines,
  cap holds), 20 concurrent writers with no `SQLITE_BUSY`.
- E2E (Playwright, chromium, two contexts): A acts → B observes <1s;
  blind-collect redaction; full facilitation loop; export download content.
  No manual-click verification, ever.

## 11. Locked decisions (do not relitigate mid-build)

- `templ` for rendering, `html/template` fallback only if codegen blocks.
- htmx 2.x pinned (NOT 4.x beta); SSE via `htmx-ext-sse` 2.2.4; morph via
  idiomorph 0.7.4; Alpine 3.15.x; PicoCSS v2.
- `modernc.org/sqlite` (pure Go, `CGO_ENABLED=0`) + `pressly/goose/v3`.
- No websockets. One `EventSource` per board page. Server owns the timer.
- Kudos is a board-level wall (+ template variant), not a column type.
- Export is markdown only; printable view via print stylesheet. No CSV,
  PDF rendering, Jira/Slack integrations, surveys, teams, or accounts v1.
