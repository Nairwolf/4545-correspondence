# Phase 2 implementation plan — Identity

## Context

Phase 1 (spec §12: read-only parity) is built and closed out; its plan
lives in git history (`git show 462c86b:PLAN.md`). Phase 2 is **identity**
(spec §12): Lichess OAuth, the registration flow, the admin registration
queue, sessions and roles — spec §3.1, §8.2, the "Registration queue"
item of §8.5, and the security bullets of §11. It replaces
`ic seed-players` as the way players enter the league, and it is what
every later phase authenticates against: the player dashboard (Phase 3),
admin round management (Phase 4), and bulk pairing, which needs a
`challenge:write` token per player (Phase 5).

What Phase 2 deliberately does **not** build: the player dashboard and its
toggles (Phase 3); notifications, including "registration approved"
(Phase 5 — the applicant sees their status on `/account` instead); the
`validate-tokens` job and token-health display (Phase 3/5 — the probe
endpoint is recorded below); admin player management beyond the queue
(pause, ban, grant admin from the UI — §8.5, later); the settings UI
(Phase 6); the GDPR self-deletion path (Phase 3, with the other
self-service actions — the token-revocation call it needs is built here).

### What Phase 1 already provides (reused, not rebuilt)

- `users` / `player_profiles` / `rating_snapshots` / `audit_log` tables
  with the spec's enums (`internal/db/migrations/00002`, `00003`, `00009`);
  `users.status` already has `pending / approved / rejected / banned` and
  `users.role` has `player / admin`.
- Queries `GetUserByLichessUserID`, `CreatePlayerProfile`,
  `InsertRatingSnapshot`, `CreateAuditLogEntry`
  (`internal/db/queries/players.sql`, `audit.sql`).
- `ratingSnapshotParams(userID, lichess.User)` in `cmd/ic/lichess_mapping.go`
  — maps `perfs` to a snapshot row; the callback reuses it (move it to
  `internal/lichess` or `internal/standings` so `web` can call it — see
  step 3).
- `standings.Recompute(ctx, q, userID, cfg)` (`internal/standings/standings.go`)
  — approval calls it so a new member appears on `/standings` at once.
- `lichess.Client.do` (`internal/lichess/client.go`) — serialised,
  429-aware HTTP; gains a per-call bearer token. `lichess.Fake` is the
  test double every consumer uses.
- `web.Server` + `newServer` + the tx-scoped `testServer` integration
  harness (`internal/web/web_integration_test.go`), `render`, `base`,
  `layout.html` with `hx-boost="true"` on `<body>`.
- `internal/config` reads env with plain `os.LookupEnv` (the precedent
  for adding five more variables without a library).

---

## Lichess OAuth — verified (source: `lichess-org/api` OpenAPI v2.0.171, fetched 2026-09-15)

Verified against the repo's YAML sources (`doc/specs/tags/oauth/*.yaml`,
`schemas/User.yaml`, `schemas/Count.yaml`, `schemas/OAuthError.yaml`), not
the rendered docs. Items marked ⚠ contradict or go beyond the spec and
become spec amendments.

| Item | Verified value |
|---|---|
| Flow | Authorization Code **with PKCE**; "The only accepted code challenge method is `S256`." "Lichess supports unregistered and public clients (no client authentication, choose any unique client id)." |
| ⚠ Client secret | **None.** `client_id` is "an arbitrary identifier that uniquely identifies your application" — nothing is registered on Lichess. `LICHESS_CLIENT_SECRET` in spec §2.2 has no use and is dropped. |
| `GET /oauth` | `response_type=code`, `client_id`, `redirect_uri`, `code_challenge_method=S256`, `code_challenge=BASE64URL(SHA256(code_verifier))` (all required); `scope` (space-separated), `state`, `username` hint (optional). Success redirect: `code`, `state`. Failure redirect: `error` (`access_denied` when the user cancels), `error_description`, `state`. |
| `POST /api/token` | `application/x-www-form-urlencoded`: `grant_type=authorization_code`, `code`, `code_verifier`, `redirect_uri`, `client_id` (all required) → `{token_type: "Bearer", access_token, expires_in}`; 400 → `{error, error_description}`. |
| ⚠ Token lifetime | "Access tokens are long-lived (expect one year), unless they are revoked. **Refresh tokens are not supported.**" `OAuthToken.refresh_token` (spec §4.1) can never be populated and is dropped; `expires_at = issued_at + expires_in`. |
| `GET /api/account` (bearer) | `UserExtended` = `User` + extras. `User`: `id`, `username` (required); `perfs` (the shape Phase 1 already decodes), `createdAt` (epoch ms, int64), `seenAt`, `disabled` ("only appears if a user's account is closed"), `tosViolation` ("only appears if … marked for the violation of Lichess TOS"), `verified`, `patron`, `profile`. `count` (`Count.yaml`): `all`, `rated` (optional), `win/draw/loss`, … — everything §8.2's admin signals need, in one call. `internal/lichess/testdata/user_thibault_live.json` is already this shape (captured from `/api/user/{name}`, same `User` schema). |
| `POST /api/token/test` | Body: up to 1000 tokens, comma-separated → per token `{userId, scopes (comma-separated string), expires (ms or null)}` or `null` if invalid. This is the token-health probe for Phase 3's authorisation status and Phase 5's `token.invalid_grace_days`. Not needed in Phase 2. |
| `DELETE /api/token` (bearer) | Revokes the bearer token; 204. Used when a sign-in produces a token we will not keep, and by the Phase 3 deletion path. |
| ⚠ Consent granularity | The consent screen is all-or-nothing for the requested `scope`. "Optional `msg:write`, which the player may decline" (§8.2 step 2) therefore has to be a checkbox on **our** join page that decides which scope string to request — a player cannot untick one scope on Lichess. |

**`golang.org/x/oauth2`** (mandated, §2.1; latest v0.37.0, `go 1.26`)
supports this flow natively: `oauth2.GenerateVerifier()`,
`oauth2.S256ChallengeOption(verifier)` on `AuthCodeURL`,
`oauth2.VerifierOption(verifier)` on `Exchange`, and
`Endpoint.AuthStyle = oauth2.AuthStyleInParams` so `client_id` goes in the
form body as Lichess requires. Verified in its source: with
`AuthStyleInParams` and an empty `ClientSecret` it sends **no**
`client_secret` and no Basic-auth header. Its `go.mod` requires exactly
one module (`cloud.google.com/go/compute/metadata`, imported only by the
`google` subpackage we don't use — it lands in `go.sum`, not in the
binary). We call exactly `AuthCodeURL` and `Exchange`; nothing refreshes a
token because nothing can. (Honest note for the dependency record: the
whole flow is ~60 lines of stdlib. The library is used because §2.1
mandates it and its PKCE/`Exchange` handling is well-trodden, not because
the stdlib couldn't do it.)

**Go 1.27 stdlib covers the rest** — no other new dependency:
`net/http.CrossOriginProtection` (Go 1.25+; verified present via
`go doc`) for CSRF, `crypto/aes` + `crypto/cipher` (AES-256-GCM) for
tokens at rest, `crypto/rand` for session ids, `crypto/hmac` for the
short-lived OAuth-state cookie, and a ~50-line per-IP token bucket for
the auth rate limit (§11) — production is one process, so an in-memory
limiter is exactly right.

---

## Decisions (answered by the maintainer, 2026-09-15)

1. **Timezone field (spec §14.4) — dropped.** Registration is one
   screen: fair-play agreement + "Continue with Lichess".
   `player_profiles.timezone` stays as a nullable, unused column so
   nothing is migrated away; a later "deadlines in local time" feature
   can start filling it.

2. **Agreement before OAuth, not after (§8.2 step order) — agreed.** The
   spec creates the `User` on callback and then shows a form. With
   timezone gone the form is one checkbox, and collecting it *before*
   the redirect means the callback creates a complete application in
   one transaction: there is never a half-registered user, nothing to
   clean up, and the access token is never held between two requests.
   The checkbox state travels in the signed OAuth-state cookie.

3. **`msg:write` — deferred to Phase 5.** There are no users yet, so
   nothing is gained by asking now. Phase 2 requests exactly
   `challenge:write`; the join page has no PM checkbox. When
   notifications ship, players who opt into Lichess PMs re-authorise
   through the same one-click `/login` flow with the wider scope (§3.1's
   remedy path doubles as the upgrade path). `oauth_tokens.scopes` is
   still stored so Phase 5 can tell who has granted what.

4. **Existing members sign in; they don't re-apply — agreed, with a
   note.** The callback matches on `lichess_user_id`; an existing
   `approved` row just gains a token and a session. The maintainer will
   **not** seed the roster at public launch, so this is simply the
   returning-member path and `seed-players` becomes a dev/testing tool.
   `fair_play_agreed_at` is NULL only for seeded rows; it is
   informational, not a gate.

5. **Bootstrap admins via `ADMIN_LICHESS_USERNAMES`, applied in the
   callback, with one creation path — agreed.** A listed username that
   already has a row is promoted to `admin` on sign-in. A listed
   username with no row goes through `/join` like everyone else — the
   callback then creates the row directly as `approved` + `admin` (the
   first admin has nobody to approve them). `/login` for a listed
   username with no row gets the same "you're not a member yet — join
   here" page as anyone. Both promotions are audited
   (`auth.bootstrap_admin`). Grant/revoke admin from the UI is §8.5
   player management, later.

6. **Rejected applicants cannot re-apply themselves — agreed.** Signing
   in again shows them the rejection reason on `/account`. The queue has
   a "rejected" tab from which an admin can still approve. Banned users
   get no session at all.

7. **Registration is open to any Lichess account — yes.** The rate
   limiter and the queue's signals are the defence.

8. **`/jobs` moves behind admin — yes.** `/admin/jobs`, `requireAdmin`;
   `/health` stays public for monitoring. Nav shows "Jobs" only to
   admins.

**Process:** implemented in this session by the same model (not an Opus
subagent), following the build order below and **stopping after each
step** for the maintainer to review and commit.

---

## Registration and sign-in flow (as built)

```
GET  /join                    join page: what the league is, fair-play rules,
                              [x] I agree to the fair-play rules (required)
                              [Continue with Lichess]
POST /join                    validates the checkbox, sets ic_oauth cookie
                              {state, verifier, intent=join},
                              302 → https://lichess.org/oauth?scope=challenge:write&…
GET  /login                   same, intent=login (no page: 302 straight to Lichess)
GET  /auth/lichess/callback   the one place the two intents converge
POST /logout                  deletes the session row, clears the cookie
GET  /account                 (auth) status page — see below
```

**htmx detail that will bite otherwise:** `layout.html` boosts every link
and form (`hx-boost="true"` on `<body>`). A boosted `POST /join` or
`GET /login` would be an XHR that follows the 302 to `lichess.org` and
dies on CORS. The join form and the sign-in link carry
`hx-boost="false"` so they are plain top-level navigations. The logout
form and every admin form stay boosted.

`ic_oauth` is an HMAC-signed (`SESSION_SECRET`), 10-minute, httpOnly,
SameSite=Lax cookie holding `{state, verifier, intent, expires}`.
Lax, not Strict: the callback is a top-level GET navigation *from*
lichess.org and Strict would drop the cookie on exactly that request.

**Callback**, in order, all writes in one transaction:

1. Verify `state` against the cookie; a mismatch, a missing cookie or an
   expired cookie is a 400 with no writes. `error=access_denied` renders a
   friendly "you cancelled on Lichess" page. Clear the cookie either way.
2. Exchange the code (PKCE verifier from the cookie) → access token +
   `expires_in`.
3. `GET /api/account` with that token → profile. Its `id` is the
   identity; its `username` is the canonical casing.
4. Look up `users` by `lichess_user_id`:
   - **exists** — update `lichess_username` if Lichess renamed them
     (audit `user.rename`); upsert `oauth_tokens` (audit
     `auth.reauthorise` when a row existed, `auth.sign_in` otherwise);
     refresh `lichess_profile`. `banned` → revoke the token we just got
     (`DELETE /api/token`), render the banned page, no session.
   - **absent, intent=join** — create `users` (`pending`,
     `fair_play_agreed_at = now()`), `player_profiles`, `oauth_tokens`, a
     `rating_snapshots` row from `perfs` (reuses Phase 1's
     `ratingSnapshotParams`), audit `registration.apply`.
   - **absent, intent=login** — revoke the token, render "you're not a
     member yet — join here". No row.
5. Bootstrap-admin check (`ADMIN_LICHESS_USERNAMES`, case-insensitive
   against the Lichess `id`): promote, or — for a row created in step 4 —
   approve and promote; audit `auth.bootstrap_admin`.
6. Create a session, set `ic_session`, redirect to `/account` whatever
   the status (the page explains pending / approved / rejected).

Every re-authorisation (§3.1's "one click" remedy) is just `/login`
again: step 4's upsert replaces the token. The button on `/account`
links there.

**`/account`** (Phase 2 minimum; Phase 3 grows it into the dashboard):
status with plain-language next steps (pending: "an admin reviews
applications, usually within a few days"; rejected: the reason;
approved: link to the profile page), Lichess authorisation block (scopes
granted, expiry date, re-authorise button), sign-out. Never renders the
token (§11).

**Nav**: anonymous → "Sign in" + "Join"; signed in → username linking to
`/account`, "Sign out", and "Admin" (and "Jobs", per decision 8) for
admins. The home page gets a short "Join the league" call-out linking to
`/join`.

---

## Admin registration queue (§8.5)

```
GET  /admin                              302 → /admin/registrations
GET  /admin/registrations?tab=pending    default; also tab=rejected
POST /admin/registrations/{id}/approve
POST /admin/registrations/{id}/reject    form field: reason (required)
POST /admin/registrations/approve        form field: ids[] (bulk approve)
GET  /admin/jobs                         the Phase 1 /jobs page, moved (decision 8)
```

Per applicant, the §8.2 signals, all derived in Go from
`users.lichess_profile` at render time (no extra columns to keep in
sync):

| Signal | Source |
|---|---|
| Account age | `createdAt` → "3 years"; highlighted under 30 days |
| Rated games | `count.rated` (absent → "—") |
| Closed / TOS-flagged | `disabled` / `tosViolation` — either one is a red banner |
| Ratings | `perfs.correspondence` / `perfs.classical` with a `prov` badge |
| Looks like an existing member | pure function `lookalikes(candidate, members)`: normalised names (lowercase, strip `_`, `-`, digits) equal, or Levenshtein ≤ 1, against every non-rejected member. Table-tested. |
| Applied | `created_at`; `fair_play_agreed_at` shown as "agreed" |

**Approve** (single or bulk, one transaction): `status=approved`,
`approved_at`, `approved_by`; `standings.Recompute(userID)` so the player
appears on `/standings` immediately rather than after the nightly
recompute; audit `registration.approve`. **Reject**: `status=rejected`,
`rejection_reason`; audit `registration.reject`. Both are plain form
POSTs that redirect back to the tab; htmx boosts them like every other
page, no custom JS. The reject reason is a `<details>`-revealed input
per row, no modal.

---

## Security mechanics (§11)

- **Tokens at rest** — `internal/tokencrypt`: AES-256-GCM, key from
  `TOKEN_ENCRYPTION_KEY` (64 hex chars), ciphertext stored as
  `nonce || sealed` in one `bytea`. A `tokencrypt.Secret` string type
  implements `fmt.Stringer` and `slog.LogValuer` returning `[redacted]`,
  so a token can't leak through a stray log line or `%v`. The decrypted
  value only ever exists inside `internal/lichess` calls.
- **Sessions** — `internal/session`: 32 random bytes, base64url, in an
  httpOnly, SameSite=Lax cookie, `Secure` whenever `LICHESS_REDIRECT_URI`
  is https (so `http://localhost` dev works without a flag). The table
  stores `sha256(token)`, not the token, so a database read doesn't yield
  live sessions. 30-day absolute lifetime; `last_seen_at` refreshed at
  most hourly (one write per hour, not per request); expired rows deleted
  opportunistically on each `Create` — one statement, no job. `Destroy`
  on logout; approve/reject/ban do not touch sessions (status is re-read
  from `users` on every request, so a banned user's next request is
  refused).
- **CSRF** — `http.NewCrossOriginProtection()` wraps the whole router:
  every non-safe request must be same-origin by `Sec-Fetch-Site` or
  `Origin`/`Host`. Combined with SameSite=Lax cookies this is the
  stdlib-only defence. Known limit, recorded here: a browser old enough
  to send neither header is allowed through; every browser since 2023
  sends `Sec-Fetch-Site`.
- **Rate limiting** — `internal/web/ratelimit.go`: per-IP token bucket
  (10 requests / minute, burst 10) on `/join`, `/login` and the callback,
  in-memory, pruned lazily. Returns 429 with a plain page. Uses
  `middleware.RealIP`, already in the chain.
- **Role checks** — middleware, server-side, on every request:
  `withUser` (loads the session's user into the context for the nav),
  `requireUser` (302 → `/login` when anonymous), `requireAdmin` (403 for
  a signed-in non-admin, 302 when anonymous). Admin routes live under one
  `r.Route("/admin", …)` group so a forgotten check is impossible to add
  by accident.
- **Callback** — `state` checked before anything else; the token
  response body and the `Authorization` header are never logged (the
  client's error path already keeps only `{error}` from JSON bodies).
  chi's `middleware.Logger` prints the request URI at entry, query
  included, so the callback route is mounted **outside** the logged
  group and logs its own line with the path only — the auth `code` never
  reaches the log.

---

## Schema — migration `00010_identity.sql`

```sql
CREATE TABLE oauth_tokens (
  user_id            uuid PRIMARY KEY REFERENCES users(id),
  access_token       bytea NOT NULL,         -- AES-256-GCM, nonce-prefixed (internal/tokencrypt)
  scopes             text[] NOT NULL,        -- as requested and granted: {'challenge:write'} in Phase 2; Phase 5 adds 'msg:write'
  issued_at          timestamptz NOT NULL DEFAULT now(),
  expires_at         timestamptz,            -- issued_at + expires_in; Lichess says ~1 year
  revoked_at         timestamptz,            -- Phase 3/5: set by the token-health probe
  last_validated_at  timestamptz NOT NULL DEFAULT now()
);
-- No refresh_token column: Lichess issues none (verified above).

CREATE TABLE sessions (
  token_hash    bytea PRIMARY KEY,           -- sha256 of the cookie value
  user_id       uuid NOT NULL REFERENCES users(id),
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_seen_at  timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL
);
CREATE INDEX sessions_user ON sessions (user_id);

ALTER TABLE users
  ADD COLUMN lichess_profile             jsonb,        -- raw GET /api/account body, same philosophy as games.raw_payload
  ADD COLUMN lichess_profile_fetched_at  timestamptz,
  ADD COLUMN fair_play_agreed_at         timestamptz;  -- NULL for seeded users; set at application
```

`users.status` transitions Phase 2 performs: `pending → approved`,
`pending → rejected`, `rejected → approved`. `banned` is only read (no UI
sets it yet; by hand in psql until §8.5 player management).

sqlc queries added (`internal/db/queries/`): `identity.sql` —
`CreatePendingUser`, `ApproveUser`, `RejectUser`, `PromoteToAdmin`,
`RenameUser`, `UpdateUserLichessProfile`, `ListRegistrations` (by status,
with profile row), `LookalikeCandidates` (id + username of every
non-rejected member — the matching is in Go), `UpsertOAuthToken`,
`GetOAuthToken`; `sessions.sql` — `CreateSession`, `GetSessionUser` (join
`users`, only unexpired), `TouchSession`, `DeleteSession`,
`DeleteExpiredSessions`. `CreateApprovedUser` stays for `seed-players`.

---

## Module layout (additions)

```
internal/config/config.go       + Auth section: LICHESS_CLIENT_ID, LICHESS_REDIRECT_URI,
                                  TOKEN_ENCRYPTION_KEY, SESSION_SECRET, ADMIN_LICHESS_USERNAMES.
                                  Read by Load; validated by `serve` only, so the one-shot
                                  CLI subcommands keep working with just DATABASE_URL.
internal/tokencrypt/            Key (from hex), Seal, Open, Secret (redacting string type). Pure; table tests.
internal/lichess/oauth.go       Auth interface { AuthCodeURL(state, verifier string, scopes []string) string;
                                  Exchange(ctx, code, verifier) (Token, error) } + OAuth (x/oauth2-backed)
                                  via NewOAuth(clientID, redirectURI, httpClient). Keeps all HTTP here.
internal/lichess/client.go      + Account(ctx, bearer) (Account, raw []byte, error)   GET /api/account
                                + RevokeToken(ctx, bearer) error                      DELETE /api/token
                                  do() gains a per-call bearer (the app-level token stays the default);
                                  API interface extended.
internal/lichess/types.go       + Account (embeds User; adds CreatedAt, Disabled, TOSViolation, Count.Rated, Verified)
internal/lichess/fake.go        + Fake implements Auth too: Codes map[code]token, Accounts map[token]Account,
                                  Revoked []token — so one Fake sits behind the whole callback in tests.
internal/lichess/snapshot.go    ratingSnapshotParams moves here from cmd/ic/lichess_mapping.go
                                  (both seed-players and the callback need it; gen is already imported by
                                  standings, so lichess → gen is acceptable — or put it in internal/standings
                                  if you'd rather keep lichess free of db types; implementer's call, say which).
internal/session/               Manager{q, secure bool}: Create, Load, Touch, Destroy; cookie helpers.
internal/web/auth.go            /join, /login, callback, /logout, /account; oauth-state cookie (HMAC).
internal/web/middleware.go      withUser, requireUser, requireAdmin; CrossOriginProtection wiring.
internal/web/ratelimit.go       per-IP token bucket middleware.
internal/web/admin.go           /admin/registrations + actions; signals; lookalikes().
internal/web/templates/         join.html, registered.html, auth_error.html, account.html,
                                admin_registrations.html; layout.html nav becomes user-aware.
internal/db/migrations/00010_identity.sql
internal/db/queries/identity.sql, sessions.sql
```

`web.Server` gains `auth lichess.Auth`, `client lichess.API`,
`sessions *session.Manager`, `crypt tokencrypt.Key`, `adminIDs
map[string]bool`, `secureCookies bool`. `base` gains the current user so
the layout can render the nav; handlers get it from a
`s.base(r, title, nav)` helper instead of constructing the struct by hand
(touches the five Phase 1 handlers, mechanically). `web.New` takes a
small `web.Deps` struct rather than five positional arguments.

Dependency direction stays `web → session/standings/… → db, lichess`;
`tokencrypt` imports only stdlib; `lichess` imports `x/oauth2`.

---

## Configuration

| Variable | Required by | Notes |
|---|---|---|
| `LICHESS_CLIENT_ID` | `serve` | Arbitrary; use the site's hostname. Nothing to register on Lichess. |
| `LICHESS_REDIRECT_URI` | `serve` | Must match byte-for-byte between `/oauth` and `/api/token`. `https` → cookies get `Secure`. Local dev: `http://localhost:8080/auth/lichess/callback`. |
| `TOKEN_ENCRYPTION_KEY` | `serve` | 64 hex chars (32 bytes). `openssl rand -hex 32`. Rotating it invalidates every stored token → everyone re-authorises; README says so. |
| `SESSION_SECRET` | `serve` | ≥ 32 bytes, any string. Signs only the 10-minute OAuth-state cookie (sessions themselves are random server-side ids). |
| `ADMIN_LICHESS_USERNAMES` | optional | Comma-separated, case-insensitive. |
| ~~`LICHESS_CLIENT_SECRET`~~ | — | Removed from §2.2: Lichess has no confidential clients. |

`.env` is git-ignored but nothing reads it today; the Makefile gets
`-include .env` + `export` so `make run` picks the five variables up.
README gets a dev block showing them.

---

## Build order

Each step is a self-contained commit point (the maintainer commits; see
`CLAUDE.md`). No step touches the live API in tests.

1. **Foundation** — `internal/config` Auth section; `internal/tokencrypt`
   with tests (round trip, tamper → error, wrong key → error,
   `LogValue`/`String` redaction); migration `00010`; `identity.sql`,
   `sessions.sql`; `make sqlc`. `make migrate` / `make migrate-down`
   verified on the dev database.
   Suggested: `feat(identity): schema, token encryption and config for Phase 2`.

2. **Lichess client** — `oauth.go` (`Auth` interface + `OAuth`),
   `Account`, `RevokeToken`, `Account` type, `Fake` support, snapshot
   helper move. Tests with `httptest.Server`: `AuthCodeURL` carries
   `code_challenge = BASE64URL(SHA256(verifier))`,
   `code_challenge_method=S256`, the scope string and state; `Exchange`
   posts exactly `grant_type, code, code_verifier, redirect_uri,
   client_id` and **no** `client_secret`; a 400 `{error,
   error_description}` surfaces as an error naming both; `Account`
   decodes `user_thibault_live.json` (`createdAt` → `time.Time`, absent
   `disabled`/`tosViolation` → false, `count.rated`) and sends the given
   bearer, not the app token; `RevokeToken` sends `DELETE` likewise.
   Adds `golang.org/x/oauth2` — commit body records: mandated by §2.1;
   used for `AuthCodeURL`/`Exchange` with PKCE; no refresh; one
   transitive requirement, not linked.
   Suggested: `feat(lichess): OAuth PKCE exchange, account lookup and token revocation`.

3. **Sessions, auth flow, `/account`** — `internal/session`; middleware,
   CSRF, rate limit; `/join`, `/login`, callback, `/logout`, `/account`;
   nav; templates; `make css`. Integration tests (existing tx-scoped
   `testServer` pattern, one `lichess.Fake` behind both the exchange and
   the account lookup):
   - new user via join → `pending` row with `fair_play_agreed_at`,
     profile row, snapshot row, `oauth_tokens` row whose bytes are not
     the plaintext and decrypt back to it, `scopes = {challenge:write}`,
     audit row, `ic_session` cookie set, 302 `/account`;
   - existing approved (seeded) user via login → no new row, token
     stored, still approved;
   - login by a non-member → no row, token revoked on the Fake, "not a
     member" page;
   - banned user → no session, token revoked;
   - Lichess rename → `lichess_username` updated, audit row;
   - bootstrap admin: existing → promoted; absent via join → created
     approved+admin; absent via login → "not a member";
   - `state` mismatch / missing cookie / expired cookie → 400, zero
     writes; `error=access_denied` → friendly page, zero writes;
   - `/account` anonymous → 302 `/login`; signed in → renders status and
     never contains the token string;
   - `POST /logout` → session row gone, cookie cleared;
   - cross-site `POST` (`Sec-Fetch-Site: cross-site`) → 403;
   - session: expired row not loaded; `Touch` writes at most once an hour.
   Unit tests: rate limiter (allows burst, then 429, refills), OAuth-state
   cookie sign/verify/expiry.
   Suggested: `feat(web): Lichess sign-in, registration and sessions`.

4. **Admin registration queue** — `/admin/*` routes, signals,
   `lookalikes()` table test (`MilsBees` vs `milsbees_` vs `M1lsBees`;
   unrelated names don't match), approve / reject / bulk approve, `/jobs`
   → `/admin/jobs` (decision 8). Integration tests: approve sets the
   three columns, writes audit and a `player_standings` row (the player
   now appears in `GetStandings`); reject requires a reason; bulk approve
   is atomic (one bad id → nothing changes); non-admin → 403; anonymous →
   302; rejected tab lists and can approve.
   Suggested: `feat(admin): registration queue with Lichess account signals`.

5. **Docs and close-out** — README (env vars, `.env`, the dev sign-in
   walkthrough, note that `seed-players` is now only for bootstrapping a
   roster without tokens), Makefile `.env` include, `CLAUDE.md`
   repository-state paragraph, spec amendments below, and this file's
   verification section filled in with what actually happened.
   Suggested: `docs: close out Phase 2`.

Implementation happens in this session, one step at a time, stopping
after each for review and commit (no commits by the assistant, no
dependencies beyond `x/oauth2`).

---

## Spec amendments (applied to `docs/infinite-correspondence-spec.md` on "do it", with a §15 changelog entry)

- **§2.2** — remove `LICHESS_CLIENT_SECRET`; note `LICHESS_CLIENT_ID` is
  self-chosen (public PKCE client, nothing registered on Lichess).
- **§3.1** — add the verified facts: PKCE `S256` only, no client
  authentication, no refresh tokens, ~1-year expiry, `POST
  /api/token/test` as the health probe, `DELETE /api/token` for
  revocation, all-or-nothing consent (hence the pre-redirect `msg:write`
  checkbox).
- **§4.1** — `OAuthToken`: drop `refresh_token`, add `issued_at`. `User`:
  add `lichess_profile`, `lichess_profile_fetched_at`,
  `fair_play_agreed_at`. New `Session` entity (`token_hash`, `user_id`,
  `created_at`, `last_seen_at`, `expires_at`).
- **§8.2** — reorder the flow (agreement on the join page, before the
  redirect); drop the timezone field; `msg:write` is requested by
  re-authorisation in Phase 5, not at registration; add: existing
  members sign in through the same flow; the bootstrap-admin rule;
  rejected applicants can't re-apply themselves; the approval
  notification is Phase 5 (status visible on `/account` meanwhile).
- **§11** — name the concrete mechanisms (AES-256-GCM, hashed session
  ids, `Sec-Fetch-Site` CSRF, per-IP limiter) so they aren't re-decided.
- **§12** — Phase 1 paragraph: `/jobs` is admin-only from Phase 2;
  Phase 2 paragraph expanded to what this plan builds and defers.
- **§13** — decisions 1–8 above. **§14.4** — resolved.

---

## Verification (end of Phase 2)

Live, against lichess.org, by hand — not in tests:

1. `make migrate` from the Phase 1 schema applies `00010`; `make
   migrate-down` reverses it.
2. `make test` and `make test-integration` green.
3. `make run` with the five variables set and
   `LICHESS_REDIRECT_URI=http://localhost:8080/auth/lichess/callback`.
   `/join` with a real Lichess account: consent screen names only
   "Create, accept, decline challenges"; lands on `/account` as pending; `users`, `oauth_tokens`
   (ciphertext, not `lio_…`), `rating_snapshots`, `audit_log` rows
   present; nothing token-like and no `code=` in the server log.
4. Cancel on the Lichess consent screen → the friendly page, zero rows.
5. Join with a username listed in `ADMIN_LICHESS_USERNAMES` → approved
   at once, "Admin" in the nav; `/admin/registrations` shows the other
   applicant with age, rated-game count and ratings matching their
   Lichess profile page.
6. Approve → applicant appears on `/standings` immediately; their
   `/account` reads approved. Reject another with a reason → their
   `/account` shows the reason; rejected tab lists them.
7. A Phase 1 seeded member signs in → no new row, `oauth_tokens` row
   created, still approved, no queue entry.
8. `POST /admin/registrations/{id}/approve` from `curl` without a
   session → 302; with a player session → 403; with an admin session but
   `Sec-Fetch-Site: cross-site` → 403.
9. Restart the server → sessions survive (they're in Postgres); `ic
   sync-games` and the other one-shot subcommands still run with only
   `DATABASE_URL` set.
