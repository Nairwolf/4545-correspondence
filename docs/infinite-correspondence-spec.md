# Infinite Correspondence — Technical Specification

Replacement web application for the "Lichess4545 - Infinite Correspondence" Google Sheets system.

**Status:** specification for implementation — Phase 1 amendments applied 2026-09-07 (see §15 changelog)
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
LICHESS_CLIENT_ID
LICHESS_CLIENT_SECRET          # if using confidential client
LICHESS_REDIRECT_URI
LICHESS_ORG_TOKEN              # organiser token, scope: challenge:bulk
TOKEN_ENCRYPTION_KEY           # for encrypting stored player OAuth tokens at rest
SESSION_SECRET
ADMIN_LICHESS_USERNAMES        # comma-separated bootstrap admin list
DISCORD_WEBHOOK_URL            # optional, league-wide announcements
LICHESS_MSG_ENABLED            # optional, send notifications as Lichess PMs
```

---

## 3. Lichess integration

### 3.1 OAuth scopes

| Actor | Scope | Purpose |
|---|---|---|
| Organiser (league account) | `challenge:bulk` | Create bulk pairings |
| Each player | `challenge:write` | Allow games to be created on their behalf |
| Each player | `msg:write` *(optional)* | Send notifications as Lichess private messages (§10) |

Bulk pairing requires the organiser's `challenge:bulk` token **plus a `challenge:write` token for every player in the pairing**.

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
| `players` | `token1:token2,token3:token4,...` — colon-joined pairs, white first |
| `days` | `2` (2 days per move) |
| `rated` | `true` |
| `variant` | `standard` |
| `pairAt` | epoch ms — when games are created |
| `message` | optional message shown to players |

Notes and constraints:

- Games are created **directly**. Players do not accept a challenge; the game simply appears in their correspondence list. This removes the "failure to start a game" failure mode for consenting players.
- `clock.limit` / `clock.increment` are omitted when `days` is used.
- Pairings can be scheduled **up to a week in advance**, which fits a Monday cadence exactly.
- A created bulk pairing can be **looked up**, **cancelled before it fires**, and (for clocked games) have its clocks started early. Cancellation before `pairAt` is the mechanism behind the admin review window (§6.3).
- The organiser token must remain valid; treat its expiry as a P1 alert.

**Implementation requirement:** verify exact parameter names, rate limits and the maximum number of pairs per request against the live API documentation at `https://lichess.org/api#tag/Bulk-pairings` before writing the client. Wrap all of this in a single `LichessClient` module so the rest of the app never touches HTTP directly.

### 3.3 Game creation — fallback path

If a player has no valid `challenge:write` token (never granted, revoked, or expired), they cannot be included in a bulk pairing. Fallback order:

This path should be rare. It exists because a token can lapse for the reasons listed in §3.1, not because players are expected to do anything themselves.

1. **Exclude from bulk request, create a direct challenge instead.** `POST /api/challenge/{username}` with `days=2`, `rated=true`, `color` set explicitly. This produces a challenge the opponent must accept — the old, manual behaviour.
2. **Prompt the player to re-authorise.** A prominent banner on their dashboard and a notification, both linking to the one-click re-auth flow. The message should explain the consequence in plain terms: their games can no longer be created automatically.
3. **Flag in the admin dashboard.** Players without a valid token are listed explicitly, since each one degrades the experience of whoever they are paired against.

A player whose token has been invalid for more than `token.invalid_grace_days` (default 14) is automatically set to inactive and excluded from pairing until they re-authorise. This is not a penalty — it prevents them from repeatedly consuming a pairing slot that cannot become a real game.

### 3.4 Reading games

Two distinct sync jobs (§7):

- **Ongoing detection.** For each published pairing, determine whether the Lichess game exists and is in progress. When games are created via bulk pairing, the game IDs are returned by the API at creation time (`GET /api/bulk-pairing/{id}` returns the pairing with its `games` array of `{id, white, black}`), so ongoing games are known immediately without polling for discovery. Polling is only needed for fallback-path challenge games and for detecting completion.
- **Completion sync.** Export finished games and ingest full detail. For any pairing with a known game ID, use `POST /api/games/export/_ids` (up to 300 comma-separated IDs per call, `Accept: application/x-ndjson`) — one call re-checks every in-progress game. `GET /api/bulk-pairing/{id}/games` streams the games of a bulk pairing. For a pairing **without** a game ID (fallback challenge, or an externally created game — §7.3), search `GET /api/games/user/{username}` for the white player with `perfType=correspondence&rated=true&since=<round pair_at>` and match on opponent, colour, variant and `daysPerTurn`. Single-game export, when needed, is `GET /game/export/{gameId}` (note: **not** under `/api/`).

Export options: `opening=true&accuracy=true&clocks=true`. Do **not** request `evals=true` — it appends a per-ply analysis array that no metric uses; `acpl` and `accuracy` come from `players.{white,black}.analysis` without it.

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
  approved_by           uuid nullable fk -> User
  rejection_reason      text nullable

OAuthToken
  user_id               uuid pk fk -> User
  access_token          bytea            # encrypted at rest
  refresh_token         bytea nullable   # encrypted at rest
  scopes                text[]
  expires_at            timestamptz nullable
  revoked_at            timestamptz nullable
  last_validated_at     timestamptz

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
  number                int unique
  state                 enum(draft, published, cancelled)
  generated_at          timestamptz
  publish_at            timestamptz        # end of review window
  published_at          timestamptz nullable
  pair_at               timestamptz        # value sent as Lichess pairAt
  generated_by          enum(schedule, manual)
  bulk_pairing_id       text nullable      # Lichess bulk pairing id
  notes                 text nullable

Pairing
  id                    uuid pk
  round_id              int fk -> Round
  white_user_id         uuid fk -> User
  black_user_id         uuid fk -> User
  creation_method       enum(bulk, challenge, manual_external)
                                                    # manual_external: pairing published outside
                                                    # the system (the spreadsheet during the
                                                    # transition, or an admin fix-up); the game
                                                    # is discovered by matching (§7.3)
  lichess_game_id       text nullable unique
  status                enum(pending, created, in_progress, completed, failed, cancelled)
  match_ambiguous       boolean default false       # §7.3: more than one candidate game found;
                                                    # admin must pick
  created_at            timestamptz
  edited_by             uuid nullable fk -> User    # set if an admin altered it

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
  user_id               uuid fk -> User
  round_id              int fk -> Round
  recorded_at           timestamptz

RoundExclusion            # why an eligible-looking player got no pairing
  id                    bigserial pk
  round_id              int fk -> Round
  user_id               uuid fk -> User
  reason                enum(at_capacity, inactive, paused, auto_paused,
                             no_valid_token, bye, pending_approval)
  ongoing_games         int nullable      # populated when reason = at_capacity
  max_concurrent_games  int nullable      # populated when reason = at_capacity
  created_at            timestamptz

Notification              # on-site notification centre (§10)
  id                    bigserial pk
  user_id               uuid fk -> User
  category              text              # maps to the event table in §10
  title                 text
  body                  text
  link_url              text nullable
  read_at               timestamptz nullable
  created_at            timestamptz

NotificationPreference
  user_id               uuid fk -> User
  category              text
  on_site               boolean default true
  lichess_pm            boolean default true
  PRIMARY KEY (user_id, category)

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

**`RoundExclusion`** — one row per player who was considered for a round but received no pairing, with the reason (at capacity, inactive, paused, auto-paused, no valid token, bye, pending approval).

It exists to answer the single most common support question this system will generate: *"why didn't I get a game this week?"* Without it, that question can only be answered by re-running the pairing engine and inferring what happened — precisely the debugging experience the spreadsheet forced on maintainers. With it, the answer renders directly on the player's dashboard and in the admin round diagnostics view, and no one has to be asked.

Both tables are append-only history. Neither is read by the pairing algorithm itself; they exist for explanation and audit.

### 4.2 Configurable settings

All of these live in `Setting`, are editable from the admin panel, and have the defaults below (taken from the current spreadsheet's behaviour).

| Key | Default | Meaning |
|---|---|---|
| `pairing.cron` | `0 12 * * 1` | When generation runs (Monday) |
| `pairing.mode` | `review_window` | `review_window` \| `auto_publish` |
| `pairing.review_window_hours` | `6` | Auto-publish delay if no admin action |
| `pairing.last_k` | `5` | Rolling window size for performance rating |
| `pairing.avoid_recent_rounds` | `5` | Do not repeat opponents from the last N rounds |
| `pairing.color_weight` | `100` | Rating points one unit of colour imbalance is worth (§6.2) |
| `pairing.repeat_penalty` | `1000000` | Large finite cost of a repeat opponent (§6.2) |
| `pairing.days_per_move` | `2` | Correspondence time control |
| `pairing.rated` | `true` | Rated games |
| `pairing.min_games_for_perf` | `5` | Below this, fall back to rating |
| `activity.threshold_weeks` | `3` | Inactivity cutoff (see §8.4) |
| `activity.missed_starts_to_pause` | `2` | Consecutive misses before auto-pause |
| `player.default_max_concurrent` | `null` | Default cap for new players. `null` = unlimited |
| `player.max_concurrent_ceiling` | `20` | Highest value a player may set, if they set one at all |
| `pairing.odd_pool_strategy` | `double_then_bye` | `double_then_bye` \| `bye_only` |
| `player.default_accepts_double` | `true` | New players absorb odd pools by default, minimising byes |
| `xp.win` / `xp.draw` / `xp.loss` | `3` / `2` / `1` | XP award per result |
| `token.invalid_grace_days` | `14` | Days before an unauthorised player is deactivated |
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

- **`review_window` (default).** The round is generated in `draft` state. Admins are notified. If no admin acts within `pairing.review_window_hours` (default 6), the round auto-publishes. An admin may publish early, edit pairings, or cancel the round.
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

5. Assign colours.
   Use the argmin already computed by colour_penalty in step 3 — the
   colour assignment that minimises leftover imbalance. In practice this
   means the player with the higher color_score (more whites so far)
   takes black.

   When the two assignments tie (colour_penalty is equal either way),
   break the tie deterministically: lower-rated player takes white, then
   by user id. Never break ties by anything time- or order-dependent, so
   that regenerating a round produces an identical result.

6. Handle the odd pool.
   The pool is odd roughly half the time, because capacity skips (§5.8)
   remove an arbitrary number of players each week. Resolve it in this
   order, and record the outcome on the Round:

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

7. Relax constraints if unsolvable.
   If no perfect matching exists (typically because repeat_cost = INFINITY
   blocks a small pool), relax in this order and record which relaxation
   was applied on the Round:
     a. reduce avoid_recent_rounds by 1, repeat until 0
     b. drop colour preference to a soft weight of 0
   Never relax the "not the same player" or eligibility constraints.
```

The engine must be **deterministic**: same inputs, same output. Seed any tie-breaks from stable identifiers, never from wall-clock time or map iteration order. This makes the review window meaningful (an admin regenerating sees the same result) and makes the engine testable.

### 6.3 Publication

On publish:

1. Partition pairings by whether both players have valid `challenge:write` tokens.
2. For the bulk-capable set, issue `POST /api/bulk-pairing` with `days=2`, `rated=true`, `pairAt` set to the publish time. Store the returned bulk pairing id on the `Round` and the returned game ids on each `Pairing`; set `Pairing.status = created`.
3. For the remainder, issue individual challenges via `POST /api/challenge/{username}`, set `creation_method = challenge`, `status = pending`.
4. **A bulk pairing is all-or-nothing on the Lichess side.** The whole request is rejected (HTTP 400 with an `error` message) if any token is missing, invalid, lacks `challenge:write`, or belongs to a closed account, or if the organiser already has 20 scheduled bulks / 1000 scheduled games. Partial bulks are never created; failed bulks do not count against the rate limit. Publication must therefore: (a) rely on `validate-tokens` (§7) having run just beforehand; (b) on a 400, parse the error, move the offending pairing(s) to the challenge fallback, and resubmit the rest; (c) give up after a bounded number of resubmissions and mark the remaining pairings `failed`. Individual challenge failures (step 3) are independent and never roll back anything.
5. Notify players (§10).

Verified limits: `days` must be one of 1, 2, 3, 5, 7, 10, 14; at most 500 games per bulk and 500 games per 10 minutes; a custom `message` must contain the `{game}` placeholder. Correspondence bulks may include the same player in more than one game (this is what makes the double game in §6.2 a single request) — confirm live before relying on it. **The `pairAt` horizon is documented inconsistently** (endpoint text says "up to 24h in advance", the field says "up to 7 days"); test it with a throwaway bulk before Phase 5 assumes a Monday-generate / later-publish gap of more than a day.

If the round is cancelled during the review window and a bulk pairing was already scheduled with a future `pairAt`, cancel it via the bulk pairing cancel endpoint.

### 6.4 Missed starts

With bulk pairing, games are created outright and a "missed start" is effectively impossible. The rule still applies to the fallback challenge path:

- If a `Pairing` with `creation_method = challenge` is still `pending` (unaccepted) when the next round generates, record a `MissedStart` for the player who failed to accept.
- After `activity.missed_starts_to_pause` (default 2) **consecutive** missed starts, set `auto_paused_at` and stop pairing the player. Notify them with a one-click resume link.
- Any accepted challenge resets the consecutive counter.

This reproduces the current rule: *"Failure to start a game 2 weeks in a row will result in your quest being paused and you will receive no new pairings until you resume quest."*

---

## 7. Background jobs

Every job must be idempotent, retryable, and record its outcome. A `JobRun` table (or the scheduler's own history, if it persists one) should capture: job name, started/finished timestamps, status, items processed, error detail. The admin dashboard reads from this — the silent-failure problem is the single most important thing this rebuild fixes.

**Design principle: this league moves slowly, and the jobs should too.** Games run at 2 days per move and pairings are issued weekly. Nothing here is time-critical, and frequent polling would buy no user-visible benefit while consuming Lichess rate limit and adding failure surface. An earlier draft of this spec ran three jobs every 15 minutes; that was over-engineering and has been cut.

| Job | Schedule | Responsibility |
|---|---|---|
| `generate-round` | Weekly (`pairing.cron`) | Run the pairing engine, create a `draft` or published `Round` |
| `publish-round` | **Scheduled once**, at the round's `publish_at`; hourly sweep as safety net | Publish a `draft` round when its review window expires |
| `sync-games` | **Hourly** | Single merged job: reconcile pairing status, match games to pairings that have no game id yet (§7.3), ingest newly finished games, update standings and levels |
| `validate-tokens` | **Weekly**, ~1h before `generate-round` | Check stored tokens, mark revoked, notify affected players — so the pairing pool is accurate when it matters |
| `refresh-ratings` | Daily | Update `RatingSnapshot` for all approved players |
| `evaluate-activity` | Weekly, after round generation | Record missed starts, apply auto-pause, flag long-inactive players |
| `recompute-aggregates` | Nightly | Full recompute of standings and stats as a self-healing backstop against incremental drift |

Reasoning behind the changes:

- **`sync-ongoing` and `sync-completed` are merged into `sync-games`.** They walk the same set of pairings and call the same endpoints; splitting them doubled the API traffic for no gain. One job, one pass.
- **Hourly, not every 15 minutes.** The worst case is that a finished game appears on the standings up to an hour late. In a league where a single game takes weeks, nobody will notice, and no decision depends on it. If results feeling "live" turns out to matter to players, this is a one-line settings change — but start slow.
- **Ongoing detection barely needs polling at all.** Bulk pairing returns the game IDs at creation time (§3.4), so games created on the primary path are known immediately, with no polling. Only fallback challenges need to be watched for acceptance, and those are meant to be rare.
- **`publish-round` is scheduled, not swept.** `river` can enqueue a job to run at a specific future time, so when a round enters `draft` the publish job is scheduled directly for its `publish_at`. The hourly sweep exists only to catch a job lost to a restart or a failed enqueue — belt and braces, not the mechanism.
- **`validate-tokens` moved from daily to weekly, timed just before pairing.** Token validity only affects one decision — who can be bulk-paired — and that decision is made once a week. Checking daily made the same API calls seven times to influence one outcome. Running it shortly before `generate-round` puts the freshest possible data where it is actually used. Tokens that fail mid-week are caught anyway, because a failed API call marks the token invalid on the spot.

Both `sync-games` and `generate-round` must also be **manually triggerable from the admin panel**, for the case where an outage means the data is stale and someone wants it fixed now rather than at the top of the hour.

### 7.1 Ingestion pipeline

For each finished game:

1. Fetch with `clocks=true`, `accuracy=true`, `opening=true` (not `evals`, see §3.4).
2. Store the complete response in `Game.raw_payload`. This is non-negotiable — it allows every derived metric to be recomputed later without re-fetching, which the spreadsheet could not do.
3. Map to the normalised `Game` columns. For a bare `draw` status, replay `moves` with a chess library to classify threefold / fifty-move / insufficient material; anything else is `draw_agreement`; an unparsable move list yields `draw_other`.
4. Upsert on `lichess_game_id`. In-progress games are stored too (`status = in_progress`, result null) so the Overview page and the `ongoing` count read from the same table; the row is updated in place when the game finishes.
5. Recompute the two players' `PlayerStanding` rows (incremental).
6. Detect level-ups and emit notifications.

### 7.2 Rate limiting and resilience

- Respect Lichess rate limits. Lichess's rule is "only make one request at a time": the client serialises all outbound calls. On HTTP 429, wait at least 60 seconds before a single retry, then let the job fail and be retried by the scheduler.
- All outbound calls go through a single client with a shared limiter, timeouts, and bounded retries with exponential backoff.
- A Lichess outage must degrade gracefully: the site keeps serving cached data, jobs retry, and admins are alerted rather than the system silently doing nothing.

### 7.3 Matching games to pairings without a game id

A pairing can lack a `lichess_game_id` in two cases: a fallback challenge (§3.3) that has not yet been accepted, and a `manual_external` pairing — in particular every pairing imported from the spreadsheet during the transition (§12, Phase 1), when the league is still being paired by the sheet and games are still being created by hand.

For each such pairing, `sync-games` fetches the **white** player's games with `GET /api/games/user/{white}?perfType=correspondence&rated=true&since=<round.pair_at − 1 day>&ongoing=true&finished=true&opening=true&accuracy=true&clocks=true` and keeps candidates where:

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
- Ongoing games: white, black, round number, link to the Lichess game, days since start. Populated at publish time from the bulk-pairing response and kept current by `sync-games`, so this reflects reality rather than being reconstructed after the fact.
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

> **Not built:** the spreadsheet's Awards page (Archbishop of Accuracy, Compensation Addict, Ace) is out of scope and is not being ported.

### 8.2 Registration and authentication

**Flow:**

1. Visitor clicks "Join the league".
2. Redirected to Lichess OAuth, requesting `challenge:write` (and optionally `msg:write`, which the player may decline).
3. On callback: create `User` with `status = pending`, store the encrypted token, capture Lichess profile data (username, ratings, account creation date, whether flagged/closed).
4. Visitor completes a short form: timezone and explicit agreement to the fair-play rules. Nothing else is required — no email, and no game limit, since unlimited is the default (§5.8).
5. Confirmation screen explaining that an admin will review the application, and what happens next.
6. Admin approves or rejects (§8.5). On approval the player becomes eligible for the next round and receives a notification.

**Why OAuth rather than a username field:** it proves the applicant controls the account, and it collects the `challenge:write` token needed for automated game creation in the same step. A plain username form would let anyone register as anyone.

**Signals to surface to the reviewing admin:** account age, number of rated games, whether the account is marked TOS-violating or closed, current correspondence and classical ratings, whether the username resembles an existing member's.

**Sessions:** server-side sessions with httpOnly, secure, SameSite cookies. The Lichess token is used only for API calls, never as a session credential.

### 8.3 Player dashboard (auth required)

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
- **My standing.** Rating, record, last-5 performance, level and XP progress.
- **Lichess authorisation status.** Green if valid; if revoked or expired, a prominent re-authorise button explaining that games cannot be created automatically without it.
- **Resume quest.** Visible only when auto-paused, clearing `auto_paused_at`.

Every change writes to `AuditLog`.

### 8.4 Activity suggestions

The old `ACTIVITY_THRESHOLD` heuristic is retained, but demoted from a hard rule to a nudge:

- If a player has not finished a game in `activity.threshold_weeks * 7` days and has no ongoing games, the system notifies them (on-site, plus Lichess PM if enabled) asking whether they want to stay active, with one-click "stay active" / "pause me" links.
- No response after a further 7 days sets `is_active = false` automatically, with a notification.

This replaces the admin manually flipping the `active` column.

### 8.5 Admin panel (role = admin)

- **Registration queue.** Pending applications with the Lichess signals from §8.2, approve/reject with reason, bulk actions.
- **Player management.** Search, view, edit any profile; pause/unpause; adjust `max_concurrent_games`; grant/revoke admin; ban.
- **Round management.** List rounds with state; view the current draft with the full pairing table; edit a pairing (swap opponents, flip colours, remove a pairing); publish now; cancel; regenerate. Every edit is attributed in `AuditLog` and flagged on the pairing.
- **Pairing diagnostics.** For a draft round: which constraints were relaxed, per-pair rating gap, colour balance impact, and any player excluded from the pool with the reason. This makes an unexpected pairing explainable instead of mysterious.
- **Manual generation.** Trigger `generate-round` off-cycle.
- **Settings.** Edit everything in §4.2 from a form, with validation and an audit trail.
- **Content.** Edit the rules/FAQ text.
- **System health.** Last run and outcome of every job, recent failures with stack traces, Lichess API error rate, count of players with invalid tokens, organiser token expiry. This page is the direct answer to the original reliability problem.

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

Three channels replace it:

1. **On-site notification centre** (primary). A persisted `Notification` record per player, surfaced as a bell icon with an unread count and a list on the dashboard. Always available, requires no external service, and is the fallback for everything.
2. **Lichess private message** (optional, per player). Reaches players in the place they already are — Lichess itself — which for a correspondence league is where they'll be several times a week anyway. Requires the `msg:write` scope, requested as an optional extra at registration and declinable without affecting anything else. Gated behind `LICHESS_MSG_ENABLED` and subject to the same rate limiting as all other Lichess calls. Keep these messages short and infrequent; nobody wants a chatty bot in their Lichess inbox.
3. **Discord webhook** (optional, league-wide). Announcements to the community channel, not per-player messages.

| Event | Recipient | Channel |
|---|---|---|
| Registration approved / rejected | Applicant | On-site + Lichess PM |
| New round published | Each paired player | On-site + Lichess PM; Discord summary |
| Game created (bulk) | Each paired player | On-site (direct game link) |
| Challenge pending acceptance | Player who must accept | On-site + Lichess PM, repeated after 48h |
| About to be auto-paused (1 missed start) | Player | On-site + Lichess PM |
| Auto-paused | Player | On-site + Lichess PM, with resume link |
| Inactivity check-in | Player | On-site + Lichess PM, with stay/pause links |
| Lichess token revoked | Player | On-site banner + Lichess PM, with re-auth link |
| Assigned a double game | Volunteer | On-site (explains both games) |
| Received a bye | Player | On-site (explains why, and that they're prioritised next) |
| Level up | Player | On-site (opt-out) |
| Job failure, token expiry | Admins | On-site + Discord |

Notes:

- A revoked token cannot send a Lichess PM. Token-problem notifications must therefore always also appear as a dashboard banner, which is the only channel guaranteed to work in that state.
- All player-facing categories must be individually opt-out-able in the dashboard, except account-critical ones (approval, auto-pause).
- Because there is no email, **admins must not rely on notifications reaching a dormant player.** The inactivity flow in §8.4 should assume a player may never see the check-in, and its automatic deactivation is the mechanism that matters, not the message.

---

## 11. Non-functional requirements

**Reliability.** Every background job is idempotent and retryable. Job outcomes are persisted and surfaced in the admin health page. Failures alert admins actively rather than waiting to be noticed.

**Security.**
- Player OAuth tokens encrypted at rest with a key held outside the database; never logged, never returned by any API response, never rendered in any template.
- Admin routes behind role checks enforced server-side on every request, not merely hidden in the UI.
- CSRF protection on all state-changing requests; rate limiting on registration and auth endpoints.
- Token revocation on account deletion, and a documented deletion path (GDPR — a European user base is likely given the existing roster). The data footprint is deliberately small: a Lichess username, an encrypted token, and game history. No email addresses are held.

**Performance.** Public pages read from materialised `PlayerStanding` and cached aggregate tables, never computing rolling performance ratings per request. Standings and stats pages should render in well under a second at 200+ players and 10,000+ games.

**Testing.**
- Unit tests for the entire scoring module (§5) with fixtures drawn from real spreadsheet values.
- A dedicated test asserting that a player with `max_concurrent_games = NULL` is paired every round regardless of ongoing game count, and is never excluded for capacity. This is the default state of every player, so a regression here breaks the entire league silently (§5.8).
- Unit tests for the pairing engine covering: colour balancing, repeat avoidance, capacity caps, the constraint-relaxation ladder, and specifically the odd-pool path — double-game selection with and without valid volunteers, the two-distinct-opponents rule, the one-white-one-black rule, cap enforcement at `ongoing + 2`, and bye rotation fairness over many simulated rounds.
- Integration tests for ingestion idempotency (ingest the same game twice, assert no double-counting).
- A mocked Lichess client for all tests; no test touches the live API.

**Observability.** Structured logging with a request/job correlation id. Error tracking (Sentry or equivalent). A `/health` endpoint reporting database connectivity and last successful job runs.

**Accessibility.** Semantic tables with proper headers for the standings and levels pages, keyboard-navigable, WCAG AA contrast. These pages are dense data tables and are the main thing users read.

---

## 12. Suggested build order

Each phase should be independently deployable and useful.

**Phase 1 — Read-only parity.**
Database schema, migrations, Lichess client, scoring module (§5), the `sync-games` / `refresh-ratings` / `recompute-aggregates` jobs with persisted outcomes, and the home / standings / levels / player profile pages. Because registration (Phase 2) and the pairing engine (Phase 4) do not exist yet, Phase 1 also ships two admin CLI commands: `seed-players` (create approved players from a username list) and `import-pairings` (create a round and its `manual_external` pairings from a CSV taken from the spreadsheet), which is what lets `sync-games` find the league's games (§7.3). A read-only `/jobs` page and `/health` make job outcomes visible before the admin panel exists. The Stats page is deferred (§8.1). With no history import, correctness is validated with hand-built fixtures and by spot-checking live players against the sheet.

**Phase 2 — Identity.**
Lichess OAuth, registration flow, admin registration queue, sessions, roles.

**Phase 3 — Self-service.**
Player dashboard: activity toggle, concurrent-games cap, token status, my games.

**Phase 4 — Pairing.**
Pairing engine, round model, scheduled generation, draft/review window, admin round management and diagnostics. Run it in shadow mode for one or two weeks — generate rounds without publishing, and compare against what the spreadsheet would have produced.

**Phase 5 — Automation.**
Bulk pairing game creation, challenge fallback, ongoing sync, missed-start tracking and auto-pause, notifications.

**Phase 6 — Polish.**
Admin settings UI, editable rules content, system health page, Discord integration.

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
| Email notifications | **None.** No SMTP, no email stored. On-site centre, optional Lichess PM, optional Discord (§10). |
| Base rating | **Any correspondence rating (even provisional), falling back to classical only when correspondence is entirely absent.** Corrects the spreadsheet's `MAX()`; provisional status no longer distinguishes the two sources (§5.1). |
| Unrated players | **Fixed constant** `rating.unrated_default` (1500), not the league median (§5.1). |
| Opponent rating in performance rating | **Rating at the time of the game**, stored on `Game`, not the opponent's current rating (§5.2). |
| History import | **No.** Only currently open pairings are imported, as `manual_external` (§9). Round numbers continue the sheet's sequence. |
| League-game discovery | **Pairing-anchored only** (§7.3). Members' other correspondence games are never ingested. |
| Stats page | Deferred until after Phase 4 (§8.1). |
| Templating / migrations | `html/template` and `goose`. |
| Perf-rating deltas | The **FIDE `dp` table**, indexed by score percentage so any `k` works (§5.3). |
| Job frequency | Deliberately slow. One hourly job; everything else daily or weekly (§7). |
| Awards page | Not ported. Out of scope. |

---

## 14. Open questions for the maintainers

1. ~~History import~~ — **resolved: no** (§9, §13).
2. ~~Round numbering~~ — **resolved: continue the sheet's sequence** via the imported open pairings (§9).
3. **Organiser account** — which Lichess account holds the `challenge:bulk` token, and who has access to it? This is a single point of failure and needs a named owner. *(Needed before Phase 5.)*
4. **Timezone field** — registration currently collects a timezone, but nothing in this spec uses it. Either find a use (displaying deadlines in local time) or drop it and make registration a single click. *(Needed before Phase 2.)*
5. ~~League median for unrated players~~ — **resolved: fixed constant** (§5.1, §13).
6. **Game analysis** — accuracy and centipawn loss exist only for games analysed on Lichess, and the public API cannot request analysis. Did the old Python script request it some other way, or were those columns sparsely populated in the sheet? *(Affects the Stats page, after Phase 4.)*

---

## 15. Changelog

- **2026-09-07** — Lichess API verified against the OpenAPI definition (v2.0.169). Corrected export paths and options (§3.4, §7.1); documented bulk-pairing atomicity, limits and the `pairAt` ambiguity (§6.3); added pairing-anchored game matching (§7.3) and `manual_external` semantics (§4.1); reshaped `Game` (status column, rating-at-game, acpl, inferred draw subtypes, aborted games excluded); decided unrated constant, rating-at-game, no history import, round numbering, Stats deferral, tooling (§13). Phase 1 scope updated (§12).
- **2026-09-07** — Simplified §5.1 base rating: any correspondence rating (provisional or not) is now used ahead of classical; the earlier rule's extra branch preferring an *established* classical rating over a *provisional* correspondence one was dropped as unwarranted complexity (§5.1, §13).
