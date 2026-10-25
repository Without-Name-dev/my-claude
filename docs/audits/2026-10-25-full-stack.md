# Full-stack audit — 2026-10-25

Sixteen read-only audit units across the Go server, the SQLite store, the
connector and bridge, the web client, the iOS client, the phone-pane plugin and
the cross-cutting seams. Every claim was re-checked by a second agent before it
reached this plan: 149 raw findings, 147 survived.

Status legend used below: nothing here is fixed unless the commit log says so.

# Execution Plan

Root: `C:/Users/25640/my_claude` (paths below are relative to it). Plan follows the verified reasons, not original severities.

## 1. Blockers

- **Signing key** — `go/internal/auth/auth.go:43`: public fallback HMACs user/signed tokens and `dummyPasswordSalt`. Fail startup unless `AGENT_SERVER_SECRET` is set; no constant fallback.
- **Session/password** — `go/internal/server/auth_handlers.go:248,356`: `AA_SESSION_TTL` unused; password change needs no current credential or revocation. Pass configured TTL; require current credential; enforce per-user credential version in `authctx.go:63`.
- **Setup/rand** — `auth_handlers.go:476`; `authctx.go:38`: setup expiry never validated; rand failure mints fixed token. Check expiry; return 500 on rand failure.
- **Pairing** — `pairing_handlers.go:32,110,234`: anonymous unbounded-TTL start burns globally-unique codes; claim inserts before CAS; allocator retries only pending collisions. Clamp TTL, auth/rate-limit, reap expired, CAS first, retry full insert on UNIQUE.
- **Login DoS** — `auth_handlers.go:221`: no throttle; 120k PBKDF2 per attempt. Add per-IP/account 429 backoff.
- **Session frames** — `projector.go:241,282,380`; `session_handlers.go:574`: delta/tool/end/cancel skip ownership; turn.end restamps orderSeq; resolved approvals unpublished. Add ownership checks, keep turn-start orderSeq, publish notice.updated + snapshot/status.
- **History/share/export** — `history.go:82`, `share_handlers.go:177`, `session_handlers.go:214`, `store/timeline.go:171`: blind-upsert import, oldest-2000 share, no hasMore/head nextSeq, revision-only cursor. Reconcile imports; latest-first paging; limit+1 hasMore; compound `(updated_seq,order_seq,id)` cursor.
- **Realtime loss** — `broker.go:81,98`: no per-session publish order; full queue drops frames silently. Ordered per-session queue; per-subscriber refetch emitting `EventRefetch`.
- **Presence** — `connector_hub.go:218,312`: offline unpublished; beats unswept; sessions stuck running. Sweep stale beats, publish offline, reset running sessions.
- **fs size** — `files_handlers.go:523`: 30s budget but connector hashes whole file. Separate budget or cheap stat.
- **Migration** — `migrations/0005_session_identity.sql:58`: duplicate fold cascade-deletes attachments, dangles terminals, orphans blobs. Repoint/record moves before delete; follow-up migration if shipped.
- **Connector wedge/adoption/truncation** — `connector/turn.go:136,145,190`; `bridge/bridge.go:132,304`; `client.go:313`: stale outbox extends deadline forever; empty external id wildcards adoption; 20k-truncated turn.end overwrites text; single `pendingEnd`. Advance deadline only on TS change, fail closed on empty id, preserve full text, queue finals.
- **Queue drop** — `client.go:208`: 32-turn queue drops acknowledged submits. Apply backpressure or emit failure.
- **Web gap** — `web-next/src/components/session-detail.tsx:989`: cursor jumps dropped frames. Detect gaps; recover `/events` from pre-gap cursor.
- **iOS retry** — `ios/Agents Anywhere/Agents Anywhere/Business/V2StreamCoordinator.swift:252`: post-connect failures retry forever; auth/404 swallowed. Classify with `V2ClientFailure`; fallback or terminal state.

## 2. Correctness

- **srv-auth**: `auth.go:72` iterations not stored → encode and rehash legacy. `auth_handlers.go:165` register lacks caps → shared validator. `:207` 409 enumeration → uniform response.
- **srv-sessions**: `projector.go:367` approvals never expire → purge on turn.end/hello/disconnect. `session_handlers.go:162` meta/bulk writes publish nothing → `publishSessionMeta` + notify. `:225` tie-group cursor loss → composite cursor. `projector.go:156` Rev unused → enforce last+1.
- **srv-connector**: `files_handlers.go:936` headers before first chunk → fetch first, abort socket later. `rpc.go:191` fs/list skips containment → enforce root both sides. `inventory.go:31` missing never restored → set visible. `terminal.go:236` no eviction → delete on exit/delete. `files_handlers.go:492` 2MiB cap on 256MiB route → exempt.
- **srv-realtime**: `stream_ws.go:270` shared write mutex → coalesce/prioritize. `dashboard_ws.go:160` errors become empty snapshot → return/omit. `:190` build failure reported success → retry. `stream_ws.go:260` pre-subscribes all sessions → lazy subscribe/cache.
- **store-a**: `timeline.go:132` non-unique cursor drops ties → composite key. `sessions.go:571` reset clobbers new turn → conditional update in one tx. `timeline.go:42` item_time uses raw OrderSeq → resolved. `timeline.go:90` oldest-first share → newest-first. `users.go:62` double-admin race → atomic first-user insert + consume token. `sessions.go:452` source_state never restored → set visible. `db.go:138` lock map growth → striped locks.
- **store-b**: `connectors.go:82` disabled users' connectors authenticate → join `users.disabled`, close sockets. `attachments.go:170` cleanup misses unindexed blobs → sweep session dirs/sidecars. `admin.go:382` admin password reset always 422 → accept verifier/salt. `connectors.go:55` device_os never persisted → store `hello.platform`. `admin.go:62` runningTurns counts offline → reset on disconnect. `terminals.go:74` arbitrary session id → validate/FK. `shares.go:174` >32766 binds → chunk 500.
- **con-client**: `client.go:380` abort only active session → pending per-session aborts. `:284` submit failure silent → emit failed turn.end. `turn.go:125` timeout leaves queued prompt → cancellation marker. `client.go:432` Ask ignores external id → forward. `turn.go:159` any new turn adopted → correlate message id.
- **con-bridge**: `bridge.go:470` SubmitCommand lacks external id → add end-to-end. `protocol/connector.go:162` Abort lacks external id → add. `connector.go:15` version/pre-hello unvalidated → validate, close unknown.
- **web-session**: `session-detail.tsx:837` older page merges into current session; send/selection rollbacks same → capture session/visit token. `:1661` prune leaves hasMore false → restore true. `:1753` unbounded timeline rebuilds → bound/virtualize. `task-composer.tsx:818` failed create strands optimistic session → home/clear/retry. `:773` old state under new id → scope state/layout reset. `:1019` capability read dead → snapshot-based/emit meta. `:1402` failed optimistic never dismissed → dismiss/retry same id.
- **web-shell**: `workspace-context.tsx:1298` ghost optimistic messages → remove on failure, clear on token reset; `:1158` unhandled/stale pin → catch + sessionsRef; `:903` read overwrites newer state → updatedSeq guard. `app-sidebar.tsx:165` >200 mark-read rejected → chunk/await/catch. `device-page.tsx:375` rename draft reset → sync only on connector change; `:430` unknown id spins → not-found; `:479` delete unhandled → catch/toast; `:660` >200 archive rejected → chunk; `:1007` selection survives tab → prune. `team-page.tsx:136` filters one page → server-side. `service-page.tsx:330` hidden refresh failure → inline error.
- **web-lib**: `dashboard/api.ts:917` runtime controls hit 501 → hide/implement; `:520` downloadBlob bypasses auth path → ApiError. `scripts/generate-protocol-types.mjs:14` ignores could drift → verify sha pins/add Go conformance.
- **ios-api**: `V2DevicePairing.swift:7` claim orphans pre-created connector → adopt or stop pre-creating. `V2Project.swift:31` manuallyCreated dropped → pass/remove. `V2TimelineItem.swift:204` reads name/toolName, wire uses `tool` → read `tool`.
- **ios-services**: `V2StreamCoordinator.swift:206` no suspend/resume → wire connectivity/background; `:324` unbounded replay bytes → skip observers/byte-bound. `V2DashboardRepository.swift:81` generation bumps every snapshot → only on change. `V2SessionRepository.swift:401` nil projection restarts socket → guard previous; `:135` dirty cleared before silent drop → clear after success.
- **ios-views**: `SessionTimelinePresentation.swift:122` subagent stuck running → reset/refetch on reconnect. `ChatTimelineView.swift:315` offset includes tail growth → measure page. `ServiceEntryView.swift:14` QR login unreachable → entry or delete.

## 3. Cleanups

- Delete `store.RevokeConnector`, vacuous `revoked=0` filters, unused `Envelope.Seq`, nine dead DashboardApi terminal methods, iOS mobile-login/email dead models. Net **−400 to −600 lines**.
- Consolidate registration/admin/profile validation and attachment/history deletion helpers. Net **−80 lines**.
- Replace never-pruned session lock map and terminal replay retention with bounded structures. Net **−20 lines**.
- Remove per-snapshot `new Error().stack` logging. Net **−6 lines**.

## 4. Missing features (proved absent)

- Email verification/reset pipeline: no `/auth/email-code`, API key discarded, `body.Code` unread.
- Web OAuth `/auth/oauth/start|callback|finalize`.
- Mobile QR login endpoints plus iOS entry point.
- Admin password-reset path.
- Runtime management (all routes 501).
- AskUserQuestion/ExitPlanMode rendering and `updatedInput` forwarding.
- `session.refetch_required` emitter and iOS activity resync call.
- Persist `device_os`; restore `source_state` on rediscovery.

## 5. Execution order

0. **Serial contract lock**: `go/internal/protocol/connector.go` + realtime docs — externalSessionId on submit/command/abort, turn.end text semantics, approval input schema/updatedInput, refetch/notice frames, command `raw`. All tracks depend on this.
1. **Parallel tracks**:
   - Auth: `auth.go`, `auth_handlers.go`, `authctx.go`, `config.go`, `pairing_handlers.go`, `store/users.go`; no session-file overlap.
   - Server sessions/realtime: `projector.go`, `session_handlers.go`, `history.go`, `share_handlers.go`, `broker.go`, `stream_ws.go`, `dashboard_ws.go`, `events.go`; one owner each for `session_handlers.go`/`projector.go`.
   - Store: `timeline.go`, `sessions.go`, `connectors.go`, `admin.go`, `attachments.go`, `terminals.go`, `shares.go`; cursor signature coordinated with server track, no parallel `timeline.go` edits.
   - Connector: `turn.go`, `client.go`, `bridge.go`; after phase 0.
   - Web: one owner each for `session-detail.tsx`, `workspace-context.tsx`, `device-page.tsx`, `dashboard/api.ts`; otherwise parallel.
   - iOS: coordinator + repository one owner; remaining files parallel.
2. **Serialize last**: migration `0005` and any follow-up (irreversible; requires blob/data review).
3. Merge order: contract → server core → connector → web/iOS; auth/store independently.

## 6. Deliberately excluded

- `_deprecated/`, `_reference/`, Python `server/` and `connector/`, `node_modules/`, `go/dist/`, `web-next/out/` per instructions.
- Deferred low batch: `UpsertTimelineItem` zero-path, 32k bind limit, `TakeoverResponse.serverTime`, unknown-enum decode, protocol hash guard, accessibility label, team-page pagination.
- Findings whose verification downgraded impact: fs truncation (client EOF), fs/list containment (owner-only), pairing orphan (clutter), legacy dashboard socket (fallback), iOS ordering (self-heals), 501 runtime controls (contract gap).
- No builds, tests, or deploy commands were run.
