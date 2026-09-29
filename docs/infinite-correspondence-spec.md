# Infinite Correspondence — Technical Specification

Replacement web application for the "Lichess4545 - Infinite Correspondence" Google Sheets system.

**Status:** specification for implementation — Phase 1 amendments applied 2026-09-07, Phase 2 and Phase 3 amendments applied 2026-09-15, Phase 4 plan amendments applied 2026-09-17, Phase 5 plan amendments applied 2026-09-28 (see §15 changelog)
**Source of truth for existing behaviour:** the `Lichess4545 - Infinite Correspondence` spreadsheet (sheets: `Overview`, `Standings`, `Standings_Backend`, `Levels`, `Pairing_Maker`, `Pairing_Maker_Backend`, `Perf_Rating_Backend`, `RawData`, `Stats`)

> **Out of scope:** the spreadsheet's `Awards` / `Awards_Backend` sheets are **not** being ported. The award metrics depend on a "compensation / sound sacrifice" detection rule whose implementation was lost, and the maintainers have confirmed the feature will not be carried over.

---

## 1. Background and goals

The league is an open-ended ("infinite") correspondence chess ladder run on Lichess. There are no seasons and no fixed roster: every Monday, all currently-active players are paired against each other, and this repeats indefinitely. Results feed a standings table, a rolling performance rating, an XP/levelling layer, and a set of statistics.

The existing implementation is a Google Sheet driven by Apps Script custom functions (`PLAYER_URL`, `LAST_K_CUT_OFF`, `SELECT_OPPONENTS_SINCE`, `PLAYER_RATINGS`, `CLASSICAL_RATING`) plus an external Python script that writes finished games into the `PythonUpdate` / `RawData` sheets.

### Problems being solved

1. **Reliability.** The Google API / Apps Script layer failed intermittently and silently. Custom functions returning `#N/A` or stale values are hard to detect and harder to debug.
2. **Manual admin overhead.** Player activity, pause status and pairing publication all require hand-editing cells.
3. **No self-service.** Players cannot mark themselves inactive or express how many games they want; this must be relayed to an admin.
4. **No game automation.** Players are told to challenge each other manually, and a meaningful share of pairings never become games.

### Goals

- Fully automated weekly pairing generation and game creation, with human intervention only as an exception.
- Player self-service: activity status and concurrent-game capacity.
- Admin-verified registration.
- Faithful reproduction of the standings, performance rating, levelling and stats logic.
- Observable, debuggable background jobs.

### Non-goals

- Replacing Lichess as the place games are played. All chess happens on Lichess; this site orchestrates and reports.
- Supporting leagues other than this one (single-tenant is fine).

---

## 2. Architecture overview

```
                    ┌──────────────────────────┐
                    │      Lichess API         │
                    │  OAuth · bulk-pairing ·  │
                    │  challenge · game export │
                    └────────────┬─────────────┘
                                 │
      ┌──────────────────────────┼──────────────────────────┐
      │                          │                          │
┌─────▼──────┐          ┌────────▼────────┐        ┌────────▼────────┐
│ Auth /     │          │ Pairing engine  │        │  Sync workers   │
│ OAuth flow │          │ (scheduled)     │        │  (scheduled)    │
└─────┬──────┘          └────────┬────────┘        └────────┬────────┘
      │                          │                          │
      └──────────────────────────┼──────────────────────────┘
                                 │
                    ┌────────────▼─────────────┐
                    │       PostgreSQL         │
                    │   (single source of      │
                    │        truth)            │
                    └────────────┬─────────────┘
                                 │
                    ┌────────────▼─────────────┐
                    │  Web app (SSR)           │
                    │  public + player + admin │
                    └──────────────────────────┘
```

### 2.1 Recommended stack

**The backend is Go.** This is a maintainer preference and is not negotiable in implementation — the person who will run this system long-term is most fluent in Go, which matters more for a volunteer-maintained league than any framework's merits.

| Layer | Choice | Rationale |
|---|---|---|
| Language | Go 1.22+ | Maintainer fluency; single static binary, trivial deployment |
| HTTP routing | `net/http` + `chi` | Stdlib-centric, no framework lock-in; `chi` only for routing and middleware |
| Templating (SSR) | `templ` or stdlib `html/template` | Public pages are server-rendered and cacheable |
| Interactivity | htmx | The UI is tables, toggles and forms. This avoids a separate SPA build and keeps everything in Go |
| Styling | Tailwind (CLI build) | Utility CSS without needing a JS runtime at serve time |
| Database | PostgreSQL | Relational data, transactional pairing generation |
| DB access | `sqlc` | Generates typed Go from hand-written SQL. Preferred over an ORM: the standings and pairing queries are the interesting part and should stay explicit SQL |
| Migrations | `golang-migrate` or `goose` | Versioned, reversible |
| Background jobs | `river` (Postgres-backed) | Durable queue with retries and job history, no extra infrastructure — the observability requirement in §7 is the whole point |
| Scheduling | `river`'s periodic jobs, or `robfig/cron` | Weekly generation and the polling jobs |
| OAuth | `golang.org/x/oauth2` | Standard, with a custom Lichess endpoint config |
| Config | env vars via `caarlos0/env` or stdlib | See §2.2 |
| Testing | stdlib `testing` + `testify` | Table-driven tests suit the scoring and pairing logic well |

Notes on the choices that carry real weight:

- **`sqlc` over GORM.** The scoring module (§5) and pairing pool queries benefit from being readable SQL that can be diffed against the spreadsheet's logic. An ORM would obscure exactly the part that needs auditing.
- **`river` over cron-only.** §7 requires persisted job outcomes, retries, and an admin health view. A bare cron loop cannot provide that, and the absence of it is the original failure this rebuild exists to fix.
- **htmx over a JS SPA.** Keeps the whole system in one language and one deployable. The interactivity needed (toggles, sortable tables, admin edits) is well within htmx's range.
- **No Redis required.** `river` uses Postgres, so the production footprint is one binary plus one database.

### 2.2 Environment configuration

```
DATABASE_URL
LICHESS_CLIENT_ID              # self-chosen public client id (e.g. the site's hostname); nothing is registered on Lichess
LICHESS_REDIRECT_URI           # absolute callback URL; its scheme also decides whether cookies are Secure
LICHESS_ORG_TOKEN              # organiser token, scope: challenge:bulk; needed only when
                               # pairing.game_creation = lichess (§4.2)
TOKEN_ENCRYPTION_KEY           # 32 bytes, hex-encoded: AES-256-GCM key for stored player OAuth tokens
SESSION_SECRET                 # >= 32 chars: signs the short-lived OAuth-state cookie
ADMIN_LICHESS_USERNAMES        # comma-separated bootstrap admin list
```

There is no `LICHESS_CLIENT_SECRET`: Lichess has no confidential clients (§3.1). Rotating `TOKEN_ENCRYPTION_KEY` makes every stored token unreadable, so every player re-authorises. *(Amended 2026-09-15.)*

`LICHESS_ORG_TOKEN` may be empty while `pairing.game_creation = manual`; a `create-games` run under `lichess` without it fails, naming the variable. There is no `LICHESS_MSG_ENABLED`: the league sends no custom Lichess messages (§10). *(Amended 2026-09-28.)*

---

## 3. Lichess integration

### 3.1 OAuth scopes

| Actor | Scope | Purpose |
|---|---|---|
| Organiser (league account) | `challenge:bulk` | Create bulk pairings (Lichess also sends each player the bulk's message from this account, §3.2) |
| Each player | `challenge:write` | Allow games to be created on their behalf, and a challenge to be sent from their account when their opponent's token has lapsed (§3.3) |

Bulk pairing requires the organiser's `challenge:bulk` token **plus a `challenge:write` token for every player in the pairing**.

**`msg:write` is not a player scope** *(amended 2026-09-28)*. An earlier draft asked each player for it so the league could send them Lichess private messages. But `POST /inbox/{username}` sends a message *as the token's owner*: a player's `msg:write` token could only send messages *from that player*. League messages would have to come from the organiser account, which Lichess allows to start only about 20 new conversations a day. Players are never asked for it; the only Lichess message the league sends is the one bulk pairing sends by itself (§3.2, §10). Custom messages, if still wanted, come from the organiser account in Phase 6 at the earliest.

**How Lichess OAuth actually works** (verified against the `lichess-org/api` OpenAPI sources, v2.0.171, 2026-09-15):

- Authorization Code flow **with PKCE, `S256` only**. Lichess "supports unregistered and public clients (no client authentication, choose any unique client id)" — there is no client secret and nothing to register. `client_id` is an arbitrary string we choose.
- `GET /oauth` takes `response_type=code`, `client_id`, `redirect_uri`, `code_challenge_method=S256`, `code_challenge`, plus optional `scope` (space-separated) and `state`; the redirect back carries `code` and `state`, or `error` (`access_denied` when the user cancels), `error_description` and `state`.
- `POST /api/token` (form-encoded `grant_type=authorization_code`, `code`, `code_verifier`, `redirect_uri`, `client_id`) returns `{token_type, access_token, expires_in}`.
- **Access tokens are long-lived (about a year) and there are no refresh tokens.** The remedy for an expired or revoked token is signing in again; nothing refreshes anything.
- `GET /api/account` with the new token identifies the player (`id`, `username`, `perfs`, `createdAt`, `disabled`, `tosViolation`, `count.rated`, …) — the registration queue's signals (§8.2) come from this one call, stored raw on the `User`.
- `POST /api/token/test` (unauthenticated, up to 1000 tokens per call) reports each token's `userId`, `scopes` and `expires`, or `null` if invalid: the token-health probe for §3.3, run weekly by `validate-tokens` (§7) and again over a round's players when its games are created (§6.3). `DELETE /api/token` revokes the bearer token — used for a token obtained but not kept, and on account deletion (§11).
- **Consent is all-or-nothing** for the requested scope string: a player cannot decline one scope on the Lichess screen, so any optional scope would have to be a choice made on *our* side before the redirect. The league requests exactly `challenge:write`, at registration and at every sign-in.

**Players do nothing technical.** The token is obtained transparently during registration through a standard OAuth consent screen — the same experience as any "Sign in with Google" button:

1. The player clicks "Join the league".
2. They are redirected to Lichess, where they are typically already signed in.
3. Lichess shows a consent screen naming the permissions requested (create, accept and decline challenges).
4. They click Authorise and land back on the site, registered.

The player never sees, copies, or manages a token. It is issued to the server, encrypted at rest, and used only to create their league games. There is no manual step, no API key to paste, and nothing to configure on Lichess.

**How a token nonetheless becomes invalid.** A token that was valid at registration can stop working later. This is uncommon per player but near-certain across a large roster over years, which is why §3.3 exists:

- The player revokes the application in their Lichess security settings, often without realising it stops their pairings.
- The token reaches its expiry, most likely for a player who has been dormant a long time.
- The Lichess account is closed, renamed, or marked for a terms-of-service violation.
- The league's OAuth client credentials are rotated, invalidating previously issued tokens.

In every case the remedy is identical and takes one click: the player re-authorises through the same flow. §3.3 is a recovery path, not a routine one.

### 3.2 Game creation — primary path

`POST /api/bulk-pairing`

Relevant form fields:

| Field | Value for this league |
|---|---|
| `players` | `token1:token2,token3:token4,...` — colon-joined pairs, white first; for correspondence a player may appear in more than one pair (the double game, §6.2 step 6a) |
| `days` | the round's `days_per_move` (default `2`), from its `settings_used` |
| `rated` | the round's `rated` (default `true`), from its `settings_used` |
| `variant` | `standard` |
| `pairAt` | **omitted**: the games are created at once (§6.3) |
| `message` | a message naming the league and the round; must contain `{game}` |

Notes and constraints (OpenAPI v2.0.174, re-checked 2026-09-24; points marked *lila* are read from the server source, not documented, and are confirmed live before game creation is switched on):

- Games are created **directly**. Players do not accept a challenge; the game simply appears in their correspondence list. This removes the "failure to start a game" failure mode for consenting players.
- The response carries the bulk's id and **the game ids at creation** (`games[{id, white, black}]`), so the games are known at once, with no discovery.
- `clock.limit` / `clock.increment` are omitted when `days` is used.
- The `message` is **sent to each player from the organiser account** when their game is created (default *"Your game with {opponent} is ready: {game}."*; *lila:* it ignores the players' messaging preferences). It is the league's "game created" Lichess message (§10).
- A bulk is all-or-nothing and a failed bulk is free (§6.3). *lila:* a bad token comes back as `{"tokens": {"<the token>": "<reason>"}}`, keyed by the raw token, which the client maps back to a player; any other refusal is `{"error": "..."}`. The body therefore contains secrets and is never logged or stored.
- `GET /api/bulk-pairing` lists the organiser's bulks: a retried game-creation job uses it to adopt a bulk it already created rather than create the games twice (§6.3).
- Bulks can also be scheduled ahead with `pairAt` and cancelled before they fire. The league uses neither: nothing reaches Lichess before a round is published, and the review window before that is the undo (§6.3).
- The organiser token must remain valid; treat its expiry as a P1 alert (§8.5).

**Implementation requirement:** verify exact parameter names, rate limits and the maximum number of pairs per request against the live API documentation at `https://lichess.org/api#tag/Bulk-pairings` before writing the client. Wrap all of this in a single `LichessClient` module so the rest of the app never touches HTTP directly.

### 3.3 Game creation — fallback path

If a player has no valid `challenge:write` token (never granted, revoked, or expired), they cannot be included in a bulk pairing. Fallback order:

This path should be rare. It exists because a token can lapse for the reasons listed in §3.1, not because players are expected to do anything themselves.

1. **Take the pairing out of the bulk request and create it another way.** A token *works* when it is stored, not revoked, not expired, carries `challenge:write`, and passed the probe run when the round's games are created (§6.3). Per pairing *(Phase 5, 2026-09-28)*:
   - **both tokens work** → bulk pairing (§3.2);
   - **one works** → a **direct challenge sent by the player whose token works**: `POST /api/challenge/{opponent}` with their token, the round's `days` and `rated`, and `color` set to the challenger's assigned colour. The opponent must accept — the old, manual behaviour. The challenge's id is the game's id once accepted (§3.4);
   - **neither works** → the pairing stays `manual_external`, and the two players start the game by hand, as before Phase 5 (§7.3).
2. **Prompt the player to re-authorise.** A prominent banner on their dashboard and a notification, both linking to the one-click re-auth flow — the ordinary sign-in, which replaces the stored token and clears its revocation (§8.2). The message should explain the consequence in plain terms: their games can no longer be created automatically.
3. **Flag in the admin dashboard.** Players without a valid token are listed explicitly (`/admin/tokens`, §8.5), with since when and the days of grace left, since each one degrades the experience of whoever they are paired against.

A player whose token has been invalid for more than `token.invalid_grace_days` (default 14) is excluded from pairing until they re-authorise. This is not a penalty — it prevents them from repeatedly consuming a pairing slot that cannot become a real game.

*(Phase 5, 2026-09-28.)* The exclusion is computed live when the pool is built — a `no_valid_token` `RoundExclusion` (§5.7) — not a flag flipped on the player. Setting `is_active = false` would overwrite the player's own choice and need undoing on re-authorisation; computed live, re-authorising restores them for the next round with nothing to undo, and the dashboard says why they were left out and how to fix it. "Invalid since" is `revoked_at`, or `expires_at` once passed. **A player with no token row at all** (seeded, never signed in) is excluded at once, with no grace: grace is for a token that lapsed, and this also means the site can never send a surprise challenge to a seeded member from a test database. The rule applies only when the site creates games (`pairing.game_creation = lichess`, §4.2).

### 3.4 Reading games

Two distinct sync jobs (§7):

- **Ongoing detection.** For each published pairing, determine whether the Lichess game exists and is in progress. When games are created via bulk pairing, the game IDs are returned by the API at creation time (`POST /api/bulk-pairing` answers with the bulk and its `games` array of `{id, white, black}`; `GET /api/bulk-pairing/{id}` returns the same), so ongoing games are known immediately without polling for discovery. **A challenge's id is its game's id once accepted** (documented), so a fallback challenge is stored with that id and re-checked by id exactly like a bulk game *(2026-09-28)*. Discovery by search is only needed for games players start by hand (§7.3); polling otherwise only detects completion.
- **Completion sync.** Export finished games and ingest full detail. For any pairing with a known game ID, use `POST /api/games/export/_ids` (up to 300 comma-separated IDs per call, `Accept: application/x-ndjson`) — one call re-checks every in-progress game. `GET /api/bulk-pairing/{id}/games` streams the games of a bulk pairing. For a pairing **without** a game ID (a game started by hand — §7.3), search `GET /api/games/user/{username}` for the white player with `perfType=correspondence&rated=true&since=<round pair_at>` and match on opponent, colour, variant and `daysPerTurn`. Single-game export, when needed, is `GET /game/export/{gameId}` (note: **not** under `/api/`).

Export options: `opening=true&accuracy=true`. Do **not** request `evals=true` — it appends a per-ply analysis array that no metric uses; `acpl` and `accuracy` come from `players.{white,black}.analysis` without it. Do **not** request `clocks=true` either: it appends a per-move array of remaining time that is meaningless for correspondence games (the clock is days per move, not a running clock) and nothing in this spec reads it. Both omissions are deliberate — they keep `raw_payload` (§7.1) small without losing anything a later phase could want. *(Decided 2026-09-13.)*

**Only games that match a `Pairing` are ingested.** Players play correspondence games outside the league, including against other members; a game is a league game if and only if it belongs to a pairing. Polling members' game lists without a pairing to match against is not a valid discovery mechanism.

All ingestion must be **idempotent**, keyed on the Lichess game ID.

---

## 4. Domain model

### 4.1 Entities

```
User
  id                    uuid pk
  lichess_username      text unique (canonical casing preserved, matched case-insensitively)
  lichess_user_id       text unique
  role                  enum(player, admin)
  status                enum(pending, approved, rejected, banned)
  created_at            timestamptz
  approved_at           timestamptz nullable
  approved_by           uuid nullable fk -> User    # NULL = the bootstrap-admin rule
  rejection_reason      text nullable
  lichess_profile       jsonb nullable   # raw GET /api/account body from the last sign-in (§8.2 signals)
  lichess_profile_fetched_at timestamptz nullable
  fair_play_agreed_at   timestamptz nullable   # set at application; NULL for seeded users

OAuthToken                                 # one per user, replaced on every sign-in
  user_id               uuid pk fk -> User
  access_token          bytea            # AES-256-GCM, nonce-prefixed; key from TOKEN_ENCRYPTION_KEY
  scopes                text[]           # as granted: {challenge:write}
  issued_at             timestamptz
  expires_at            timestamptz nullable   # issued_at + expires_in
  revoked_at            timestamptz nullable
  last_validated_at     timestamptz
  # No refresh_token: Lichess issues none (§3.1). Amended 2026-09-15.

Session
  token_hash            bytea pk         # sha256 of the cookie value
  user_id               uuid fk -> User
  created_at            timestamptz
  last_seen_at          timestamptz      # refreshed at most hourly
  expires_at            timestamptz      # 30 days after creation, absolute

PlayerProfile
  user_id               uuid pk fk -> User
  is_active             boolean default true      # player-controlled
  max_concurrent_games  int NULL                  # player-controlled.
                                                  # NULL = UNLIMITED (the default):
                                                  # always paired every round.
                                                  # When set, caps TOTAL ongoing
                                                  # games, not pairings per
                                                  # round (§5.8)
  accepts_double_game   boolean default true      # player-controlled; opt out of
                                                  # absorbing an odd pool (§6.2.6a)
  paused_by_admin       boolean default false
  paused_reason         text nullable
  auto_paused_at        timestamptz nullable      # missed-start rule
  resumed_at            timestamptz nullable      # last "resume quest"; missed starts
                                                  # before it no longer count (§6.4)
  timezone              text
  joined_at             timestamptz

RatingSnapshot
  id                    bigserial pk
  user_id               uuid fk -> User
  correspondence_rating int nullable
  classical_rating      int nullable
  fetched_at            timestamptz

Round
  id                    serial pk
  number                int                # unique among non-cancelled rounds: a cancelled
                                           # draft gives its number back (Phase 4)
  state                 enum(draft, published, cancelled)   # at most one draft at a time
  generated_at          timestamptz
  publish_at            timestamptz        # end of review window
  published_at          timestamptz nullable
  pair_at               timestamptz        # set to the actual publish moment on publish; the
                                           # §7.3 search cutoff. Not sent to Lichess: games
                                           # are created at once, without pairAt (§6.3)
  generated_by          enum(schedule, manual, imported)
  bulk_pairing_id       text nullable      # Lichess bulk pairing id
  game_creation         enum(manual, creating, created, gave_up) default manual
                                           # how the games were created (§6.3): manual =
                                           # published under pairing.game_creation = manual
                                           # (and every round before Phase 5)
  games_created_at      timestamptz nullable   # when create-games finished
  notes                 text nullable      # cancellation reason, admin remarks
  pool_size             int nullable       # diagnostics (§8.5): players in the matching pool
  odd_pool              enum(even, double_game, bye) nullable   # how an odd pool was resolved
  repeat_pairings       int nullable       # repeats the solver had to accept (§6.2 step 7)
  settings_used         jsonb nullable     # the pairing.* settings as they were at generation

Pairing
  id                    uuid pk
  round_id              int fk -> Round
  white_user_id         uuid fk -> User
  black_user_id         uuid fk -> User
  creation_method       enum(bulk, challenge, manual_external)
                                                    # manual_external: pairing published outside
                                                    # the system (the spreadsheet during the
                                                    # transition, or an admin fix-up); the game
                                                    # is discovered by matching (§7.3). Generated
                                                    # pairings start as manual_external; game
                                                    # creation (§6.3) overwrites the method for
                                                    # those it creates, and the rest stay
                                                    # hand-started.
  lichess_game_id       text nullable unique        # for a challenge, the challenge id: it is the
                                                    # game's id once accepted (§3.4)
  status                enum(pending, created, in_progress, completed, failed, cancelled)
  match_ambiguous       boolean default false       # §7.3: more than one candidate game found;
                                                    # admin must pick
  created_at            timestamptz
  edited_by             uuid nullable fk -> User    # set if an admin altered it
  position              int nullable                # order in the generated list
  rating_gap            int nullable                # |power(white) − power(black)| at generation;
  color_penalty         int nullable                # colour_penalty of the colours assigned;
  repeat_of_round       int nullable                # last round these two met within the window.
                                                    # All three are NULL on an admin-edited row.

Game                                       # one row per league game, in progress or finished
  lichess_game_id       text pk
  pairing_id            uuid fk -> Pairing          # every game belongs to a pairing (§3.4)
  round_number          int                          # denormalised from Round
  white_user_id         uuid fk -> User
  black_user_id         uuid fk -> User
  status                enum(in_progress, finished)
  lichess_status        text                         # raw API status, kept verbatim
  result                enum(white_win, black_win, draw) nullable   # null while in_progress
  termination           enum(mate, resign, clock_flag, draw_agreement, threefold,
                             fifty_move, stalemate, insufficient_material,
                             draw_other, unknown) nullable          # null while in_progress
  days_per_turn         int nullable
  eco                   text nullable
  opening_name          text nullable
  opening_ply           int nullable
  white_first_move      text nullable
  black_first_move      text nullable
  white_rating_at_game  int nullable       # players.white.rating in the API response (§5.2)
  black_rating_at_game  int nullable
  white_accuracy        numeric nullable   # only present if the game was analysed on Lichess
  black_accuracy        numeric nullable
  white_moves           int nullable
  black_moves           int nullable
  white_acpl            int nullable       # AVERAGE centipawn loss — the API has no "total"
  black_acpl            int nullable
  started_at            timestamptz        # createdAt
  last_move_at          timestamptz
  finished_at           timestamptz nullable
  duration_seconds      bigint nullable
  raw_payload           jsonb             # full Lichess response, for reprocessing
  ingested_at           timestamptz
  updated_at            timestamptz

  Aborted games (Lichess status aborted / noStart) are NOT stored here; they are
  recorded on the Pairing as failed. Termination mapping from the Lichess status
  enum: mate->mate, resign->resign, outoftime->clock_flag, timeout->clock_flag,
  stalemate->stalemate, insufficientMaterialClaim->insufficient_material,
  cheat/unknownFinish/variantEnd->unknown (result still taken from `winner`).
  The API reports every other draw as a bare `draw`; the split into
  draw_agreement / threefold / fifty_move / draw_other is INFERRED by replaying
  the moves (§7.1) and must be labelled as inferred wherever it is displayed.

PlayerStanding                             # materialised, recomputed on ingest
  user_id               uuid pk fk -> User
  rating                int
  games_played          int
  wins                  int
  draws                 int
  losses                int
  ongoing               int
  last_k_score          numeric nullable
  last_k_perf_rating    numeric nullable
  power_rating          numeric
  color_score           int
  xp                    int
  level                 int
  xp_to_next_level      int
  last_level_up_round   int nullable
  last_level_up_at      timestamptz nullable
  last_game_finished_at timestamptz nullable
  is_eligible           boolean
  updated_at            timestamptz

MissedStart
  id                    bigserial pk
  user_id               uuid fk -> User       # the player who had to accept the challenge
  round_id              int fk -> Round
  pairing_id            uuid fk -> Pairing    # the challenge pairing that was never accepted
  recorded_at           timestamptz
  UNIQUE (round_id, user_id)

RoundExclusion            # why an eligible-looking player got no pairing
  id                    bigserial pk
  round_id              int fk -> Round
  user_id               uuid fk -> User
  reason                enum(at_capacity, inactive, paused, auto_paused,
                             no_valid_token, bye, pending_approval,
                             removed_by_admin)      # removed_by_admin: an admin deleted the
                                                    # pairing from the draft (§8.5)
  ongoing_games         int nullable      # populated when reason = at_capacity
  max_concurrent_games  int nullable      # populated when reason = at_capacity
  created_at            timestamptz
  UNIQUE (round_id, user_id)

Notification              # on-site notification centre (§10)
  id                    bigserial pk
  user_id               uuid fk -> User
  category              text              # maps to the event table in §10
  title                 text
  body                  text
  link_url              text nullable
  dedupe_key            text nullable          # e.g. round:201:paired:<pairing id>, so a
                                              # retried job never notifies twice
  UNIQUE (user_id, dedupe_key)                # per recipient: both players of a pairing
                                              # are told about one event under one key
  read_at               timestamptz nullable
  created_at            timestamptz

NotificationPreference    # a row only for a category the player turned off
  user_id               uuid fk -> User
  category              text
  PRIMARY KEY (user_id, category)
  # On-site is the only channel (§10), so the earlier on_site / lichess_pm
  # flags are gone: the row's existence is the opt-out. Amended 2026-09-28.

Bye                       # history, drives bye rotation fairness (§6.2.6b)
  id                    bigserial pk
  round_id              int fk -> Round
  user_id               uuid fk -> User
  created_at            timestamptz
  UNIQUE (round_id, user_id)

DoubleGame                # history, drives double-game rotation (§6.2.6a)
  id                    bigserial pk
  round_id              int fk -> Round
  user_id               uuid fk -> User
  created_at            timestamptz
  UNIQUE (round_id, user_id)

AuditLog
  id                    bigserial pk
  actor_user_id         uuid nullable fk -> User    # null = system
  action                text
  entity_type           text
  entity_id             text
  before                jsonb nullable
  after                 jsonb nullable
  created_at            timestamptz

Setting                                     # runtime-editable configuration
  key                   text pk
  value                 jsonb
  updated_at            timestamptz
  updated_by            uuid nullable fk -> User
```

### 4.1.1 Notes on the less obvious entities

**`MissedStart`** — records that a player was issued a *challenge* (fallback path, §3.3) and never accepted it, so the pairing never became a real game. It is the input to the legacy rule *"fail to start two weeks running → paused"* (§6.4).

With bulk pairing this is close to vestigial: games are created outright, so there is nothing for the player to accept and nothing to miss. It still earns its place because a player whose token has lapsed would otherwise consume a pairing slot every week, producing a challenge nobody accepts and quietly wasting an opponent's round. Rows should be rare; if this table fills up, something is wrong with token health rather than with players.

A row is written **only for a challenge pairing, for the player who had to accept it** — the one case where the league knows who did not act. A hand-started pairing that never became a game is retired without one: either player could have issued the challenge, and Lichess shows no challenge that was sent and ignored (§6.4). *(Phase 5, 2026-09-28.)*

**`RoundExclusion`** — one row per player who was considered for a round but received no pairing, with the reason (at capacity, inactive, paused, auto-paused, no valid token, bye, pending approval).

It exists to answer the single most common support question this system will generate: *"why didn't I get a game this week?"* Without it, that question can only be answered by re-running the pairing engine and inferring what happened — precisely the debugging experience the spreadsheet forced on maintainers. With it, the answer renders directly on the player's dashboard and in the admin round diagnostics view, and no one has to be asked.

Both tables are append-only history. Neither is read by the pairing algorithm itself; they exist for explanation and audit.

Rows are written for approved members only, and only for published rounds' history to be read back: a draft's rows are deleted when it is regenerated and ignored when it is cancelled. Pending applicants are never loaded into the pool, so `pending_approval` is unused until a phase needs it; `no_valid_token` arrives with `validate-tokens` (Phase 5). *(Phase 4, 2026-09-17.)* It is written only under `pairing.game_creation = lichess`, for a token lapsed beyond its grace or no token at all (§3.3). *(Phase 5, 2026-09-28.)*

### 4.2 Configurable settings

All of these live in `Setting`, are editable from the admin panel, and have the defaults below (taken from the current spreadsheet's behaviour). Until the admin settings UI exists (Phase 6) they are set with `ic setting <key> <json>` or directly in the table; the loader validates ranges and enum values (`review_window_hours`, `avoid_recent_rounds`, `color_weight` ≥ 0; `repeat_penalty` > 0; `mode`, `odd_pool_strategy` and `solver` from their lists) and fails naming the key, so a bad value stops the next job run visibly rather than defaulting silently. `pairing.cron` is read when the server starts; changing it takes effect on restart. *(Phase 4, 2026-09-17.)* The admin's round actions that run the engine (generate now, regenerate, swap) read the table on every click, as the scheduled job does on every run, so any other pairing setting takes effect from the next action without a restart. *(2026-09-22.)* Phase 5 adds `pairing.game_creation` (from its list), `days_per_move` from Lichess's set, `token.invalid_grace_days` ≥ 0 and `activity.missed_starts_to_pause` ≥ 1 to the validation; a `days_per_move` outside Lichess's set would make every bulk fail. *(2026-09-28.)*

| Key | Default | Meaning |
|---|---|---|
| `pairing.cron` | `0 12 * * 1` | When generation runs (Monday) |
| `pairing.mode` | `review_window` | `review_window` \| `auto_publish` |
| `pairing.review_window_hours` | `6` | Auto-publish delay if no admin action |
| `pairing.last_k` | `5` | Rolling window size for performance rating |
| `pairing.avoid_recent_rounds` | `5` | Do not repeat opponents from the last N rounds |
| `pairing.color_weight` | `100` | Rating points one unit of colour imbalance is worth (§6.2) |
| `pairing.repeat_penalty` | `1000000` | Large finite cost of a repeat opponent (§6.2) |
| `pairing.days_per_move` | `2` | Correspondence time control: one of 1, 2, 3, 5, 7, 10, 14 (the values Lichess accepts) |
| `pairing.rated` | `true` | Rated games |
| `pairing.game_creation` | `manual` | `manual` \| `lichess`: whether publication creates the round's games on Lichess (§6.3). `manual` = players start every game by hand, as before Phase 5; `lichess` needs `LICHESS_ORG_TOKEN` and turns on the token gate (§3.3, §5.7). Recorded on each round |
| `pairing.min_games_for_perf` | `5` | Below this, fall back to rating |
| `activity.threshold_weeks` | `3` | Inactivity cutoff (see §8.4; not read while §8.4 is deferred) |
| `activity.missed_starts_to_pause` | `2` | Consecutive missed starts before auto-pause (§6.4); a warning comes one before |
| `player.default_max_concurrent` | `null` | Default cap for new players. `null` = unlimited |
| `player.max_concurrent_ceiling` | `20` | Highest value a player may set, if they set one at all |
| `pairing.odd_pool_strategy` | `double_then_bye` | `double_then_bye` \| `bye_only` |
| `pairing.solver` | `greedy` | `greedy` \| `blossom`: the matching algorithm (§6.2 step 4), recorded on each round |
| `player.default_accepts_double` | `true` | New players absorb odd pools by default, minimising byes |
| `xp.win` / `xp.draw` / `xp.loss` | `3` / `2` / `1` | XP award per result |
| `token.invalid_grace_days` | `14` | Days a lapsed token is tolerated before the player is left out of pairing (`no_valid_token`, §3.3) |
| `rating.unrated_default` | `1500` | Rating assumed for a player with neither a correspondence nor a classical rating (§5.1) |

---

## 5. Scoring, rating and levelling logic

This section is the precise reproduction of the spreadsheet's computations. Implement it as a **pure, unit-tested module** with no I/O, so it can be validated against the historical spreadsheet values.

### 5.1 Base rating

The spreadsheet used `Standings_Backend!T: =MAX(PLAYER_RATING(A2), CLASSICAL_RATING(A2))`. **This is being corrected**, because `MAX` was almost certainly a shortcut for the intent rather than the intent itself.

The purpose of consulting classical rating at all is to estimate the strength of a **newcomer who has no correspondence rating yet** but does have an established classical one — without it they would enter the league effectively unrated and be paired badly for their first few rounds. `MAX` achieves that, but as a side effect it also permanently inflates any established player whose classical rating happens to exceed their correspondence rating, which is not rare and is not intended.

Corrected rule — **prefer correspondence, fall back to classical**:

```
rating(player):
    if correspondence_rating exists:              # provisional or not — see note below
        return correspondence_rating
    if classical_rating exists:                   # provisional or not — newcomer fallback
        return classical_rating
    return UNRATED
```

*(Simplified 2026-09-07: an earlier version of this rule additionally preferred an established classical rating over a provisional correspondence one. The maintainer decided that added a distinction not worth the complexity — any correspondence rating, however new, is closer to what the league is actually measuring than a classical one, and provisional ratings settle quickly as a player accumulates league games.)*

Notes:

- **Provisional** follows Lichess's own definition: the API marks a rating `prov` when its rating deviation is high (roughly RD > 110). It plays no role in this rule beyond being one of the two things that can be missing (see `UNRATED` below) — a provisional correspondence rating is used exactly like an established one.
- **`UNRATED`** players (brand-new Lichess accounts with neither rating) are given the configurable constant `rating.unrated_default` (default 1500) for pairing and power-rating purposes, and flagged as unrated in the standings until they have played `pairing.min_games_for_perf` league games, after which their performance rating takes over (§5.4). The scoring module returns an explicit *unrated* result; substituting the constant is the caller's job, so the constant never leaks into the pure logic. *(Decided 2026-09-07: a fixed constant was chosen over a league-median prior for simplicity.)*
- Once a player has *any* correspondence rating, classical is never consulted again. The fallback is a bootstrap, not an ongoing input.

**Migration impact.** This is a deliberate behavioural difference from the spreadsheet. Any player whose classical rating exceeded their correspondence rating will compute a *lower* base rating here than the sheet showed. When validating a history import (§9.2), expect these players to differ, verify the difference is explained by exactly this rule, and do not "fix" the code to match the old values.

### 5.2 Last-k window

`k = pairing.last_k` (default 5). The window is the player's **k most recently finished games**, ordered by `finished_at` descending. The spreadsheet expresses this as a cut-off timestamp (`LAST_K_CUT_OFF`); a direct `ORDER BY finished_at DESC LIMIT k` is equivalent and simpler.

Within that window:

```
last_k_score = wins + 0.5 * draws          # points out of k
last_k_avg_opponent_rating = mean(opponent_rating_at_game for each game in window)
```

**Opponent rating is the opponent's rating when the game was played** — `Game.white_rating_at_game` / `black_rating_at_game`, taken from `players.{white,black}.rating` in the Lichess game export. This is a deliberate difference from the spreadsheet, which used the opponent's *current* rating (`PLAYER_RATINGS`). Rating-at-game is what a performance rating means (FIDE uses opponents' ratings at the event), it never changes retroactively, and it makes every standing reproducible from the `Game` table alone. *(Decided 2026-09-07.)*

### 5.3 Performance rating

```
last_k_perf_rating = last_k_avg_opponent_rating + delta(last_k_score)
```

`delta` is a lookup on score-out-of-5, reproduced exactly from `Perf_Rating_Backend`:

| Score (of 5) | Delta |
|---:|---:|
| 0.0 | −800 |
| 0.5 | −366 |
| 1.0 | −240 |
| 1.5 | −149 |
| 2.0 | −72 |
| 2.5 | 0 |
| 3.0 | +72 |
| 3.5 | +149 |
| 4.0 | +240 |
| 4.5 | +366 |
| 5.0 | +800 |

#### Where these numbers come from

They are not arbitrary, and they are not specific to this league. This is the **FIDE performance-rating table** (the "rating difference `dp`" table from the FIDE rating regulations), which is the standard way to convert a tournament score into a rating.

The table is really indexed by **score percentage**, not by raw score. With `k = 5`, each half-point is exactly 10%, which is why the spreadsheet's rows line up one-to-one with FIDE's 10% steps:

| Score (of 5) | Score % | FIDE `dp` |
|---:|---:|---:|
| 0.0 | 0% | −800 |
| 0.5 | 10% | −366 |
| 1.0 | 20% | −240 |
| 1.5 | 30% | −149 |
| 2.0 | 40% | −72 |
| 2.5 | 50% | 0 |
| 3.0 | 60% | +72 |
| 3.5 | 70% | +149 |
| 4.0 | 80% | +240 |
| 4.5 | 90% | +366 |
| 5.0 | 100% | +800 |

The underlying idea is the inverse of the Elo expected-score formula. Elo says a rating difference predicts an expected score; performance rating runs that backwards, asking *what rating difference would have predicted the score you actually got?* Roughly:

```
dp ≈ -400 * log10(1/p - 1)          where p = score percentage
```

Sanity-checking against the table: `p = 0.8` gives ≈ 241 against FIDE's 240, and `p = 0.7` gives ≈ 147 against FIDE's 149. The small divergences exist because FIDE's published table derives from a normal distribution rather than the logistic curve, and is rounded. **Use the table, not the formula** — the table is the standard, and matching it means league performance ratings agree with what players see elsewhere in chess.

The **±800 endpoints are a convention, not a computation.** A 100% or 0% score implies an infinite rating difference under the formula (`log10(0)` diverges), so FIDE caps it. This is why a player who wins all 5 of their last games shows a performance rating of *opponent average + 800* rather than something unbounded — and it is worth surfacing that caveat in the UI, because a 2600 performance rating from a 5–0 streak is a soft number, not a claim about strength.

#### Implementing for any `k`

Because the table is percentage-indexed, implement `delta` as a function of `p = score / k`, looking up the FIDE table and **linearly interpolating between the 10% steps**. This has two benefits:

- Changing `pairing.last_k` from 5 to any other value keeps working correctly and requires no new table.
- A partial window (a player with only 3 finished games when `k = 5`) can still be scored on percentage, if the league later decides to show performance ratings before the window is full.

Do **not** index the table by raw score. That silently corrupts every number the moment `k` changes, and the failure is invisible — the ratings still look plausible.

### 5.4 Power rating

```
power_rating = last_k_perf_rating   if the player has >= min_games_for_perf finished games
             = rating               otherwise
```

Mirrors `Standings_Backend!U: =IF(M2<>"-", S2, T2)`. This is the value the pairing engine sorts and matches on.

### 5.5 Colour score

```
color_score = (games played as white) - (games played as black)
color_imbalance = abs(color_score)
```

Positive means the player has had white too often and is due black.

### 5.6 XP and levels

```
xp = 3 * wins + 2 * draws + 1 * loss_count
level = floor(sqrt(xp))
xp_to_next_level = (level + 1)^2 - xp
```

Every completed game awards XP — even a loss. This is deliberate: it rewards participation, so a player who plays constantly climbs levels regardless of results. `last_level_up_round` records the round number in which the player's level last increased.

### 5.7 Eligibility for pairing

A player is paired in a round if **all** hold:

1. `User.status = approved`
2. `PlayerProfile.is_active = true`
3. `PlayerProfile.paused_by_admin = false`
4. `auto_paused_at IS NULL` (not paused by the missed-start rule)
5. Capacity allows it — unlimited by default, see §5.8
6. A valid, unrevoked `challenge:write` token exists — **or** the fallback challenge path is enabled

*(Phase 4, 2026-09-17.)* Until Phase 5 creates games, every game is created by hand — the fallback path is the only path — so item 6 does not gate the pool and no `no_valid_token` exclusion is written. Seeded players have no token at all; a literal reading would empty the pool.

*(Phase 5, 2026-09-28.)* Item 6 applies only under `pairing.game_creation = lichess` (§4.2); under `manual` the Phase 4 reading above still holds. Under `lichess`, "valid, or the fallback path is enabled" means §3.3's rule: a lapsed token is tolerated for `token.invalid_grace_days`, during which the player's games go through the challenge fallback; past it, or with no token row at all, the player is excluded with `no_valid_token`. It is checked last, after items 2–4, so a player who is both paused and tokenless is shown as paused.

Note the difference from the spreadsheet, which used a time-since-last-game threshold (`inactive for < ACTIVITY_THRESHOLD * 7` days) as the sole activity signal. That heuristic is retained only as an *automatic suggestion* to deactivate (§8.4), not as the eligibility rule itself, because players now declare activity explicitly.

### 5.8 Concurrent-game capacity

This is the single most important scheduling rule in the league, and it is new — the spreadsheet had no equivalent.

**Every eligible player receives exactly one new pairing per round. Never more.** `max_concurrent_games` does not control how many games are handed out in a round; it controls how many games a player may have *in flight at once*.

**The cap is opt-in and unlimited by default.** A newly registered player has `max_concurrent_games = NULL`, meaning no limit: they are paired every single week regardless of how many games they already have running. This is the intended default behaviour of the league — a new game every Monday, forever. The cap exists only for players who actively decide their workload has become unsustainable and set a number themselves.

The rule matters because correspondence games at 2 days per move run for weeks while pairings are issued weekly, so an unlimited player's ongoing-game count naturally grows until their completion rate matches one game per week. That equilibrium is fine for most players; the cap is the escape hatch for those it isn't.

```
ongoing_games(player) = count of games where the player is white or black
                        and status is created or in_progress

eligible_for_capacity(player) =
    max_concurrent_games IS NULL                       -- unlimited: always true
    OR ongoing_games(player) < max_concurrent_games(player)
```

**What counts as in flight** *(decided 2026-09-17)*: `Game` rows with `status = in_progress` **plus** pairings of *published* rounds that are still `pending` or `created` with no game ingested yet. A pairing the player has been told to start is a game in flight before Lichess knows about it; without the second term a capped player would be re-paired every week for as long as they delayed their challenge. One query provides the number to the pairing engine, the `PlayerStanding.ongoing` column and the dashboard sentence, so the three can never disagree. A pairing nobody ever starts counts until it is marked `failed` — automatically, by `evaluate-activity` at the next generation (§6.4, Phase 5), or by an admin's *Mark failed* on the round page. A bulk-created game counts from its creation (`status = created`), before `sync-games` has ingested it.

Behaviour:

- A player with **no cap set** (the default) is always in the pool and paired every round.
- A player **under** their cap is placed in the pool and paired exactly once.
- A player **at or over** their cap is **skipped entirely for that round**. They receive no pairing, no bye, and no penalty. They re-enter the pool automatically in the first round after one of their games finishes.
- Being skipped for capacity is **not** an inactivity signal. It must never contribute to missed starts, auto-pause, or the inactivity check-in in §8.4 — a player at their cap is by definition playing a lot.

**Implementation warning.** `NULL` must be handled as *unlimited*, not coerced to zero. A `NULL`-to-`0` coercion bug would silently exclude the entire default population from every round — the pairing engine would appear to run successfully and pair nobody. This case must have an explicit test (§11).

**Worked example** for a player who has chosen `max_concurrent_games = 4`. A default player with no cap would simply read "yes" in every row:

| Round | Ongoing at generation | Paired? | Ongoing after |
|---|---|---|---|
| 1 | 0 | yes | 1 |
| 2 | 1 | yes | 2 |
| 3 | 2 | yes | 3 |
| 4 | 3 | yes | 4 |
| 5 | 4 | **no — at cap** | 4 |
| 6 | 4 | **no — at cap** | 4 |
| 7 | 3 (one game finished) | yes | 4 |

**Evaluation timing.** `ongoing_games` is counted at the moment the round is generated, not when it is published. Games created earlier in the same round are irrelevant since each player is paired at most once. If a round sits in the review window and games finish in the meantime, the pool is not recalculated — regenerating the round is the way to pick up those changes, and admins should be told this in the round diagnostics view.

**Lowering the cap below the current count** is allowed and requires no special handling: the player is simply skipped until natural attrition brings them under the new limit. The same applies to a player who has been unlimited for a long time and then sets a cap below their current load. The dashboard must explain this so it does not look broken (§8.3).

**Removing the cap** returns the player to unlimited and takes effect at the next round.

---

## 6. Pairing engine

### 6.1 Cadence and modes

Pairing generation is **automatic and scheduled**. It runs every Monday via `pairing.cron`. No human action is required in normal operation.

Two publication modes, selectable in admin settings:

- **`review_window` (default).** The round is generated in `draft` state. Admins are notified: from Phase 5, a banner on every admin page says a draft is waiting for review and when it will publish itself (§8.5). If no admin acts within `pairing.review_window_hours` (default 6), the round auto-publishes. An admin may publish early, edit pairings, or cancel the round.
- **`auto_publish`.** The round is generated and published in the same job, with no draft state.

Manual generation (`generated_by = manual`) exists only for exceptions: a failed scheduled run, a Lichess outage, or an off-cycle round. It is not part of the normal weekly flow.

### 6.2 Algorithm

Inputs: the eligible player set (§5.7), each with `power_rating`, `color_score`, `max_concurrent_games`, `ongoing_games`, and recent opponent history.

```
1. Build the candidate pool.
   - Filter to eligible players (§5.7).
   - Apply the capacity rule (§5.8): drop any player whose
     ongoing_games >= max_concurrent_games.
   - Each remaining player appears in the pool EXACTLY ONCE and will
     receive at most one pairing. Do not expand players into multiple
     slots. Record every player excluded for capacity, with their
     ongoing count and cap, for the admin diagnostics view (§8.5).

2. Sort by power_rating descending.
   Mirrors Pairing_Maker's QUERY(... ORDER BY F DESC).

3. Build a weighted matching.
   For every legal pair (a, b), compute a cost in RATING POINTS. Every
   term is expressed in the same unit so they can be summed meaningfully:

     rating_cost = abs(power_rating(a) - power_rating(b))

     color_cost  = pairing.color_weight * color_penalty(a, b)

     repeat_cost = REPEAT_PENALTY if b played a within the last
                   avoid_recent_rounds rounds, else 0

     cost(a, b)  = rating_cost + color_cost + repeat_cost

   COLOR_PENALTY, defined precisely:

     Let cs(x) = color_score(x) = (games as white) - (games as black).
     Playing white adds 1 to cs; playing black subtracts 1.

     Each pair has two possible colour assignments, so take the better:

       colour_penalty(a, b) = min(
           abs(cs(a) + 1) + abs(cs(b) - 1),    -- a white, b black
           abs(cs(a) - 1) + abs(cs(b) + 1)     -- a black, b white
       )

     This is simply the total colour imbalance the pairing LEAVES BEHIND,
     under whichever colour assignment is better. Lower is better. The
     argmin also decides the colours in step 5, so the two steps cannot
     disagree.

     Worked example. cs(a) = +2 (two whites too many), cs(b) = +1:
       a white, b black -> |3| + |0| = 3
       a black, b white -> |1| + |2| = 3
     Equal, so colours are decided by the step 5 tie-break.

     Now cs(a) = +2, cs(b) = -2:
       a white, b black -> |3| + |-3| = 6
       a black, b white -> |1| + |-1| = 2   <- chosen
     Pairing two oppositely-imbalanced players and giving each their
     needed colour fixes both at once, and the penalty reflects that.

   pairing.color_weight (default: 100) converts one unit of leftover
   colour imbalance into rating points. At 100, the engine will accept a
   pairing that is 100 rating points worse in order to remove one unit of
   colour imbalance. Raise it to prioritise colour balance over rating
   accuracy; lower it for the reverse. It is a Setting so it can be tuned
   against real rounds rather than guessed once.

   REPEAT_PENALTY (default: 1_000_000) is a large finite number, NOT
   infinity. A true infinity makes the matching unsolvable rather than
   merely expensive, and prevents the solver from returning any answer at
   all in a small pool. A large finite penalty means repeats are avoided
   whenever possible and permitted only when the alternative is failing to
   pair someone — which is the behaviour we actually want, and which makes
   the relaxation ladder in step 7 a safety net rather than the main path.

4. Solve.
   - Preferred: minimum-weight perfect matching (blossom algorithm) over the
     pool. Deterministic, globally optimal, and the right tool for this shape
     of problem. Go has no canonical blossom implementation in the
     standard ecosystem, so either vendor a maintained one or port a
     reference implementation and cover it with the tests in §11.
   - Acceptable fallback: greedy pairing down the sorted list, taking each
     unpaired player and matching them to the lowest-cost available partner.
     This is what the spreadsheet effectively did.

   Decided 2026-09-17: Phase 4 ships the greedy solver behind a small
   Solver interface; the blossom port is taken up only if real rounds
   show greedy's short-sighted bottom-of-the-list pairings actually
   occurring. Greedy ties are broken by (cost, lower user id, higher
   user id); a blossom implementation must be fed edges in that same
   order so its output never depends on insertion order. With a double
   game in play (step 6a), greedy pairs the volunteer's two slots first,
   with their two cheapest distinct partners — otherwise the scan can
   strand the two copies as the last unmatched pair.

   Amended 2026-09-22: both solvers ship, and the admin chooses with
   `pairing.solver` (§4.2) — `greedy` by default, `blossom` for the
   optimum. Each round records its solver in `settings_used` and the
   round view shows it, so an admin can regenerate a draft under each
   and compare. Blossom is a port of NetworkX's `max_weight_matching`
   (BSD-3-Clause, notice kept in the source file): Galil's O(n³) form of
   Edmonds' algorithm, run in maximum-cardinality mode on weights
   `M − cost` (M above every cost). Every perfect matching has the same
   number of pairs, so the heaviest is exactly the cheapest. It was
   chosen over van Rantwijk's 2008 `mwmatching.py`, from which it
   descends, because that file carries no licence. Edges are fed in the
   greedy tie-break order above.

5. Assign colours.
   Use the argmin already computed by colour_penalty in step 3 — the
   colour assignment that minimises leftover imbalance. In practice this
   means the player with the higher color_score (more whites so far)
   takes black.

   When the two assignments tie (colour_penalty is equal either way),
   break the tie deterministically: lower-rated player takes white, then
   by user id. Never break ties by anything time- or order-dependent, so
   that regenerating a round produces an identical result.

   The volunteer of a double game (step 6a) is the exception: their two
   games are coloured jointly after matching — of the two ways to give
   them one white and one black, take the smaller total leftover
   imbalance, then the same tie-break. The penalty actually realised
   (which may differ from the per-pair argmin the cost used) is what is
   stored on the pairing. *(Phase 4, 2026-09-17.)*

6. Handle the odd pool.
   The pool is odd roughly half the time, because capacity skips (§5.8)
   remove an arbitrary number of players each week. Resolve it in this
   order, BEFORE solving (so the volunteer's second slot is part of the
   matching), and record the outcome on the Round:

   a. DOUBLE GAME (preferred).
      Select one volunteer to play TWO games this round, against two
      DIFFERENT opponents. A pool of N players then yields N+1 pairing
      entries, which is even and matches cleanly.

      A player is a valid double-game candidate only if ALL hold:
        - accepts_double_game = true              (opt-in, §8.3)
        - ongoing_games + 2 <= max_concurrent_games   (cap still respected)
        - they are otherwise eligible (§5.7)

      Choose from valid candidates by longest time since their last
      double game, tie-broken by fewest double games total, then by
      user id. This stops the same person absorbing it every week.

      Assign the volunteer ONE WHITE and ONE BLACK. This is free colour
      balancing and should be treated as a hard constraint, not a
      preference — it means a double game never worsens color_score.

      Their two opponents must be distinct from each other, and the
      normal repeat-opponent rule (avoid_recent_rounds) applies to both.

   b. BYE (fallback).
      If there is no valid double-game candidate, one player sits out.
      Select the player with the LONGEST TIME SINCE THEIR LAST BYE,
      tie-broken by fewest byes total, then deterministically by user id.
      A player who has never had a bye is treated as having waited
      forever, so newcomers are candidates before repeat recipients.

      Explicitly NOT used as selection criteria: rating, level, XP, or
      recent results. Byes must never look like a punishment for being
      weak.

   A bye awards no XP and no rating change, does not count as a missed
   start, never contributes to auto-pause, and is surfaced on the
   player's dashboard with an explanation so it does not read as a
   system failure.

   Record either outcome in the Bye / DoubleGame history so the
   rotation is auditable.

7. Record repeats rather than relaxing constraints.
   An earlier draft of this step relaxed avoid_recent_rounds and then
   colour weight when "no perfect matching exists". With REPEAT_PENALTY
   finite (step 3) the cost graph is complete, so a perfect matching
   always exists — with the duplicated volunteer it is K(n+1) minus one
   edge, still matchable for n >= 3 — and the solver already returns
   the matching with the fewest, least recent repeats; lowering the
   window or the colour weight could never change solvability. The
   engine therefore records, on each pairing, whether it is a repeat
   and of which round (repeat_of_round), and on the Round how many
   repeats it had to accept (repeat_pairings), so an admin can see
   exactly where a small pool forced its hand. Nothing is iterated.
   The "not the same player" and eligibility constraints are hard and
   are never violated. *(Decided 2026-09-17.)*
```

The engine must be **deterministic**: same inputs, same output. Seed any tie-breaks from stable identifiers, never from wall-clock time or map iteration order. This makes the review window meaningful (an admin regenerating sees the same result) and makes the engine testable.

### 6.3 Publication

*(Phase 4, 2026-09-17.)* Publication is a guarded state change: `state = published`, `published_at` set, and `pair_at` set to the same moment (so an early publish never leaves §7.3's `pair_at − 1 day` cutoff ahead of the games players create). Every publish path — the scheduled job, the hourly sweep, an admin's *Publish now*, and generation under `auto_publish` — is the same guarded update (`WHERE state = 'draft'`, after locking the row), so whichever runs first wins and the others are no-ops.

**Game creation** *(Phase 5, 2026-09-28)* follows `pairing.game_creation` (§4.2), which the round records at publication (`Round.game_creation`):

- **`manual`** (the default): publication is the state change only. The pairings stay `manual_external` / `pending`; players challenge each other by hand and `sync-games` finds the games (§7.3). The switch exists so the code can ship and be tested before the organiser account does (§14.3), and so that if automation misbehaves an admin turns it off with one setting and the league carries on by hand.
- **`lichess`**: the round is marked `creating` and a `create-games` job for it is queued **in the same transaction** as the state change. This is the rule for every Lichess call that follows from a change in our data — *database first, Lichess after*: either both the change and its job are written or neither is, and the job then talks to Lichess outside any transaction. (The Phase 4 sketch called Lichess inside the publish transaction; a transaction held open across a 60-second 429 wait is bad, and a crash after Lichess created the games but before our commit would roll back our record while the games exist.)

`create-games` is unique per round while pending or running, and has a longer retry budget than the other jobs (10 attempts, which river's back-off spreads over about four hours) so a short outage is ridden out. It is idempotent and reconciles before acting:

1. Load the round's pairings still `manual_external` / `pending` with no game id. None → the round is `created`; done.
2. **Reconcile.** List the organiser's recent bulks (`GET /api/bulk-pairing`); a bulk created after `published_at` whose games match some of these pairings (same white, same black) is adopted rather than created again, so a retried job never creates the games twice. A retry an admin asks for (§8.5) first runs the §7.3 search, so a game a player has already started by hand is attached rather than duplicated.
3. **Probe** the round's players' tokens with one `POST /api/token/test`. `validate-tokens` (§7) runs weekly and can be a day stale by publication, and a bulk is all-or-nothing, so its result is not relied on alone. A failing token is marked revoked on the spot and its player notified.
4. **Partition** the pairings by token, per §3.3: bulk, challenge, or left to be started by hand.
5. **Bulk.** One request for the whole bulk-capable set, the double-game volunteer's two games included; `days` and `rated` from the round's `settings_used` — the settings it was paired under, not today's; no `pairAt`, so the games start at once; and the league's `message` (§3.2). A bulk is all-or-nothing on the Lichess side: the whole request is rejected (HTTP 400) if any token is missing, invalid, lacks `challenge:write`, or belongs to a closed account, or if the organiser has too many scheduled bulks or games. Partial bulks are never created, and failed bulks do not count against the rate limit. A rejection naming tokens marks them revoked, moves their pairings to the challenge or by-hand partition, and resubmits the rest — at most 3 submissions per run. Any other refusal fails the run, and river retries it. The result is committed right after the call: the bulk id on the `Round`; on each pairing `creation_method = bulk`, `status = created` and the game id.
6. **Challenges**, one call each with the challenger's token (§3.3), each committed on its own right after its call, so a crash can at worst leave one duplicate challenge. The pairing becomes `creation_method = challenge`, `status = pending`, with the challenge id as its game id. A failed call leaves that pairing to be started by hand and does not stop the others.
7. The round is `created` with `games_created_at`; players are notified (§10); a `sync-games` run is queued so the new games reach the Overview within minutes rather than an hour.

**When Lichess keeps refusing** *(decided 2026-09-24)*, the players start those games by hand rather than losing the week. On `create-games`' last failed attempt the round's creation is marked `gave_up`, its pairings stay `manual_external` / `pending`, and both players of each are told on-site to challenge their opponent by hand; the dashboard shows the challenge link. A pairing nobody then starts gets the 48-hour reminder and is retired at the next generation, with no missed start (§6.4). An admin can *Retry game creation* from the round page (§8.5); players whose games the retry creates are told their game now exists and not to start another. This replaces the earlier rule of marking the remaining pairings `failed`.

A pairing's status only ever moves with a guarded update (`WHERE status = 'pending' AND lichess_game_id IS NULL`), so an admin's *Mark failed* in the meantime wins.

**A published round is final** *(decided 2026-09-24)*. After publication an undo cannot be clean: the games exist, Lichess has messaged the players, hand-started games are out of reach, and aborting bulk games would rest on undocumented behaviour. The review window is the undo; a mistake after it is lived with or repaired pairing by pairing (*Mark failed*). Voiding a single existing game — a player banned for cheating mid-week, say — is left to Phase 6's player management.

Verified limits (OpenAPI v2.0.174, 2026-09-24): `days` must be one of 1, 2, 3, 5, 7, 10, 14; at most 500 games per bulk and 500 games per 10 minutes; a custom `message` must contain the `{game}` placeholder. Correspondence bulks may include the same player in more than one game — documented since v2.0.174 — which is what makes the double game in §6.2 part of the single request. The `pairAt` horizon, documented inconsistently ("up to 24h in advance" in the endpoint text, "up to 7 days" on the field; the server enforces 7 days), no longer matters: games are created at publication, no bulk is ever scheduled ahead, and so none ever needs cancelling.

### 6.4 Unstarted pairings and missed starts

With bulk pairing, games are created outright and a "missed start" is effectively impossible. The rule still applies to the fallback challenge path. *(Rewritten for Phase 5, 2026-09-28.)*

`evaluate-activity` runs as the **first step of every generation** — scheduled, *Generate now*, and `ic generate-round` — in its own transaction with its own job run, then generation follows; a failed evaluation is visible on the job log but does not block the round. It runs before generation rather than after, because a player who reaches the pause threshold must already be out of the round generated next; run afterwards, they would be paired again and their next challenge would go unaccepted too.

- **Every still-`pending` pairing of a published round is marked `failed`**, whatever its kind: an unaccepted challenge, or a hand-started pairing nobody started (both tokens lapsed, game creation gave up, or the whole league under `pairing.game_creation = manual`). It stops counting toward capacity (§5.8), which ends the admin's weekly *Mark failed* chore; the button stays for exceptions.
- **A `MissedStart` is recorded only for a challenge pairing, for the player who had to accept** — the one case where the league knows who did not act. For a hand-started pairing it cannot tell: either player could have challenged, and Lichess shows no challenge that was sent and ignored. When automation gave up, the failure was the league's, not the players'. Those pairings are retired without a missed start and never lead to auto-pause — the same outcome as before Phase 5, minus the chore. Never for a bye or a capacity skip either (§5.8, §6.2 step 6b).
- The missed challenge is **withdrawn on Lichess** (`POST /api/challenge/{id}/cancel` with the challenger's token, run by a `cancel-challenge` job queued in the same transaction, §6.3), so a late acceptance cannot create a game that never counts.
- **Consecutive** means missed starts recorded after both the player's latest started league game and their latest *resume quest* (`PlayerProfile.resumed_at`). At `activity.missed_starts_to_pause − 1` the player is warned; at `activity.missed_starts_to_pause` (default 2) `auto_paused_at` is set, they leave the pool, and they are notified with a one-click resume link. Any started game, and resuming, resets the count.
- A pairing still not started **48 hours after publication** (challenge or hand-started) sends one reminder (§10), from the hourly `sync-games`.

This reproduces the current rule: *"Failure to start a game 2 weeks in a row will result in your quest being paused and you will receive no new pairings until you resume quest."* — applied where a failure to start can be attributed to one player.

---

## 7. Background jobs

Every job must be idempotent, retryable, and record its outcome. A `JobRun` table (or the scheduler's own history, if it persists one) should capture: job name, started/finished timestamps, status, items processed, error detail. The admin dashboard reads from this — the silent-failure problem is the single most important thing this rebuild fixes.

**Design principle: this league moves slowly, and the jobs should too.** Games run at 2 days per move and pairings are issued weekly. Nothing here is time-critical, and frequent polling would buy no user-visible benefit while consuming Lichess rate limit and adding failure surface. An earlier draft of this spec ran three jobs every 15 minutes; that was over-engineering and has been cut.

| Job | Schedule | Responsibility |
|---|---|---|
| `validate-tokens` | **Weekly**, at `pairing.cron` − 1h | Test every stored token (and the organiser's), mark revoked, notify affected players — so the pairing pool is accurate when it matters |
| `evaluate-activity` | **First step of every generation** (scheduled, admin, CLI) | Retire unstarted pairings, record missed starts, warn and auto-pause (§6.4) |
| `generate-round` | Weekly (`pairing.cron`) | Run the pairing engine, create a `draft` or published `Round` |
| `publish-round` | **Scheduled once**, at the round's `publish_at` | Publish a `draft` round when its review window expires |
| `publish-round-sweep` | **Hourly** | Safety net: publish any `draft` whose `publish_at` has passed (a job lost to a restart or a failed enqueue) |
| `create-games` | **Queued by publication** under `pairing.game_creation = lichess`; an admin's *Retry*; `ic create-games` | Create the round's games on Lichess: bulk, challenges, give up to hand-started (§6.3) |
| `cancel-challenge` | **Queued by a missed start** | Withdraw the unaccepted challenge on Lichess (§6.4) |
| `sync-games` | **Hourly**, and once right after `create-games` | Single merged job: reconcile pairing status, match games to pairings that have no game id yet (§7.3), ingest newly finished games, update standings and levels; send the 48-hour "not started yet" reminder (§6.4) |
| `refresh-ratings` | Daily | Update `RatingSnapshot` for all approved players |
| `recompute-aggregates` | Nightly | Full recompute of standings and stats as a self-healing backstop against incremental drift |

Reasoning behind the changes:

- **`sync-ongoing` and `sync-completed` are merged into `sync-games`.** They walk the same set of pairings and call the same endpoints; splitting them doubled the API traffic for no gain. One job, one pass.
- **Hourly, not every 15 minutes.** The worst case is that a finished game appears on the standings up to an hour late. In a league where a single game takes weeks, nobody will notice, and no decision depends on it. If results feeling "live" turns out to matter to players, this is a one-line settings change — but start slow.
- **Ongoing detection barely needs polling at all.** Bulk pairing returns the game IDs at creation time (§3.4), so games created on the primary path are known immediately, with no polling. Only fallback challenges need to be watched for acceptance, and those are meant to be rare.
- **`publish-round` is scheduled, not swept.** `river` can enqueue a job to run at a specific future time, so when a round enters `draft` the publish job is scheduled directly for its `publish_at`. The hourly sweep exists only to catch a job lost to a restart or a failed enqueue — belt and braces, not the mechanism. The job is unique on `{round id, publish_at}` — not on the round id alone — because river treats a completed job with identical args as a duplicate, and a regenerated round would otherwise keep its old schedule. Cancelling or regenerating a draft cancels its pending job. *(Phase 4, 2026-09-17.)*
- **`generate-round` runs on `pairing.cron`**, the first wall-clock schedule in the system (the sync jobs run on intervals). The schedule is built when the server starts; a firing missed while the binary was down is not caught up — manual generation (§6.1) covers that, and the admin round page says so. *(Phase 4, 2026-09-17.)*
- **`validate-tokens` moved from daily to weekly, timed just before pairing.** Token validity only affects one decision — who can be bulk-paired — and that decision is made once a week. Checking daily made the same API calls seven times to influence one outcome. Running it shortly before `generate-round` puts the freshest possible data where it is actually used. Tokens that fail mid-week are caught anyway, because a failed API call marks the token invalid on the spot.
- **`validate-tokens` runs exactly one hour before `pairing.cron`** *(Phase 5, 2026-09-28)*: its schedule is the parsed `pairing.cron` shifted by −1h, so it follows the setting, `CRON_TZ=` included. With a review window the pool it gates can be a day old by publication, so `create-games` probes the round's tokens again before any bulk (§6.3).
- **Lichess calls that follow from a change in our data run in jobs queued in the same transaction as the change** *(Phase 5, 2026-09-28)*: `create-games` with a round's publication, `cancel-challenge` with a missed start. Either both are written or neither is, no transaction is held open across a Lichess call, and each job reconciles before acting, so a retry never acts twice (§6.3).
- **`evaluate-activity` runs before generation, not after it** *(Phase 5, 2026-09-28)*, so a player who has just reached the pause threshold is already out of the round being generated (§6.4). It no longer flags long-inactive players: the §8.4 check-in is deferred.
- **Every generation path queues its draft's publish job** *(Phase 5, 2026-09-28)*: the web layer and the CLI get a job enqueuer, so a draft made with *Generate now* or `ic generate-round` publishes when its window ends rather than up to an hour later from the sweep.

Both `sync-games` and `generate-round` must also be **manually triggerable from the admin panel**, for the case where an outage means the data is stale and someone wants it fixed now rather than at the top of the hour.

### 7.1 Ingestion pipeline

For each finished game:

1. Fetch with `accuracy=true`, `opening=true` (not `evals`, not `clocks` — see §3.4).
2. Store the complete response in `Game.raw_payload`. This is non-negotiable — it allows every derived metric to be recomputed later without re-fetching, which the spreadsheet could not do.
3. Map to the normalised `Game` columns. For a bare `draw` status, replay `moves` with a chess library to classify threefold / fifty-move / insufficient material; anything else is `draw_agreement`; an unparsable move list yields `draw_other`.
4. Upsert on `lichess_game_id`. In-progress games are stored too (`status = in_progress`, result null) so the Overview page and the `ongoing` count read from the same table; the row is updated in place when the game finishes.
5. Recompute the two players' `PlayerStanding` rows (incremental).
6. Detect level-ups and emit notifications.

### 7.2 Rate limiting and resilience

- Respect Lichess rate limits. Lichess's rule is "only make one request at a time": the client serialises all outbound calls. On HTTP 429, wait at least 60 seconds before a single retry, then let the job fail and be retried by the scheduler.
- All outbound calls go through a single client with a shared limiter, timeouts, and bounded retries with exponential backoff.
- A Lichess outage must degrade gracefully: the site keeps serving cached data, jobs retry, and admins are alerted rather than the system silently doing nothing.
- **How a run's outcome is recorded.** A job that makes many Lichess calls (`sync-games` makes one per white player with an unmatched pairing) records a failed call and carries on with the rest. The run is `failed` if any database write fails (the run stops at that point) or if every Lichess call it attempted failed. Otherwise it is `succeeded`; if some calls failed, the run's error text still names them, so they are visible in the job log and in `/health`'s last error without turning the health check red for a single persistently failing account. A run with nothing to fetch has made no calls and has succeeded. A `failed` run returns an error to the scheduler so it is retried. *(Decided 2026-09-13.)* A run whose work is cut short by the scheduler's job timeout or by shutdown is `failed` with that reason, however many calls succeeded before it; its outcome is still recorded, and any run found still `running` when the server starts (its process died) is marked `failed` as interrupted. *(Decided 2026-09-13.)*

### 7.3 Matching games to pairings without a game id

A pairing can lack a `lichess_game_id` in two cases: a fallback challenge (§3.3) that has not yet been accepted, and a `manual_external` pairing — in particular every pairing imported from the spreadsheet during the transition (§12, Phase 1), when the league is still being paired by the sheet and games are still being created by hand.

*(Amended 2026-09-28.)* Only the second case remains. A challenge's id is its game's id once accepted (§3.4), so a fallback challenge is stored with that id and re-checked by id like a bulk game, never searched. The pairings searched are the `manual_external` ones players start by hand: imported pairings, every generated pairing under `pairing.game_creation = manual`, and under `lichess` those whose two players both lack a working token or whose game creation gave up (§3.3, §6.3).

Only pairings of **published** rounds are matched or re-checked. A draft's pairings must never reach Lichess: with `pair_at − 1 day` as the cutoff, the hourly job would otherwise attach any rated two-day game two draft opponents happened to start into a round that does not exist yet, and the resulting `Game` row would block the draft's regeneration. *(Phase 4, 2026-09-17.)*

For each such pairing, `sync-games` fetches the **white** player's games with `GET /api/games/user/{white}?perfType=correspondence&rated=true&since=<round.pair_at − 1 day>&ongoing=true&finished=true&opening=true&accuracy=true` and keeps candidates where:

- `players.white.user.id` is the pairing's white player and `players.black.user.id` its black player (colours must match — a game with reversed colours is not this pairing),
- `variant = standard`, `rated = true`, `daysPerTurn = pairing.days_per_move`,
- `createdAt >= round.pair_at − 1 day`,
- the game id is not already attached to another pairing.

Exactly one candidate → attach it and ingest. None → leave pending (the game may not exist yet). More than one → set `match_ambiguous = true`, attach nothing, and surface it in the admin round view; an admin picks the game (or marks the pairing failed). Never guess.

Since the search is anchored on a pairing, games members play against each other outside the league are never ingested. This is the only discovery mechanism; there is no "scan everyone's games" mode.

---

## 8. Web application

### 8.1 Public pages (no auth)

**Home / Overview** — reproduces the `Overview` sheet.
- Ongoing games: white, black, round number, link to the Lichess game, days since start. Populated minutes after publication by the `sync-games` run that game creation queues (the bulk-pairing response gives the game ids, §6.3) and kept current by `sync-games`, so this reflects reality rather than being reconstructed after the fact.
- Recent results feed: most recently finished games with result and round.
- Current top 5 by power rating.
- Active player count.
- Rules and FAQ (fair play, pairing rules, time control, inactivity policy) — editable by admins as rich text, seeded with the existing text.

**Standings** — reproduces the `Standings` sheet.
- Columns: player, rating, games, wins, draws, losses, ongoing, last-5 score, last-5 performance rating, active flag.
- Sortable on every column, filterable by active/inactive, searchable by name.
- Player names link to Lichess profiles and to the internal player page.

**Levels** — reproduces the `Levels` sheet.
- Columns: rank, player, XP, level, XP until level-up, last level-up (round number when known, otherwise date).
- Explanation of the XP rules inline (win 3 / draw 2 / loss 1, level = √XP).

**Player profile**
- Header: name, rating, level with progress bar, active status, W/D/L, member since.
- Full game history: opponent, colour, result, round, opening, accuracy, link to game.
- Charts: rating over time, XP over time, results distribution.

**Stats** — reproduces the `Stats` sheet. **Deferred out of Phase 1** (decided 2026-09-07): the draw-subtype inference, the analysed-vs-unanalysed caveat and the per-round series all raise questions better answered once real data is flowing. Build after Phase 4.
- Termination breakdown (mate, resign, clock flag, draw by agreement, threefold, fifty-move) with counts and percentages. Draw subtypes are inferred by replay (§7.1) and the page must say so.
- Result distribution (white win / black win / draw).
- Most common first replies to 1.e4 and 1.d4, with count, share, and white W/D/L split.
- Average game duration.
- Accuracy / centipawn-loss figures exist only for games someone has requested analysis on at Lichess; show "n of m games analysed" next to any such figure.
- Players paired per round, as a time series.

**Every page** — the footer links to the source code and the licence (§13): AGPL-3.0 §13 asks that a site's users be offered its source.

> **Not built:** the spreadsheet's Awards page (Archbishop of Accuracy, Compensation Addict, Ace) is out of scope and is not being ported.

### 8.2 Registration and authentication

**Flow:**

1. Visitor clicks "Join the league".
2. Redirected to Lichess OAuth, requesting `challenge:write`, the only player scope (§3.1).
3. On callback: create `User` with `status = pending`, store the encrypted token, capture Lichess profile data (username, ratings, account creation date, whether flagged/closed).
4. Account page explaining that an admin will review the application, and what happens next.
5. Admin approves or rejects (§8.5). On approval the player becomes eligible for the next round and receives a notification (Phase 5; until then the status is visible on the account page).

*(Reordered 2026-09-15.)* The fair-play agreement is a single checkbox on the join page, ticked **before** the redirect to Lichess, so the callback creates a complete application in one transaction — there is never a half-registered user and the token is never held between two requests. The earlier design's post-callback form is gone, and with it the timezone field (§14.4: nothing used it). Nothing else is asked — no email, and no game limit, since unlimited is the default (§5.8).

**Why OAuth rather than a username field:** it proves the applicant controls the account, and it collects the `challenge:write` token needed for automated game creation in the same step. A plain username form would let anyone register as anyone. The username is never typed: it is whatever `GET /api/account` returns for the token Lichess issued, so Lichess's own uniqueness is the only identity check needed. *(An earlier draft also flagged usernames resembling an existing member's; dropped 2026-09-15 as redundant for that reason.)*

**Signals to surface to the reviewing admin:** account age, number of rated games, whether the account is marked TOS-violating or closed, current correspondence and classical ratings.

**Signing in.** Existing members use the same OAuth flow ("Sign in"); the callback matches on the stable Lichess `id`, follows a rename, refreshes the stored profile and replaces the stored token — this is also §3.1's one-click re-authorisation. A non-member who signs in rather than joins is told so and their fresh token is revoked; a banned account gets no session. A rejected applicant cannot re-apply themselves — signing in shows the rejection reason; an admin can still approve from the queue's rejected tab.

**Bootstrap admins.** Accounts listed in `ADMIN_LICHESS_USERNAMES` are made admins when they sign in; a listed account with no row yet goes through "Join" like anyone else and is created approved and admin (the first admin has nobody to approve them). Granting or revoking admin from the UI is §8.5 player management.

**Sessions:** server-side sessions with httpOnly, secure, SameSite=Lax cookies (Lax, because the OAuth callback is a top-level navigation from lichess.org). The cookie carries a random id; only its SHA-256 is stored, with a 30-day absolute lifetime. The Lichess token is used only for API calls, never as a session credential.

### 8.3 Player dashboard (auth required)

*(Built 2026-09-15, Phase 3, at `/account`.)* Pending applicants already see and may set the preferences below; they take effect once approved. Rejected applicants see their status only. Every control is a plain form submission with an inline validation message, and every change writes to `AuditLog` with the field's previous and new value.

- **Activity toggle.** "I'm playing" / "Pause my quest". Pausing takes effect for the next round; existing games are unaffected and must still be finished.
- **Concurrent games limit.** Presented as an optional setting, off by default, not a number the player must reason about on day one.

  Default state — a checkbox or toggle, unchecked:

  > **Limit how many games I play at once** — off
  > You'll get a new game every week. Turn this on if you'd rather cap how many run at the same time.

  When enabled, reveal a number input bounded by `player.max_concurrent_ceiling`:

  > You'll be paired for **one new game each week**, as long as you have fewer than **{cap}** games in progress. Hit the ceiling and you'll simply sit out until one finishes — no penalty, and you're back automatically.

  Show the live count next to the control: *"3 of 4 games in progress — you'll be paired this week."* or *"4 of 4 games in progress — you'll be skipped until one finishes."* For an unlimited player: *"6 games in progress — no limit set, you'll be paired every week."*

  If the player sets a cap below their current ongoing count, show a calm explanation rather than an error: *"You're above your new limit. You won't get new pairings until you're back under it. Nothing happens to your current games."*
- **My games.** Ongoing (with links and days-since-last-move) and completed.
- **Double games.** A toggle, **on by default**, so byes stay rare without anyone having to opt in: *"If there's an odd number of players some week, I'm happy to play two games instead of someone sitting out."* State that it happens occasionally rather than weekly, that it respects any cap they've set (it needs two free slots), and that they get one white and one black. Show how many times they've absorbed a double game. Turning it off is always allowed and carries no penalty.
- **Bye history.** If the player received a bye, show it on their dashboard for that round with plain-language reasoning: *"Odd number of players this week and nobody was free for a double game, so you sat out. You're first in line to avoid the next one."*
- **This week** *(Phase 4, built 2026-09-22)*. For the latest published round: the player's pairing (opponent, colour, and — until Phase 5 creates the game — a link to challenge them on Lichess), or the bye sentence above, or the `RoundExclusion` reason in plain words (at capacity, with the count and the cap; paused; inactive; removed by an admin). With it, the double-game count in the double-games section and the bye count with the last bye's round. As built: a draft is never shown (it can still change); the double-game volunteer sees both games; a pairing marked failed says "marked as not played"; the link becomes the game's once `sync-games` has found it; a player with neither a pairing nor an exclusion row was approved after the round was generated and is told they'll be in the next one; under `bye_only` the bye sentence drops its double-game clause, which would be false.

  *(Phase 5, 2026-09-28.)* The paired sentence is worded for how the game is being created: **bulk** — the game link; **being created** — "your game is being created"; **challenger** — "we challenged X for you, waiting for them to accept"; **challenged** — "accept X's challenge", with the link; **by hand** (both tokens lapsed, creation gave up, or `pairing.game_creation = manual`) — as in Phase 4, the link to challenge them. A player one missed start from auto-pause sees the warning here (§6.4).
- **My standing.** Rating, record, last-5 performance, level and XP progress.
- **Lichess authorisation status.** Green if valid; if revoked or expired, a prominent re-authorise button explaining that games cannot be created automatically without it. *(Until `validate-tokens` ships in Phase 5, "valid" means stored and not past `expires_at`; a revocation made on Lichess's side is not detected yet, and the copy says the token was checked at sign-in.)* *(Phase 5, 2026-09-28.)* The status comes from the last check (`last_validated_at`, `revoked_at`). A lapsed token raises a banner at the top of the page saying in plain words what it costs — the player's games can no longer be created automatically, and after the grace period they will not be paired at all — with the days of grace left and the re-authorise button (the ordinary sign-in). The banner shows whatever the player's notification preferences: it is the one channel certain to reach them in that state.
- **Notifications** *(Phase 5)*. A bell with the unread count in the site header for a signed-in player; `/account/notifications` lists them, with mark one or all read; a checkbox per opt-out-able category (§10) on `/account`.
- **Resume quest.** Visible only when auto-paused, clearing `auto_paused_at`. A player paused by an admin sees the reason and no button: that pause is the admin's to lift (§8.5). Resuming also sets `resumed_at`, so earlier missed starts no longer count towards the next pause (§6.4).

Every change writes to `AuditLog`.

### 8.4 Activity suggestions

**Deferred** *(decided 2026-09-24)*. Once games are created automatically, a player who stops playing still gets a game every week, and each ends by timeout — so "has not finished a game in 3 weeks" no longer describes a dormant player. The check-in is revisited once there is real data (for example, a run of losses on time). The original design, kept for reference:

The old `ACTIVITY_THRESHOLD` heuristic is retained, but demoted from a hard rule to a nudge:

- If a player has not finished a game in `activity.threshold_weeks * 7` days and has no ongoing games, the system notifies them on-site asking whether they want to stay active, with one-click "stay active" / "pause me" links.
- No response after a further 7 days sets `is_active = false` automatically, with a notification.

This replaces the admin manually flipping the `active` column.

### 8.5 Admin panel (role = admin)

- **Registration queue.** Pending applications with the Lichess signals from §8.2, approve/reject with reason, bulk actions. Approving several at once is one transaction — if any selected application has changed state meanwhile, none are approved. Approval also computes the player's first `PlayerStanding` row so they appear on the standings at once. A rejected tab lists past rejections and allows approval from there. *(Built 2026-09-15.)*
- **Player management.** Search, view, edit any profile; pause/unpause; adjust `max_concurrent_games`; grant/revoke admin; ban.
- **Round management** *(Phase 4)*. List rounds with state; view a round with the full pairing table; on a draft: edit a pairing (swap opponents between two pairings — colours re-derived by the engine's colour step; flip colours; remove a pairing, which writes a `removed_by_admin` exclusion for both players and drops the volunteer's double-game record if it was one of theirs), publish now, cancel with a reason, regenerate in place (same number, fresh pairings and diagnostics). On a published round: mark a pending pairing failed (§5.8). Edits are refused server-side on anything but a draft. Every edit is attributed in `AuditLog`, sets `edited_by`, and nulls the row's stored diagnostics so the view shows "edited" instead of numbers that no longer hold. **A published round is final** (§6.3): it cannot be cancelled. *(Amended 2026-09-28; an earlier draft planned cancelling a published round in Phase 5.)* *(Phase 5.)* Per pairing, the view shows how its game is being created (bulk, challenge, by hand), its status and the game link; for the round, a game-creation line — by hand (`manual`), being created, created at … with the bulk id, or gave up with a link to the failed job — and *Retry game creation* when creation gave up or some pairings are still to be started by hand. The retry first runs the §7.3 search for the round's pending pairings, so a game a player already started by hand is attached rather than duplicated.
- **Pairing diagnostics** *(Phase 4)*. For any round: pool size, how an odd pool was resolved and for whom, the repeats accepted (§6.2 step 7), the settings as they were at generation (the stored `settings_used` snapshot, solver included; an imported round says it has none), per-pair rating gap, colour penalty and repeat-of-round, and every player excluded from the pool with the reason and, for capacity, the count and the cap. The page reminds the admin that the pool is not recalculated during the review window — regenerate to pick up changes (§5.8).
- **Manual generation** *(Phase 4)*. Trigger `generate-round` off-cycle. It runs in the request (the engine is milliseconds and makes no Lichess call) and records a job run like the scheduled one; the database allows at most one draft, so a second generation — from the page or the scheduler — fails with a clear message instead of racing. Running in the request means it has no job runner to schedule the draft's publication, so a manually generated draft is published by the hourly `publish-round-sweep`, up to an hour after its window ends; *Publish now* is immediate. The same holds for `ic generate-round`. *(Phase 5, 2026-09-28.)* The web layer and the CLI gain a job enqueuer, so a manually generated draft gets its own publish job and publishes when its window ends, like a scheduled one; the sweep goes back to catching lost jobs only. Generation from any path runs `evaluate-activity` first (§6.4).
- **Tokens** *(Phase 5)*. `/admin/tokens` lists the players without a valid token, since when, and the days of grace left (§3.3), and the organiser token's state and expiry.
- **Settings.** Edit everything in §4.2 from a form, with validation and an audit trail.
- **Content.** Edit the rules/FAQ text.
- **System health.** Last run and outcome of every job, recent failures with stack traces, Lichess API error rate, count of players with invalid tokens, organiser token expiry. This page is the direct answer to the original reliability problem. *(Phase 5, 2026-09-28.)* Until the full page (Phase 6), admin alerts are **computed, not stored**: a banner on every admin page, built from the same data as `/health`, shows a draft waiting for review and when it will publish itself (§6.1), a watched job whose latest run failed, and an organiser token that is invalid or expires within 14 days. `/health` watches the Phase 5 jobs, reports the organiser token, and returns 503 when `pairing.game_creation = lichess` and that token is invalid — the P1 alert of §3.2.

---

## 9. Data migration (DECLINED)

**Decided 2026-09-07: historical games are not imported.** The site starts with an empty game record. What *is* imported, during Phase 1, is the small set of **currently open pairings** from the spreadsheet (round number, white, black, and the game id where the sheet has it), as `manual_external` pairings — see §7.3 and §12. Round numbers on those pairings are taken from the sheet as-is, and the pairing engine continues the sequence from the highest imported round.

The remainder of this section is kept for reference in case the decision is revisited.

If history is imported, it must be the **full historical game record**, not a standings snapshot — the stats pages depend on per-game opening, accuracy, termination and CPL data, and a snapshot would leave those pages empty for all pre-launch games.

### 9.1 Scope

Source: the `RawData` sheet, ~1,500+ games with these columns:

```
ID, Round, White, Black, Termination, Results,
White_Accuracy, Black_Accuracy, W_Move_1, B_Move_1, Opening,
Start_Date, Termination_Date, Duration,
w_comp, b_comp, w_total_moves, b_total_moves, w_total_CPL, b_total_CPL
```

### 9.2 Procedure

1. Export `RawData` to CSV.
2. Reconcile every distinct player name against Lichess accounts. Names in the sheet are inconsistently cased (`MilsBees` vs `milsbees`); match case-insensitively and store canonical casing. Produce a manual review list for unmatched names — players who left, renamed, or closed their accounts.
3. Create `User` rows with `status = approved` for historical players who are not registering fresh, marked `is_active = false` so they are not paired until they opt in.
4. Import games, keyed on the Lichess game ID from the `ID` column. Where possible, **re-fetch each game from the Lichess API** rather than trusting the sheet, so `raw_payload` is populated and derived metrics can be recomputed later. Fall back to the sheet's values for games no longer retrievable.
5. Recompute all aggregates from scratch: standings, XP, levels, stats.
6. **Validate.** Compare computed standings, XP and levels against the spreadsheet's current values, player by player. Any discrepancy indicates a misunderstanding of the original logic and must be resolved before launch. This validation is the single best test of §5.

---

## 10. Notifications

**No email.** There is no SMTP dependency, no transactional email provider, and no email address is collected or stored. This removes an entire category of deliverability problems, spam-folder support requests, and personal data to protect.

**One channel: the on-site notification centre.** A persisted `Notification` record per player, surfaced as a bell icon with an unread count and a list on the dashboard (§8.3). Always available, requires no external service, and is the fallback for everything.

**The only Lichess message is the one bulk pairing sends.** A bulk's `message` reaches each player from the organiser account when their game is created (§3.2). There are no other Lichess private messages. *(Amended 2026-09-28.)* An earlier draft made Lichess PMs a second, per-player channel, sent with each player's `msg:write` token; that cannot work, because a message is sent as the token's owner (§3.1). League PMs would have to come from the organiser account, which may start only about 20 new conversations a day. Custom PMs, if still wanted, come from the organiser account in Phase 6 at the earliest.

| Event | Category | Recipient | Channel |
|---|---|---|---|
| Registration approved / rejected | `registration` | Applicant | On-site |
| Round published: paired, worded for how the game is created | `round` | Each paired player | On-site; for a bulk game also the bulk's Lichess message |
| Assigned a double game | `round` | Volunteer | On-site (explains both games) |
| Received a bye | `round` | Player | On-site (explains why, and that they're prioritised next) |
| Game creation gave up: challenge your opponent by hand | `round` | Both players of each pairing left | On-site |
| Retried game creation created your game: don't start another | `round` | Both players | On-site |
| Challenge to accept | `challenge` | Player who must accept | On-site |
| Game not started 48 hours after publication | `unstarted` | Challenge: the player who must accept; by hand: both players | On-site, once |
| One missed start from auto-pause | `missed_start` | Player | On-site |
| Auto-paused | `auto_pause` | Player | On-site, with resume link |
| Lichess token revoked | `token` | Player | On-site, and a dashboard banner, with re-auth link |
| Level up | `level_up` | Player | On-site |
| Draft awaiting review, job failure, organiser token invalid or expiring | — | Admins | Banner on admin pages, computed (§8.5) |
| Inactivity check-in | — | — | Deferred with §8.4 |

Notes:

- A player whose token has lapsed may no longer be visiting the site. Token-problem notifications must therefore also appear as a dashboard banner, shown regardless of preferences.
- All player-facing categories must be individually opt-out-able in the dashboard, except the account-critical ones: `registration`, `auto_pause`, `token`.
- Notifications are **idempotent**: each carries a `dedupe_key` (e.g. `round:201:paired:<pairing id>`), unique per player when set, so a retried job never notifies twice. *(2026-09-28; per player since 2026-09-29, as the example key needs: both players of a pairing get a notice under it.)*
- Because there is no email, **admins must not rely on notifications reaching a dormant player.** The inactivity flow in §8.4 should assume a player may never see the check-in, and its automatic deactivation is the mechanism that matters, not the message.

---

## 11. Non-functional requirements

**Reliability.** Every background job is idempotent and retryable. Job outcomes are persisted and surfaced in the admin health page. Failures alert admins actively rather than waiting to be noticed.

**Security.** *(Mechanisms fixed 2026-09-15, Phase 2.)*
- Player OAuth tokens encrypted at rest (AES-256-GCM, key from `TOKEN_ENCRYPTION_KEY`, held outside the database); never logged, never returned by any API response, never rendered in any template. In code the plaintext is a distinct `Secret` type that prints as `[redacted]`.
- Admin routes behind role checks enforced server-side on every request, not merely hidden in the UI — one `/admin` route group with the check as middleware.
- CSRF protection on all state-changing requests via the standard library's cross-origin protection (`Sec-Fetch-Site` / `Origin` vs `Host`) plus SameSite=Lax cookies; no per-form token. Rate limiting on registration and auth endpoints: an in-memory per-IP token bucket (10 per minute).
- The OAuth callback verifies `state` (from an HMAC-signed, 10-minute cookie carrying the PKCE verifier) before anything else, and is kept out of the request log so the one-time code never appears there.
- Token revocation on account deletion, and a documented deletion path (GDPR — a European user base is likely given the existing roster). The data footprint is deliberately small: a Lichess username, an encrypted token, and game history. No email addresses are held.

  **The deletion path is a precondition of opening registration to the public, built in Phase 6 against the final schema** *(decided 2026-09-15)*. Every later phase adds per-user tables, so a path written earlier would be re-opened in each of them or silently miss one. Until then the roster is the maintainers' test accounts and the documented path is an admin deleting the rows by hand. Design intent, to be implemented then: revoke the token on Lichess, delete the token, sessions, rating snapshots, profile and standing, anonymise the `User` row (username, Lichess id, profile) and **keep the `Game` rows** — opponents' results, XP and performance ratings are computed from them, and the games are public on Lichess regardless. Tables carrying a `user_id` today, the checklist for that work: `player_profiles`, `rating_snapshots`, `oauth_tokens`, `sessions`, `player_standings`, `pairings` (white/black, `edited_by`), `games` (white/black), `audit_log` (actor, and `entity_id` for user actions), `settings.updated_by`, `users.approved_by`; from Phase 4, `round_exclusions`, `byes`, `double_games`; from Phase 5, `notifications`, `notification_preferences`, `missed_starts`.

**Performance.** Public pages read from materialised `PlayerStanding` and cached aggregate tables, never computing rolling performance ratings per request. Standings and stats pages should render in well under a second at 200+ players and 10,000+ games.

**Testing.**
- Unit tests for the entire scoring module (§5) with fixtures drawn from real spreadsheet values.
- A dedicated test asserting that a player with `max_concurrent_games = NULL` is paired every round regardless of ongoing game count, and is never excluded for capacity. This is the default state of every player, so a regression here breaks the entire league silently (§5.8).
- Unit tests for the pairing engine covering: colour balancing, repeat avoidance, capacity caps, an unavoidable repeat being recorded rather than relaxed away (the relaxation ladder is unreachable with a finite penalty, §6.2 step 7), each solver (the blossom solver checked against brute force on small pools and against its optimality certificate on large ones), and specifically the odd-pool path — double-game selection with and without valid volunteers, the two-distinct-opponents rule, the one-white-one-black rule, cap enforcement at `ongoing + 2`, and bye rotation fairness over many simulated rounds.
- Integration tests for ingestion idempotency (ingest the same game twice, assert no double-counting).
- A mocked Lichess client for all tests; no test touches the live API.

**Observability.** Structured logging with a request/job correlation id. Error tracking (Sentry or equivalent). A `/health` endpoint reporting database connectivity and last successful job runs.

**Accessibility.** Semantic tables with proper headers for the standings and levels pages, keyboard-navigable, WCAG AA contrast. These pages are dense data tables and are the main thing users read.

---

## 12. Suggested build order

Each phase should be independently deployable and useful.

**Phase 1 — Read-only parity.**
Database schema, migrations, Lichess client, scoring module (§5), the `sync-games` / `refresh-ratings` / `recompute-aggregates` jobs with persisted outcomes, and the home / standings / levels / player profile pages. Because registration (Phase 2) and the pairing engine (Phase 4) do not exist yet, Phase 1 also ships two admin CLI commands: `seed-players` (create approved players from a username list) and `import-pairings` (create a round and its `manual_external` pairings from a CSV taken from the spreadsheet), which is what lets `sync-games` find the league's games (§7.3). A read-only `/jobs` page and `/health` make job outcomes visible before the admin panel exists. The Stats page is deferred (§8.1). With no history import, correctness is validated with hand-built fixtures and by spot-checking live players against the sheet.

**Phase 2 — Identity.** *(Built 2026-09-15.)*
Lichess OAuth (PKCE, §3.1), the registration flow and account page (§8.2), server-side sessions, roles with the bootstrap-admin rule, and the admin registration queue (§8.5). `/jobs` moves to `/admin/jobs` now that a role exists to gate it; `/health` stays public. `seed-players` remains as a dev/testing tool. Not in Phase 2: the player dashboard (Phase 3), notifications including "registration approved" (Phase 5), `validate-tokens` and token-health display (Phase 3/5), admin player management beyond the queue, the settings UI, and the GDPR deletion path (Phase 3).

**Phase 3 — Self-service.** *(Built 2026-09-15.)*
Player dashboard at `/account` (§8.3): activity toggle, the opt-in concurrent-games cap with the live in-progress count, double-game opt-out, resume-quest, my standing, my games and the stored authorisation status. UI only: no schema change, no new job, no new Lichess call; `player.max_concurrent_ceiling` becomes the first non-scoring setting read. Not in Phase 3: `validate-tokens` and revocation detection (Phase 5, with the rest of the token automation); bye / double-game history (Phase 4, with the tables); the deletion path (Phase 6, see §11).

**Phase 4 — Pairing.** *(Built 2026-09-22; see `PLAN.md`.)*
The pure pairing engine (§6.2, greedy and blossom solvers behind a `Solver` interface, chosen with `pairing.solver`), the `byes` / `double_games` / `round_exclusions` tables and the round diagnostics columns, `generate-round` on `pairing.cron`, the draft / review-window / auto-publish flow with `publish-round` and its hourly sweep, admin round management and diagnostics (§8.5), the dashboard's this-week block (§8.3), and the in-flight capacity count (§5.8). Generated rounds are published as `manual_external` pairings: players create the games by hand and §7.3 finds them, so the phase replaces the spreadsheet's `Pairing_Maker` on its own. **No shadow mode**: the maintainer decided the site pairs for real from its first run; the first rounds use `review_window` with a long window (e.g. 24 hours) so each draft is checked on `/admin/rounds` before it publishes itself. Not in Phase 4: anything that talks to Lichess (bulk pairing, challenges, `validate-tokens`, the `no_valid_token` exclusion), `missed_starts` / `evaluate-activity` / auto-pause, notifications — all Phase 5.

**Phase 5 — Automation.** *(Scope rewritten 2026-09-28; see `PLAN.md`.)*
Creating each round's games on Lichess at publication with bulk pairing, behind the `pairing.game_creation` switch (`manual` by default), in a `create-games` job queued with the publication, which reconciles before acting and gives up to hand-started games rather than failing them (§6.3); the challenge fallback for a lapsed token (§3.3); `validate-tokens` (§7), the publication probe, the re-authorise banner and the `no_valid_token` grace rule (§3.3, §5.7); `evaluate-activity` before every generation — unstarted pairings retired, missed starts for challenges, warning and auto-pause, `cancel-challenge` (§6.4); the on-site notification centre (§10); `/admin/tokens` and computed admin alerts (§8.5); a publish job for every draft, whatever generated it. Not in Phase 5: cancelling a published round (dropped: a published round is final, §6.3); custom Lichess PMs (§10) and the inactivity check-in (§8.4), both deferred.

**Phase 6 — Polish.**
Admin settings UI, editable rules content, system health page, and the account deletion path (§11) — the last is a hard gate before registration is opened publicly.

**Phase 7 — Optional.**
Historical data migration (§9), only if confirmed.

---

## 13. Decisions already made

These were open and are now settled. Recorded here so they are not relitigated during implementation.

| Question | Decision |
|---|---|
| Backend language | **Go.** Maintainer fluency; see §2.1 for the full stack. |
| Pairings per player per round | Exactly one. `max_concurrent_games` caps total ongoing games only (§5.8). |
| Default game limit | **Unlimited.** `max_concurrent_games` is `NULL` by default; players are paired every week unless they opt into a cap. |
| Odd pool | Double game if a valid volunteer exists, otherwise a bye (§6.2 step 6). |
| Double-game opt-in | **On by default**, to keep byes rare. Players may opt out. |
| Bye selection | Longest time since last bye; never rating-based. |
| Email notifications | **None.** No SMTP, no email stored. The on-site centre is the only channel (§10). |
| Base rating | **Any correspondence rating (even provisional), falling back to classical only when correspondence is entirely absent.** Corrects the spreadsheet's `MAX()`; provisional status no longer distinguishes the two sources (§5.1). |
| Unrated players | **Fixed constant** `rating.unrated_default` (1500), not the league median (§5.1). |
| Opponent rating in performance rating | **Rating at the time of the game**, stored on `Game`, not the opponent's current rating (§5.2). |
| History import | **No.** Only currently open pairings are imported, as `manual_external` (§9). Round numbers continue the sheet's sequence. |
| League-game discovery | **Pairing-anchored only** (§7.3). Members' other correspondence games are never ingested. |
| Stats page | Deferred until after Phase 4 (§8.1). |
| Templating / migrations | `html/template` and `goose`. |
| Registration form | **One checkbox** (fair-play agreement) on the join page, before the Lichess redirect; no timezone field (§8.2, §14.4). |
| `msg:write` | **Never requested.** A message is sent as the token's owner, so a player's `msg:write` could only send messages from that player. The only Lichess message is the bulk's own; custom PMs, if still wanted, from the organiser account in Phase 6 (§3.1, §10). Amended 2026-09-28 from "asked for by re-authorisation in Phase 5". |
| Existing members | Sign in through the same OAuth flow; the roster is not seeded at public launch (§8.2). |
| Bootstrap admins | `ADMIN_LICHESS_USERNAMES`, applied at sign-in; a listed newcomer joins normally and is created approved + admin (§8.2). |
| Rejected applicants | Cannot re-apply themselves; an admin can approve from the rejected tab (§8.2, §8.5). |
| Registration | Open to any Lichess account, subject to admin approval (§8.2). |
| `/jobs` | Admin-only from Phase 2 (§12). |
| Username lookalike signal | Dropped — the name comes from Lichess, not the applicant (§8.2). |
| Perf-rating deltas | The **FIDE `dp` table**, indexed by score percentage so any `k` works (§5.3). |
| Job frequency | Deliberately slow. One hourly job; everything else daily or weekly (§7). |
| Awards page | Not ported. Out of scope. |
| Deletion path (GDPR) | Built in **Phase 6**, against the final schema, as a precondition of public registration; anonymise the user, keep game rows (§11). |
| `validate-tokens` | **Phase 5**, with the other token automation; the dashboard shows stored token state until then (§8.3). |
| Dashboard URL | `/account`, grown from the Phase 2 account page. |
| Preferences before approval | Pending applicants may set them; they take effect on approval (§8.3). |
| Matching solver | **Both**, behind a `Solver` interface, chosen by the admin with `pairing.solver`: greedy by default, blossom (a port of NetworkX's minimum-cost matching) for the optimum (§6.2 step 4). Amended 2026-09-22 from "greedy, blossom only if live rounds show the need". |
| Cron parsing | `robfig/cron/v3` for `pairing.cron` (already in river's module graph); read at start-up (§7). |
| Shadow mode | **Not built.** The site pairs for real from its first run; a long review window replaces it (§12). |
| Relaxation ladder | Unreachable with a finite repeat penalty; repeats are recorded on the round and pairing instead (§6.2 step 7). |
| In-flight games for capacity | In-progress games **plus** pending pairings of published rounds, one shared query (§5.8). |
| Token gate in Phase 4 | Not applied: the manual path is the fallback path (§5.7). |
| Round numbering | `max(number) + 1` over non-cancelled rounds — the first generated round follows the last imported one (§14.2). |
| Exclusion rows | Approved members only; `removed_by_admin` added for admin-deleted pairings (§4.1.1). |
| Game creation switch | `pairing.game_creation` = **`manual`** (default) \| `lichess`, recorded on each round: the code ships before the organiser account exists, and one setting returns the league to hand-started games (§4.2, §6.3). |
| Who gets which kind of game | Both tokens working → bulk; one → a challenge from the player whose token works, in their assigned colour; neither → started by hand (§3.3). |
| Token grace | A live `no_valid_token` pool exclusion after `token.invalid_grace_days`, not a flip of `is_active`; no token row → excluded at once; only under `lichess` (§3.3, §5.7). |
| Game creation after publication | In a `create-games` job queued in the publish transaction — database first, Lichess after — that probes tokens and reconciles before acting (§6.3). |
| Lichess refusing a round's bulk | After 10 attempts (about four hours) the pairings are left to be started by hand and the players told, not marked `failed`; an admin can retry (§6.3). |
| A published round | **Final.** No cancel after publication; the review window is the undo (§6.3, §8.5). |
| `evaluate-activity` | Runs first in every generation, not after it (§6.4, §7). |
| Unstarted pairings, missed starts | Every still-pending pairing retired at the next generation; a missed start only for the player who had to accept a challenge (§6.4). |
| Inactivity check-in (§8.4) | **Deferred** until real data shows what dormancy looks like once games are created automatically. |
| Licence | **AGPL-3.0-or-later** (2026-09-28). The site is a hosted service, and only the AGPL obliges a deployment running modified code to publish it; every page's footer links to the source (§8.1). Third-party code keeps its own licence (the NetworkX port in the blossom solver is BSD-3-Clause). |

---

## 14. Open questions for the maintainers

1. ~~History import~~ — **resolved: no** (§9, §13).
2. ~~Round numbering~~ — **resolved: continue the sheet's sequence** via the imported open pairings (§9); concretely, a generated round takes `max(number) + 1` over non-cancelled rounds (§13, 2026-09-17).
3. **Organiser account** — which Lichess account holds the `challenge:bulk` token, and who has access to it? This is a single point of failure and needs a named owner. *(Needed before `pairing.game_creation` is switched to `lichess` — not for building Phase 5, which ships with game creation off, 2026-09-28.)*
4. ~~Timezone field~~ — **resolved: dropped** (§8.2, §13). Registration is a single screen; the unused `PlayerProfile.timezone` column stays nullable in case a "deadlines in local time" feature ever wants it.
5. ~~League median for unrated players~~ — **resolved: fixed constant** (§5.1, §13).
6. **Game analysis** — accuracy and centipawn loss exist only for games analysed on Lichess, and the public API cannot request analysis. Did the old Python script request it some other way, or were those columns sparsely populated in the sheet? *(Affects the Stats page, after Phase 4.)*

---

## 15. Changelog

- **2026-09-28** — Phase 5 plan amendments (see `PLAN.md`). `msg:write` is not a player scope and there are no custom Lichess PMs; `LICHESS_MSG_ENABLED` removed (§2.2, §3.1, §8.2, §10). Bulk pairing re-checked against v2.0.174: game ids at creation, no `pairAt`, the `message` sent by the organiser account, the rejection body, the double game in one bulk (§3.2, §6.3). Who gets a bulk game, a challenge or a hand-started one; the grace rule as a live `no_valid_token` exclusion (§3.3, §5.7). A challenge's id is its game's id, so challenges are re-checked by id (§3.4, §7.3). `Notification.dedupe_key`, `NotificationPreference` reduced to an opt-out row, `MissedStart.pairing_id` and uniqueness, `PlayerProfile.resumed_at`, `Round.game_creation` and `games_created_at` (§4.1, §4.1.1). `pairing.game_creation`, the `days_per_move` set, validation of the Phase 5 keys (§4.2). Publication queues `create-games`, which probes, reconciles, bulks, challenges and gives up to hand-started games; a published round is final (§6.3, §8.5). Unstarted pairings and missed starts (§6.4). Jobs table: `validate-tokens` at `pairing.cron` − 1h, `evaluate-activity` before generation, `create-games`, `cancel-challenge`, every draft gets its publish job (§7, §8.5). Dashboard: token status and banner, this-week wording per case, notifications (§8.3); inactivity check-in deferred (§8.4); `/admin/tokens` and computed admin alerts, a draft awaiting review included (§6.1, §8.5). Notifications redesigned around the on-site centre (§10). Deletion checklist extended (§11). Phase 4 and 5 paragraphs (§12), decisions (§13), §14.3 now gates switching game creation on, not building.
- **2026-09-28** — Discord dropped (maintainer): no `DISCORD_WEBHOOK_URL` (§2.2), no Discord notification channel (§10), no Discord integration in Phase 6 (§12, §13).
- **2026-09-29** — Phase 5 step 2 (see `PLAN.md`): `Notification.dedupe_key` is unique per recipient, `UNIQUE (user_id, dedupe_key)`, not across the table (§4.1, §10). The example key `round:201:paired:<pairing id>` names an event both players of the pairing are told about, which a table-wide unique key could not hold.
- **2026-09-28** — Licensed AGPL-3.0-or-later (§13); every page's footer links to the source and the licence (§8.1).
- **2026-09-22** — Phase 4 close-out. The dashboard's "This week" section recorded as built, with the cases settled while building it (§8.3). The diagnostics show the whole settings snapshot (§8.5). A manually generated draft is published by the hourly sweep, up to an hour after its window (§8.5). The pairing tests requirement no longer asks for the unreachable relaxation ladder, and names both solvers (§11). Phase 4 marked built (§12).
- **2026-09-22** — Blossom solver, selectable. `pairing.solver` added (`greedy` default | `blossom`), validated at load (§4.2). The blossom solver is a port of NetworkX's `max_weight_matching` (the 2008 `mwmatching.py` it descends from has no licence), run on `M − cost` weights in maximum-cardinality mode; each round records its solver in `settings_used`, shown on the round view (§6.2 step 4). The admin's generate, regenerate and swap actions read settings per click rather than the server's start-up copy (§4.2). Decision on the solver amended (§13), Phase 4 paragraph updated (§12).
- **2026-09-17** — Phase 4 plan amendments. `Round` gains `pool_size`, `odd_pool`, `repeat_pairings`, `settings_used`, a number unique among non-cancelled rounds and a one-draft rule; `Pairing` gains `position`, `rating_gap`, `color_penalty`, `repeat_of_round`; `RoundExclusion` gains `removed_by_admin` and a per-round uniqueness (§4.1, §4.1.1). Settings validated at load, `pairing.cron` applied on restart (§4.2). Token gate not applied before Phase 5 (§5.7). In-flight count includes pending pairings of published rounds (§5.8). Greedy solver chosen, volunteer slots paired first and coloured jointly, odd pool resolved before solving, the relaxation ladder replaced by recorded repeats (§6.2). Phase 4 publication is the state change with `pair_at` = publish time (§6.3). `publish-round-sweep` named, job uniqueness on `{round, publish_at}`, cron read at start-up (§7). Only published rounds are matched (§7.3). This-week dashboard block (§8.3); round management, diagnostics and manual generation detailed (§8.5). Deletion checklist extended (§11). Phase 4 scope rewritten, shadow mode dropped (§12). Decisions recorded (§13, §14).
- **2026-09-15** — Phase 3 amendments. Dashboard recorded as built with its scope notes (§8.3); the deletion path made a Phase 6 launch gate with its design intent and a `user_id` table checklist (§11); `validate-tokens` moved to Phase 5 and the Phase 3/5/6 paragraphs updated (§12); decisions recorded (§13).
- **2026-09-15** — Phase 2 amendments. Lichess OAuth verified (v2.0.171): PKCE `S256`, public clients only, no client secret, no refresh tokens, `POST /api/token/test` and `DELETE /api/token` recorded (§3.1); `LICHESS_CLIENT_SECRET` removed (§2.2). `OAuthToken` loses `refresh_token`, gains `issued_at`; `User` gains `lichess_profile`, `lichess_profile_fetched_at`, `fair_play_agreed_at`; `Session` entity added (§4.1). Registration reordered — agreement before the redirect, timezone dropped (§8.2, §14.4); sign-in, bootstrap-admin and rejected-applicant rules written down; the username-lookalike signal dropped (§8.2). Security mechanisms named (§11). `/jobs` moved under `/admin` (§12). Decisions recorded (§13).
- **2026-09-07** — Lichess API verified against the OpenAPI definition (v2.0.169). Corrected export paths and options (§3.4, §7.1); documented bulk-pairing atomicity, limits and the `pairAt` ambiguity (§6.3); added pairing-anchored game matching (§7.3) and `manual_external` semantics (§4.1); reshaped `Game` (status column, rating-at-game, acpl, inferred draw subtypes, aborted games excluded); decided unrated constant, rating-at-game, no history import, round numbering, Stats deferral, tooling (§13). Phase 1 scope updated (§12).
- **2026-09-13** — Recorded how a job run's outcome is decided (§7.2): database failures and total Lichess failure fail the run; partial Lichess failure succeeds but is named in the run's error text.
- **2026-09-13** — Game export no longer requests `clocks=true` (§3.4, §7.1, §7.3): per-move clock data is meaningless for correspondence games and was only bloating `raw_payload`.
- **2026-09-07** — Simplified §5.1 base rating: any correspondence rating (provisional or not) is now used ahead of classical; the earlier rule's extra branch preferring an *established* classical rating over a *provisional* correspondence one was dropped as unwarranted complexity (§5.1, §13).
