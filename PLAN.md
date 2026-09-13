# Phase 1 implementation plan — Infinite Correspondence

## Context

`docs/infinite-correspondence-spec.md` specifies a Go replacement for the Lichess4545 Infinite Correspondence Google Sheet. Phase 1 (spec §12) is **read-only parity**: schema + migrations, a Lichess client, the `sync-games` job, the pure scoring module (§5), and the public home / standings / levels / player pages (Stats deferred) — everything needed to prove the hardest logic before anything depends on it. Pairing, OAuth, dashboards, notifications are later phases; they shape the schema here but are not built.

The repo is empty (no code, no commits). Only the spec and `CLAUDE.md` exist.

### Decisions taken during planning (answers from the maintainer)

| Question | Answer | Consequence |
|---|---|---|
| History import (§9 / §14.1) | **No import.** | No historical games, no historical `User` rows. Round numbers continue the sheet's sequence via the imported open pairings (next row). Validation against sheet values (§9.2) is replaced by hand-built fixtures and live spot checks. |
| Phase 1 data source | **Seed players + import the sheet's open pairings; discovery is pairing-anchored.** | `ic seed-players` creates approved players from a username list. `ic import-pairings` creates a round and its `manual_external` pairings from a CSV taken from the spreadsheet (round, white, black, optional game id). `sync-games` only ingests games that match a pairing (spec §7.3) — members' other correspondence games, including games against other members, are never touched. Round numbers come from the sheet, so the sequence continues (~168+). |
| Unrated players (§5.1 / §14.5) | **Fixed constant**, not league median. | Setting `rating.unrated_default` (default 1500). Scoring returns an explicit unrated result; callers substitute the setting. |
| Opponent rating in perf rating (§5.2) | **Rating at the time of the game** (`games.*_rating_at_game`). | Deliberate divergence from the sheet; standings reproducible from `games` alone. |
| Stats page | **Deferred until after Phase 4.** | Removes the draw-subtype inference and analysed-games caveats from Phase 1. `notnil/chess` is not needed yet; `termination` for a bare `draw` is stored as `draw_other` for now and reclassified from `raw_payload` when Stats is built. |
| Templating / migrations | **`html/template` + `goose`.** | Only `sqlc` and Tailwind need generation steps. |

§14.3 (organiser account) is Phase 5. §14.4 (timezone) is Phase 2. **All of the above is now written into the spec** (§3.4, §4.1, §5.1, §5.2, §6.3, §7.1–7.3, §8.1, §9, §12, §13, §14, §15 changelog).

---

## Lichess API verification (source: `lichess-org/api` OpenAPI spec v2.0.169, fetched 2026-09-06)

Verified against the real OpenAPI definitions, not the rendered docs page. **Where the spec and the API disagree, the API wins.** Items marked ⚠ were spec errors, now corrected in the spec.

### Endpoints Phase 1 uses

| Purpose | Endpoint | Notes |
|---|---|---|
| Player ratings, bulk | `POST /api/users` body `id1,id2,…` (text/plain), ≤300 IDs, no auth | Returns `perfs.correspondence.{rating,rd,prov,games}` and `perfs.classical.*`. `prov` is **absent when false**. Limit 8,000 users / 10 min. One call refreshes the whole roster. |
| Player's games | `GET /api/games/user/{username}` `Accept: application/x-ndjson` | Query: `perfType=correspondence&rated=true&opening=true&accuracy=true&ongoing=true&finished=true&since=<ms>&sort=dateAsc`. Throttled at 30 games/s authenticated. `since` is a creation-time filter — see sync design. |
| Fetch known games by ID | `POST /api/games/export/_ids` body `id1,id2,…`, ≤300 IDs, `Accept: application/x-ndjson` | Used to re-check known in-progress games. Ongoing games are delayed by 3 moves (irrelevant for finished ones). |
| Single game | ⚠ `GET /game/export/{gameId}` — **not** `/api/game/export/{gameId}` as spec §3.4 says. | Only needed for an admin "re-fetch this game" command. |

### Game JSON shape (fields we map)

`id, rated, variant, speed, perf, createdAt, lastMoveAt, status, winner?, daysPerTurn, moves, opening{eco,name,ply}?, players.{white,black}.{user{name,id}, rating, ratingDiff, provisional?, analysis{inaccuracy,mistake,blunder,acpl,accuracy?}?}`

`status` enum: `created, started, aborted, mate, resign, stalemate, timeout, draw, outoftime, cheat, noStart, unknownFinish, insufficientMaterialClaim, variantEnd`.

### Findings already absorbed into the plan and spec — no decision needed

Recorded so nobody re-derives them. Even without the Stats page, Phase 1 fills the `games` columns from the API payload, so the mapping had to be fixed once.

1. **`status: draw` does not say why.** Lichess collapses agreement, threefold, 50-move and dead position into `draw`; only `insufficientMaterialClaim` and `stalemate` are distinct. Spec §7.1 now says: classify by replaying `moves` (`github.com/notnil/chess`) when the Stats page is built. **Phase 1 stores bare draws as `draw_other`** and does not pull in the chess library; reclassification runs from `raw_payload` later.
2. **`timeout` vs `outoftime`.** `outoftime` = clock expired (→ `clock_flag`). `timeout` = player abandoned / was disconnected (rare in correspondence; map to `clock_flag` too but keep the raw status column so they can be split later). `cheat` → winner is set; map termination `unknown`, result from `winner`.
3. **Aborted / noStart games have no result.** Spec's `games.result` is NOT NULL. **Decision:** do not insert `aborted`/`noStart` games into `games` at all (they are not games). Phase 5 will record them on the `pairings` row.
4. **CPL is average, not total.** The API gives `analysis.acpl` (average centipawn loss). Spec columns `white_total_cpl` are renamed `white_acpl`. Spreadsheet values were presumably acpl too.
5. **Accuracy / acpl only exist if the game has been analysed**, and correspondence games are not analysed automatically. The public API has no "request analysis" endpoint. Columns stay nullable and are filled when present. The one open question this raises (where the sheet's accuracy data came from) is parked in spec §14.6 for the Stats phase.
6. **`evals=true` is unnecessary.** It appends a per-ply `analysis` array (large) that no metric uses; `acpl`/`accuracy` live on `players.*.analysis` without it. Drop it from §7.1. `clocks=true` was initially kept as harmless, then dropped on 2026-09-13: per-move clock data means nothing for a days-per-move game and it was ~35% of every stored payload.
7. **Rate limiting:** "Only make one request at a time"; on 429 wait ≥ 60 s. The client serialises all calls behind a single semaphore and sleeps 60 s on 429 before one retry, then fails the job (river retries later).

### Verified for later phases (recorded now so nobody re-derives them)

- `POST /api/bulk-pairing` (form-encoded): `players=tokW1:tokB1,tokW2:tokB2`, `days` ∈ {1,2,3,5,7,10,14}, `rated`, `variant`, `pairAt` (epoch ms), `message` (**must contain `{game}`** if set), `rules`. Max **500 games per bulk**, 500 games / 10 min, ≤20 scheduled bulks, ≤1000 scheduled games. Response: `{id, games:[{id,white,black}], pairAt, pairedAt|null, …}`. Cancel: `DELETE /api/bulk-pairing/{id}` (no-op once games exist). Games: `GET /api/bulk-pairing/{id}/games` (ndjson).
- ⚠ **A bulk is all-or-nothing.** Rejected entirely if any token is missing/invalid/lacks scope, or an account is closed. Spec §6.3 step 4 ("failures must not roll back the successful pairings") cannot be satisfied per-pair by the API — Phase 5 must pre-validate tokens, and on a 400 parse the error, drop the offending pairing to the challenge fallback, and resubmit.
- ⚠ **`pairAt` horizon is contradictory in the docs**: the endpoint description says "up to 24h in advance", the `pairAt` field says "up to 7 days". Spec assumes a week. Must be tested live with a throwaway bulk before Phase 5 commits to a Monday-noon → publish-later design.
- Correspondence games **may include the same player twice in one bulk** ("except in correspondence" appears twice in the rejection list) — so the double-game (§6.2.6a) fits in a single request. Verify live; the `players` field text also says "each token at most once".

---

## Tooling and versions

Go 1.27 is installed. `sqlc`, `goose`, `tailwindcss`, `psql` are **not** installed; Docker is available.

- `go tool` directives in `go.mod` for `sqlc` (v1.31) and `goose` (v3.28) — no global installs.
- Tailwind v4 **standalone binary**, downloaded by `make tailwind` into `.bin/` (git-ignored). No Node.
- Postgres via `docker compose` (`postgres:17`) for dev and integration tests.
- Pinned: `chi/v5 v5.3`, `river v0.47`, `pgx/v5 v5.10`, `testify v1.12`. (`notnil/chess` only when Stats is built.) Config is plain `os.LookupEnv` — no env-parsing library, the field count doesn't justify one.

### Commands (to be added to `CLAUDE.md` once they exist)

```
make db-up            # docker compose up -d postgres
make migrate          # go tool goose -dir internal/db/migrations postgres "$DATABASE_URL" up
make sqlc             # go tool sqlc generate
make css              # tailwind standalone: web/input.css -> internal/web/static/app.css
make test             # go test ./...              (unit only; no DB)
make test-integration # TEST_DATABASE_URL=… go test -tags integration ./...
make run              # go run ./cmd/ic serve
go test ./internal/scoring -run TestPerfDelta/…   # single test
```

---

## Module layout

```
go.mod                          module github.com/nairwolf/4545-correspondence  (confirm path)
cmd/ic/main.go                  subcommands: serve | migrate | seed-players | import-pairings | sync-games | refresh-ratings | recompute | refetch-game <id>
internal/config/                env → Config struct (§2.2); only DATABASE_URL, LISTEN_ADDR, LICHESS_TOKEN (optional, for higher throttle) are read in Phase 1
internal/db/
  migrations/                   goose SQL files, embedded
  queries/                      hand-written SQL for sqlc (standings.sql, games.sql, players.sql, settings.sql, jobs.sql)
  sqlc.yaml
  gen/                          sqlc output (committed)
  db.go                         pgxpool + goose runner + tx helper
internal/lichess/                built step 4; see its testdata/README.md for fixture provenance
  client.go                     API interface (UsersByID, UserGames, GamesByID, ExportGame) + the real
                                 Client: mutex-serialized calls, one 429 retry after a 60s wait (both
                                 injectable for tests), internal batching at Lichess's own 300-id limit
                                 for both UsersByID and GamesByID (GamesByID chains multiple batches
                                 behind one GameStream so callers never see the seam) — merged into one
                                 file rather than split client.go/http.go, small enough not to need it
  types.go                      structs mirroring the verified JSON (User/Perf, Game/GamePlayer/GameOpening)
  fake.go                       in-memory API implementation for tests — no httptest.Server needed downstream
  testdata/                     two REAL captures (GET /api/user/thibault, GET /game/export/q7ZvsdUF) plus
                                 two hand-built-from-verified-schema fixtures (ongoing correspondence game,
                                 cheat game) — this session's egress could reach /api/user/{username},
                                 /api/users and /game/export/{id} but consistently got 404 from
                                 /api/games/user/{username} despite it being correctly documented and the
                                 export/_ids endpoint on the same host being reachable enough to trip its
                                 own rate limiter; treated as a sandbox networking quirk, not a spec error
internal/scoring/               PURE — no imports beyond stdlib/math. See below.
internal/ingest/                lichess.Game → games row; termination mapping; upsert; triggers standing recompute
internal/matching/              spec §7.3: find the Lichess game for a pairing without a game id (exactly-one rule)
internal/standings/             loads a player's games + ratings from db, calls scoring, writes player_standings
internal/settings/              typed accessors over the settings table with defaults from §4.2 + rating.unrated_default
internal/jobs/
  runner.go                     wraps every river worker: writes job_runs row (start/finish/status/items/error), structured log with run id
  sync_games.go                 hourly
  refresh_ratings.go            daily
  recompute.go                  nightly
  periodic.go                   river PeriodicJob config
internal/web/
  router.go                     chi, middleware (request id, recover, logging, static)
  handlers_*.go                 home, standings, levels, player, health, jobs
  templates/ (embedded)         layout.html, standings.html, …
  static/ (embedded)            app.css (generated), htmx.min.js (vendored), app.js (tiny)
web/input.css                   tailwind source
docker-compose.yml, Makefile, .gitignore, README.md
```

Dependency direction: `web → standings/ingest/settings → db, scoring, lichess`. `scoring` imports nothing from the project.

---

## Database schema (Phase 1 migrations)

Phase 1 creates only the tables it writes, but with the column shapes from §4.1 so later phases add tables rather than alter these. `rounds` and `pairings` **are** created now: `import-pairings` writes them and every game must belong to a pairing.

```sql
-- 0001_extensions.sql
CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- gen_random_uuid()

-- 0002_users.sql
CREATE TYPE user_role   AS ENUM ('player','admin');
CREATE TYPE user_status AS ENUM ('pending','approved','rejected','banned');

CREATE TABLE users (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  lichess_username  text NOT NULL,                       -- canonical casing
  lichess_user_id   text NOT NULL UNIQUE,                -- lowercase id from Lichess
  role              user_role   NOT NULL DEFAULT 'player',
  status            user_status NOT NULL DEFAULT 'pending',
  created_at        timestamptz NOT NULL DEFAULT now(),
  approved_at       timestamptz,
  approved_by       uuid REFERENCES users(id),
  rejection_reason  text
);
CREATE UNIQUE INDEX users_username_ci ON users (lower(lichess_username));

CREATE TABLE player_profiles (
  user_id               uuid PRIMARY KEY REFERENCES users(id),
  is_active             boolean NOT NULL DEFAULT true,
  max_concurrent_games  int,                              -- NULL = UNLIMITED (§5.8). Never coerce.
  accepts_double_game   boolean NOT NULL DEFAULT true,
  paused_by_admin       boolean NOT NULL DEFAULT false,
  paused_reason         text,
  auto_paused_at        timestamptz,
  timezone              text,
  joined_at             timestamptz NOT NULL DEFAULT now(),
  CHECK (max_concurrent_games IS NULL OR max_concurrent_games > 0)
);

-- 0003_ratings.sql
CREATE TABLE rating_snapshots (
  id                      bigserial PRIMARY KEY,
  user_id                 uuid NOT NULL REFERENCES users(id),
  correspondence_rating   int,
  correspondence_prov     boolean,          -- NULL when no rating
  correspondence_games    int,
  classical_rating        int,
  classical_prov          boolean,
  classical_games         int,
  fetched_at              timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX rating_snapshots_latest ON rating_snapshots (user_id, fetched_at DESC);

-- 0004_rounds_pairings.sql
CREATE TYPE round_state      AS ENUM ('draft','published','cancelled');
CREATE TYPE round_source     AS ENUM ('schedule','manual','imported');
CREATE TYPE pairing_method   AS ENUM ('bulk','challenge','manual_external');
CREATE TYPE pairing_status   AS ENUM ('pending','created','in_progress','completed','failed','cancelled');

CREATE TABLE rounds (
  id               serial PRIMARY KEY,
  number           int NOT NULL UNIQUE,
  state            round_state NOT NULL,
  generated_at     timestamptz NOT NULL DEFAULT now(),
  publish_at       timestamptz NOT NULL,
  published_at     timestamptz,
  pair_at          timestamptz NOT NULL,        -- for imported rounds: the sheet's pairing date
  generated_by     round_source NOT NULL,
  bulk_pairing_id  text,
  notes            text
);

CREATE TABLE pairings (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  round_id         int NOT NULL REFERENCES rounds(id),
  white_user_id    uuid NOT NULL REFERENCES users(id),
  black_user_id    uuid NOT NULL REFERENCES users(id),
  creation_method  pairing_method NOT NULL,
  lichess_game_id  text UNIQUE,
  status           pairing_status NOT NULL DEFAULT 'pending',
  match_ambiguous  boolean NOT NULL DEFAULT false,
  created_at       timestamptz NOT NULL DEFAULT now(),
  edited_by        uuid REFERENCES users(id),
  CHECK (white_user_id <> black_user_id)
);
CREATE INDEX pairings_round ON pairings (round_id);
CREATE INDEX pairings_unmatched ON pairings (status) WHERE lichess_game_id IS NULL;

-- 0005_games.sql
CREATE TYPE game_status      AS ENUM ('in_progress','finished');
CREATE TYPE game_result      AS ENUM ('white_win','black_win','draw');
CREATE TYPE game_termination AS ENUM ('mate','resign','clock_flag','draw_agreement','threefold',
                                      'fifty_move','stalemate','insufficient_material','draw_other','unknown');

CREATE TABLE games (
  lichess_game_id    text PRIMARY KEY,
  pairing_id         uuid NOT NULL UNIQUE REFERENCES pairings(id),
  round_number       int NOT NULL,        -- denormalised from rounds.number
  white_user_id      uuid NOT NULL REFERENCES users(id),
  black_user_id      uuid NOT NULL REFERENCES users(id),
  status             game_status NOT NULL,
  result             game_result,         -- NULL while in_progress
  termination        game_termination,    -- NULL while in_progress
  lichess_status     text NOT NULL,       -- raw API status, kept verbatim
  days_per_turn      int,
  eco                text,
  opening_name       text,
  opening_ply        int,
  white_first_move   text,
  black_first_move   text,
  white_rating_at_game int,               -- players.white.rating
  black_rating_at_game int,
  white_accuracy     numeric(5,2),
  black_accuracy     numeric(5,2),
  white_acpl         int,                 -- API gives average CPL, not total
  black_acpl         int,
  white_moves        int,
  black_moves        int,
  started_at         timestamptz NOT NULL,      -- createdAt
  last_move_at       timestamptz NOT NULL,
  finished_at        timestamptz,               -- lastMoveAt when finished
  duration_seconds   bigint,
  raw_payload        jsonb NOT NULL,
  ingested_at        timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  CHECK (white_user_id <> black_user_id),
  CHECK (status = 'in_progress' OR (result IS NOT NULL AND termination IS NOT NULL AND finished_at IS NOT NULL))
);
CREATE INDEX games_white_finished ON games (white_user_id, finished_at DESC);
CREATE INDEX games_black_finished ON games (black_user_id, finished_at DESC);
CREATE INDEX games_status ON games (status);
CREATE INDEX games_finished_at ON games (finished_at DESC) WHERE status = 'finished';

-- 0006_standings.sql
CREATE TABLE player_standings (
  user_id               uuid PRIMARY KEY REFERENCES users(id),
  rating                int,              -- base rating (§5.1); NULL = unrated
  is_unrated            boolean NOT NULL DEFAULT false,
  games_played          int NOT NULL,
  wins                  int NOT NULL,
  draws                 int NOT NULL,
  losses                int NOT NULL,
  ongoing               int NOT NULL,
  last_k_score          numeric(4,1),     -- NULL until >= min_games_for_perf
  last_k_perf_rating    int,
  power_rating          int NOT NULL,
  color_score           int NOT NULL,
  xp                    int NOT NULL,
  level                 int NOT NULL,
  xp_to_next_level      int NOT NULL,
  last_level_up_round   int,              -- Phase 4+
  last_level_up_at      timestamptz,      -- usable now
  last_game_finished_at timestamptz,
  is_eligible           boolean NOT NULL DEFAULT false,   -- Phase 4 fills this; Phase 1 = is_active && approved
  updated_at            timestamptz NOT NULL DEFAULT now()
);

-- 0007_settings.sql
CREATE TABLE settings (
  key         text PRIMARY KEY,
  value       jsonb NOT NULL,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  updated_by  uuid REFERENCES users(id)
);
-- defaults are NOT inserted; the settings package returns §4.2 defaults for missing keys,
-- so a fresh DB behaves correctly and the admin UI (Phase 6) only stores overrides.

-- 0008_job_runs.sql
CREATE TYPE job_run_status AS ENUM ('running','succeeded','failed');
CREATE TABLE job_runs (
  id               bigserial PRIMARY KEY,
  job_name         text NOT NULL,
  river_job_id     bigint,
  started_at       timestamptz NOT NULL DEFAULT now(),
  finished_at      timestamptz,
  status           job_run_status NOT NULL DEFAULT 'running',
  items_processed  int NOT NULL DEFAULT 0,
  error            text,
  detail           jsonb                      -- per-job counters (games seen / new / finished / skipped, players polled…)
);
CREATE INDEX job_runs_name_started ON job_runs (job_name, started_at DESC);

-- 0009_audit_log.sql   (seed-players / import-pairings write here from day one)
CREATE TABLE audit_log (
  id             bigserial PRIMARY KEY,
  actor_user_id  uuid REFERENCES users(id),   -- NULL = system/CLI
  action         text NOT NULL,
  entity_type    text NOT NULL,
  entity_id      text NOT NULL,
  before         jsonb,
  after          jsonb,
  created_at     timestamptz NOT NULL DEFAULT now()
);

-- 0010_river.sql   river's own schema via its migration package (rivermigrate), run inside the same goose migration
```

Why `games` carries `status` rather than a separate ongoing-games table: the Overview page and the `ongoing` standings column need in-progress games, and one row per league game updated in place is simpler than two tables. `ongoing_games(player)` (§5.8) becomes `COUNT(*) FROM games WHERE status='in_progress' AND (white_user_id=$1 OR black_user_id=$1)` — the Phase 4 engine can use that directly. `pairings.status` mirrors it (`in_progress` / `completed`) so the round views need no join.

---

## Scoring module (`internal/scoring`) — pure, no I/O

**Implemented as built (step 2), superseding the earlier sketch:**

```go
type FinishedGame struct {              // already resolved to ONE player's perspective —
    OpponentID           UserID          // no separate "me" parameter needed anywhere below
    OpponentRatingAtGame int
    PlayedWhite          bool
    Result               Result          // Win | Draw | Loss, from that player's side
    FinishedAt           time.Time
}

type Rating struct{ Value int; Provisional bool }
func BaseRating(correspondence, classical *Rating) (value int, unrated bool)   // §5.1, corrected rule
func PerfDelta(score float64, windowSize int) float64                          // §5.3: p = score/windowSize, FIDE table interpolated on 10% steps
func LastK(games []FinishedGame, k int) []FinishedGame                         // sorts its own copy by FinishedAt desc, caps at k
func LastKScore(window []FinishedGame) float64                                  // wins + 0.5*draws
func PerfRating(games []FinishedGame, k int) (perf float64, ok bool)           // avg(OpponentRatingAtGame) + PerfDelta; ok=false on an empty window
func PowerRating(perf float64, perfOK bool, base int, gamesPlayed, minGamesForPerf int) int  // §5.4
func ColorScore(games []FinishedGame) int                                       // §5.5 (white − black), all-time not windowed
func XP(games []FinishedGame, weights XPWeights) int                            // §5.6
func Level(xp int) (level, xpToNextLevel int)                                   // floor(sqrt(xp)), (level+1)^2 − xp, integer-corrected
func CapacityAllows(maxConcurrent *int, ongoing int) bool                       // §5.8 — nil means unlimited
```

Two deliberate deviations from the original sketch: (1) `FinishedGame` carries the result already resolved to one player's side, so no function needs a `me UserID` parameter — the caller (the future `standings` package) does that resolution once per player instead of every scoring function repeating it; (2) `PowerRating` takes `perf float64, perfOK bool` instead of `*float64`, avoiding nil-vs-zero ambiguity for "no computable performance rating yet".

FIDE table stored as `[11]float64{-800,-366,...,800}` indexed by `p*10`, linearly interpolated between neighbours. `PerfDelta` panics on `windowSize <= 0` and clamps `p` to `[0,1]` rather than extrapolating past the FIDE table's own ±800 cap.

`CapacityAllows` lives here even though pairing is Phase 4, because the brief demands the NULL test exists from the start and the function is trivially pure. `Level` corrects `math.Sqrt`'s float result with integer arithmetic before trusting it — cheap, and it removes any dependence on floating-point rounding behaviour at level boundaries even though no drift was observed in practice.

**Opponent rating for §5.2 — decided: rating at game time.** `PerfRating` takes the window of `FinishedGame`s, each carrying `OpponentRatingAtGame`; no rating lookup function is injected. A game whose API payload lacks the opponent's rating (should not happen for rated games) is excluded from the average and counted in `job_runs.detail` — this is an ingest-layer concern (step 6), not something the pure `scoring` package handles.

Tests: `internal/scoring/scoring_test.go`, table-driven with `testify`, covering every §5.1 branch (established correspondence beating a higher established classical; provisional correspondence beating an established classical, since 2026-09-07 that's a plain win not a fallback; provisional correspondence with no classical at all; both provisional; classical-only, established and provisional; neither → unrated), the full FIDE table at `k=5`, interpolation at a non-table-aligned score, `k=3`/`k=10` sanity, out-of-range clamping, the `PowerRating` threshold on both sides, `XP`/`Level` boundaries, and `CapacityAllows(nil, 999) == true` plus the at/over-cap cases per spec §11.

---

## `seed-players` and `import-pairings` CLIs — built step 5, verified against the live API

Both wrap their writes in one `pgx.Tx` and validate every username up front, reporting *every* unresolvable one at once rather than stopping at the first — no partial roster/round is ever left behind on failure. Their sqlc queries (`internal/db/queries/pairings.sql`, `audit.sql`) were deferred from step 3 to here, where they're first consumed, per the step-3 scoping decision.

`ic seed-players usernames.txt` — one username per line, blank lines and `#` comments skipped. Lichess ids are the lowercased username (holds in practice; the actual match is still keyed off `UsersByID`'s own response, not the assumption). For each name already present (`GetUserByLichessUserID`), it's a no-op; for each new one: `CreateApprovedUser` → `CreatePlayerProfile` → `InsertRatingSnapshot` (via the `ratingSnapshotParams` helper shared with `refresh-ratings`, in `cmd/ic/lichess_mapping.go`) → an `audit_log` row. Verified live: seeded `thibault` and `DrNykterstein`, confirmed their real ratings landed (1942/377 correspondence for thibault, matching §refresh-ratings' earlier verification); re-run was a true no-op (`created=0`); an unresolvable username aborted with zero rows written.

`ic import-pairings round.csv` — CSV columns `round,white,black[,game_id]`, one round per file (the sheet's `Overview`/`Pairing_Maker` output), header row required. Creates or reuses the `rounds` row by number (`state=published`, `generated_by=imported`, `published_at` set equal to `pair_at` since an imported round has no review window to have happened; `pair_at` from `--pair-at` (RFC3339) or defaulting to the most recent Monday 12:00 UTC) and upserts one `manual_external` pairing per line via the `pairings_round_white_black` unique index. On conflict, `lichess_game_id` is only filled in if it was previously unset — `COALESCE(pairings.lichess_game_id, EXCLUDED.lichess_game_id)` — so a re-import can never clobber a game id `sync-games` (§7.3) has since discovered. Usernames are resolved case-insensitively against `users`; an unknown name, self-pairing, or a file mixing more than one round number aborts before any write. Every round-creation and every pairing-upsert writes an `audit_log` row, including on a no-op re-run (it's an audit trail of when the command ran, not just of what changed). Verified live end to end: created round 1 with a real pairing; re-run was a no-op (same round/pairing count); all four validation paths (mixed rounds, self-pairing, unresolved username, wrong header) rejected cleanly with zero partial writes; a second round with an explicit `game_id` and `--pair-at` both worked as designed.

## `sync-games` design (hourly, plus `ic sync-games` for manual runs) — spec §3.4 / §7.3

1. **Re-check known games**: `POST /api/games/export/_ids` (batches of 300) with every `games.status='in_progress'` id. Newly finished → ingest. One or two calls for the whole league.
2. **Match pairings without a game id** (`pairings.lichess_game_id IS NULL AND status='pending' AND NOT match_ambiguous`): for each, stream the **white** player's games `GET /api/games/user/{white}?perfType=correspondence&rated=true&since=<round.pair_at−1d>&ongoing=true&finished=true&opening=true&accuracy=true`, and keep candidates where white/black ids match the pairing **with the same colours**, `variant=standard`, `rated`, `daysPerTurn == pairing.days_per_move`, `createdAt >= pair_at−1d`, and the id is not attached to another pairing. Exactly one → attach + ingest. Zero → stay pending. Several → `match_ambiguous=true`, nothing attached, listed on `/jobs` detail. Group by white player so one player with several pending pairings costs one call.
   Games with status `aborted`/`noStart` are candidates for matching (so the pairing can be marked `failed`) but are never inserted into `games`.
3. `ingest.Upsert(game, pairing)`: map → row, `INSERT … ON CONFLICT (lichess_game_id) DO UPDATE` (payload, status, result, finished fields); update `pairings.status` (`in_progress` / `completed` / `failed`). Idempotent — re-running on the same JSON changes nothing but `updated_at`.
4. For every game that transitioned to `finished` in this run, recompute both players' `player_standings` (`standings.Recompute(userID)` = one SQL aggregate + last-k query + scoring). Level-up detection compares old/new `level` and sets `last_level_up_at` / `last_level_up_round`.
5. Write the `job_runs` row: pairings checked / matched / ambiguous / still pending, games finished, errors. A Lichess error on one call is logged and counted and the job carries on; the run reports `failed` if any database write failed or if every Lichess call failed, and a partially failed run still carries the failed calls in its `error` text (spec §7.2; as first built, the "every call failed" rule was the only rule and database errors could be swallowed — see Post-review fixes, B-2).

No per-member cursor is needed: every search is anchored on a pairing's `pair_at`.

`refresh-ratings` (daily): one `POST /api/users` per 300 members → insert `rating_snapshots` rows → recompute standings for members whose base rating changed.
`recompute-aggregates` (nightly): `standings.RecomputeAll()` — full rebuild of `player_standings` from `games` + latest snapshots.

River: one `river.Client` with three periodic jobs, `MaxAttempts: 3`, queue `default` concurrency 1 (Lichess wants one request at a time anyway). Job outcome goes to `job_runs` via the `runner` wrapper regardless of river's own retention.

## Step 6 — built and verified: `internal/settings`, `internal/ingest`, `internal/matching`, `internal/standings`, `ic sync-games`

**`internal/settings`** — typed `Settings` struct with spec §4.2 defaults, overridden by whatever rows exist in `settings`. Only the keys Phase 1 actually reads so far (`LastK`, `MinGamesForPerf`, `XPWeights`, `UnratedDefault`, `DaysPerMove`) — the pairing-engine-only keys are added when Phase 4 builds their first consumer, not speculatively now.

**`internal/ingest`** — pure mapping from `lichess.Game` to `UpsertGame`'s params, no I/O. `TerminationFor`/`ResultFor` implement the status-mapping table from §3 of this plan; `IsStorable` excludes aborted/noStart entirely (spec §4.1); a bare `draw` maps to `draw_other`. Tested against the fixtures in `internal/lichess/testdata` (five new hand-built ones added for this step — resign, flag/outoftime, stalemate, insufficient-material, aborted — documented in that directory's README alongside the existing real captures) rather than duplicating fixtures per package.

**`internal/matching`** — pure implementation of spec §7.3's exactly-one rule, taking plain `Candidate` structs (not `lichess.Game` directly) so it has zero dependency on `internal/lichess`. 11 unit tests cover every documented case: exact match, reversed colours, wrong days-per-move, before/within the pair_at−1d cutoff, ambiguous (2 candidates), a game already taken by another pairing, casual, wrong variant, and a realistic taken+fresh mix.

**`internal/standings`** — `Recompute(userID)` loads finished games + latest rating snapshot, calls `internal/scoring` for every actual number, and writes one upsert. Key design point not obvious from the sketch: the `rating` column is `NULL` when a player is genuinely unrated (`is_unrated=true`), but `power_rating` still uses `cfg.UnratedDefault` internally — the *display* value and the *pairing-relevant* value are deliberately different, matching spec §5.1's "flagged as unrated... for pairing and power-rating purposes" wording exactly. Level-up detection compares against the previous row; first-ever computation for a player is never itself a "level up". `RecomputeAll` loops every approved user for the nightly job. Verified with 3 integration tests against real Postgres (`internal/standings/standings_integration_test.go`, `-tags integration`): computed values from two finished games, idempotency across two calls, and level-up setting `last_level_up_round`/`last_level_up_at` correctly.

**`ic sync-games`** (`cmd/ic/sync_games.go`) wires all of the above together per the design above, with one correction made *during* implementation: the original design split "recheck known in-progress games" (source: the `games` table) from "match pairings without a game id" (source: `pairings`), but a pairing whose CSV already specified a `game_id` at import time has `lichess_game_id` set with **no `games` row yet** — invisible to both queries as originally scoped. Fixed by replacing the games-table-sourced recheck query with `ListPairingsToRecheck`, a pairings-table query that covers both "already ingested, still in progress" and "known game id, never fetched" in one pass. This was caught by live verification, not by writing the code carefully enough the first time — worth remembering.

**Live verification against the real API** (not just tests): seeded the two real players from the `q7ZvsdUF` draw game verified back in step 4 (`lance5500`, `tryinghard87`), imported a pairing with that real `game_id` already set, and ran `sync-games`. This caught a second real bug: `GamesByID` sent no query parameters at all, so `opening`/`accuracy` silently fell back to Lichess's defaults (off) — every accuracy/acpl/opening field came back `NULL` despite the game genuinely having that data. Fixed (now matches `UserGames`'s params) and re-verified live: `white_accuracy=90.00`, `black_accuracy=90.00`, both `acpl=26`, opening name populated, duration correct, both players' standings recomputed correctly (1 draw, xp=2, level=1). Re-running was confirmed to skip the completed pairing entirely (no Lichess call at all, not just a no-op upsert) — `pairings.status NOT IN ('completed','failed','cancelled')` in `ListPairingsToRecheck` does that filtering.

The "match pending pairings" path (`GET /api/games/user/{username}`) could not be live-verified end-to-end — this sandbox's egress reliably 404s that specific endpoint (documented back in step 4) and, separately, is rate-limited by other concurrent sandbox traffic sharing the same egress IP (`"Please only run N request(s) at a time"`, confirmed via a raw probe). Rather than rely on flaky live behaviour to exercise the error-handling path, `internal/lichess.Fake` gained a small `UserGamesErrFor map[string]error` knob (a standard test-double pattern, not scope creep) and `cmd/ic/sync_games_integration_test.go` (`-tags integration`, real Postgres) now covers, deterministically: recheck-and-finish, match-and-ingest, ambiguous (flags the pairing, attaches nothing, confirmed via `ListUnmatchedPairings` no longer returning it), and — the case live testing couldn't reach — one white player's Lichess call failing while a different pairing's call succeeds, confirming the job still reports success and the failed pairing is left untouched, per spec §7.2's graceful-degradation requirement.

---

## Step 7 — built: `internal/jobs`, `ic serve`, `ic recompute`

`serve` now starts the two long-running concerns and blocks until a
signal: the river job runner and an HTTP server. `signal.NotifyContext`
(SIGINT/SIGTERM) cancels the root context; shutdown then drains the HTTP
server and stops river under a fresh 30s-bounded context. *(Corrected
2026-09-13, REVIEW B-3: as first built this did not let a running job
finish — with no `SoftStopTimeout`, cancelling river's start context
cancelled the job immediately. River now soft-stops with a 20s grace; see
"Post-review fixes".)* Verified locally end to end: `ic serve` against the
dev Postgres logs `River client started` and `http listening`, `GET
/healthz` returns 200 (`pool.Ping`) / 404 for anything else, and a
SIGTERM produces a clean `River client stopped` with exit 0.

**`internal/jobs`** owns only river scheduling and dispatch, nothing
domain-specific: a `Handlers` struct of `func(ctx, riverJobID int64)
error` supplied by the caller, three zero-field job-arg types
(`sync-games`, `refresh-ratings`, `recompute-aggregates`), and
`NewClient` returning a `*river.Client[pgx.Tx]` with one `default` queue
at `MaxWorkers: 1` (Lichess wants one request at a time — spec §3.4),
`MaxAttempts: 3`, and three `PeriodicInterval` jobs (1h / 24h / 24h,
`RunOnStart: false`). Fixed intervals from process start are deliberate
for Phase 1: the spec §4.2 cron settings and wall-clock alignment belong
to the Phase 4 pairing scheduler, not these sync workers.

**Job bodies stayed in `cmd/ic`.** `runSyncGames` / `runRefreshRatings`
already do the full `job_runs` bookkeeping and are verified; they gained
a trailing `riverJobID *int64` (nil from the one-shot CLI subcommand,
`&job.ID` from the periodic worker) so `job_runs.river_job_id` — dead
until now — is populated when the job ran under `serve`. `serve` passes
each as a `jobs.Handler` closure over the shared pool + `LichessClient`.

**New `runRecompute` + `ic recompute` subcommand.** The nightly
`recompute-aggregates` job (`standings.RecomputeAll`, no Lichess I/O)
existed in the package but was never wired to anything; it now has the
same `job_runs` wrapper as the other two and both a periodic
registration and a manual subcommand.

**HTTP is a placeholder.** `serveMux` answers only `GET /healthz`
(`pool.Ping`). The chi router, the public pages and the real `/health` /
`/jobs` endpoints (spec §8.1) are step 8 — `serve` needs *an* HTTP
server now so a deployment has something to health-check and so step 8
is purely additive.

**Dependencies:** `go mod tidy` pulled in `tidwall/{gjson,match,pretty,
sjson}` and `robfig/cron/v3` as new `// indirect` entries — all
transitive deps of `river` that the migrate-only usage hadn't reached.
No new direct dependency; nothing outside the mandated set.

---

## Step 8 — built: `internal/web` (public pages), Tailwind build

`serve` now serves the public site (spec §8.1) instead of the step-7
liveness stub. `internal/web` is a self-contained SSR package:
`html/template` (embedded), a chi router with the standard middleware
(request id, real ip, recover, logger, 15s timeout), and embedded static
assets. Dependency direction holds: `web → db/gen, settings, scoring`.

**Routes**

| Route | What it renders |
|---|---|
| `/` | Overview: ongoing games (idle days), recent results feed (20), top 5 by power rating, active-player count, static Rules/FAQ |
| `/standings` | every approved player; `?sort=&dir=&active=&q=` handled in Go; unrated shown as "—" + badge |
| `/levels` | same feed ranked by XP; XP rules stated inline from `settings.XPWeights` |
| `/players/{username}` | header (rating, level+progress bar, W/D/L, member since), full game history, inline-SVG rating and XP sparklines |
| `/health` | JSON `{db, jobs:{<name>:{last_status,last_error,last_run_at,last_success}}}`; 503 if the DB is unreachable or any job's most recent run failed |
| `/jobs` | last 50 `job_runs` + the `match_ambiguous` pairings — the read-only stand-in for the Phase 6 admin health page |

**Design decisions**

- **Sort/filter/search is done in Go**, not SQL. `GetStandings` stays one
  readable statement; `arrangeStandings` is a pure function with a
  table-driven unit test (no DB). The league is small enough that
  fetching every row per request is a non-issue, and it keeps the "reads
  like the sheet" property the spec asks for.
- **`GetStandings` extended** to also carry the level columns so one
  query feeds both `/standings` and `/levels`.
- **New read-only queries** live in `internal/db/queries/web.sql`
  (`ListOngoingGames`, `ListRecentFinishedGames`, `CountActivePlayers`,
  `GetPlayerHeader`, `ListGamesForUser`, `ListRatingSnapshotsForUser`,
  `ListFinishedGamesForUserAsc`, `ListAmbiguousPairings`) plus
  `GetLatestSuccessfulJobRunByName` in `jobs.sql`.
- **htmx** is vendored (`static/htmx.min.js`) and used only as
  progressive enhancement (`hx-boost` on `<body>`, a tiny `app.js` that
  auto-submits the standings filter). Every page works with JS disabled —
  the filter form and column-sort links are plain `GET`s.
- **Charts** are inline SVG `<polyline>`s built by a pure `lineChart`
  helper (600×160 viewBox, y inverted, empty on <2 points); no chart
  library.
- **Settings are read once at startup** (`web.New`), not per request —
  Phase 1 has no admin UI to change them (spec §4.2 editing is Phase 6).
- **Test seam:** `newServer(pool, q, cfg)` lets the integration test
  inject a transaction-scoped `*gen.Queries` (rolled back, nothing
  committed) while `/health` still pings the real pool.

**Tailwind**

Tailwind v4 **standalone binary** (no Node), fetched by `make tailwind`
into `.bin/` (git-ignored). `make css` builds `web/input.css` →
`internal/web/static/app.css`. That generated file is now **committed**
(removed from `.gitignore`) because `//go:embed static` needs it present
at build time, so `go build` / `make run` / `make test` never depend on
the Tailwind step. Utility classes only, dark zinc palette, dense
tables — no component library.

**Dependency:** `github.com/go-chi/chi/v5 v5.2.3` — mandated by spec
§2.1 (PLAN's "v5.3" was approximate; v5.2.x is chi v5's current line).

**Verified locally end to end:** seeded the two real players from the
`q7ZvsdUF` draw, imported round 168, ran `sync-games` + `recompute`,
then hit every route — standings ordered by power rating, `?sort` /
`?active` / `?q` all correct, player page rendering the game and Level 1,
`/health` 200 with per-job status, static assets served with correct
content types. Dev DB truncated afterward.

---

## Post-review fixes (2026-09-13)

Findings from `REVIEW.md` that have been addressed, with what changed.

**B-1 — `raw_payload` now holds Lichess's bytes, not a re-encoding.**
`ingest.BuildGameParams` used to `json.Marshal` the decoded
`lichess.Game`, which only declares the fields Phase 1 maps, so
`ratingDiff`, per-player blunder/mistake/inaccuracy counts, tournament
info and anything Lichess adds later were silently dropped — and
`"accuracy": null` was written for a key Lichess never sent. Spec §7.1
calls the full response non-negotiable. Now:

- `lichess.GameStream` keeps a copy of each ndjson line (`Raw()`); the
  slice is valid until the next `Next`, so `sync-games` clones it when it
  buffers candidates. `ExportGame` reads its body once and returns the
  bytes alongside the decoded game.
- `BuildGameParams` takes the bytes as a parameter and stores them
  verbatim, rejecting anything that isn't valid JSON up front so a bad
  payload fails at the mapping rather than inside the jsonb upsert.
- `lichess.Fake` gained `RawGames map[string][]byte`: when set for an id,
  the fake emits those bytes as the stream line, exactly as the real
  client would, so a test can prove pass-through end to end.
- Tests at each layer: `GameStream.Raw` equals the line and survives
  buffer reuse across `Next`; `BuildGameParams` stores the live
  `q7ZvsdUF` fixture byte-for-byte including three fields the struct
  doesn't declare; and an integration test (`-tags integration`) runs
  `doSyncGames` with a `RawGames` entry carrying an undeclared field and
  reads it back from `games.raw_payload`.

Cost: with `clocks` no longer requested (see the API findings above),
the stored payload grows by roughly 250 bytes per game over the old
re-encoding. Games ingested before this fix keep their lossy payload;
`UpsertGame` overwrites `raw_payload` on every re-fetch, so in-progress
games self-heal on their next hourly poll, but finished games are never
re-fetched and would need a one-off pass through the still-unbuilt
`refetch-game` command if the old rows ever matter.

**B-2 — `sync-games` no longer reports success when it failed.** As
built, three things combined to hide failures: per-player Lichess errors
were swallowed and never reached the outcome; the re-check step counted
a successful Lichess call even when it made none; and a database error
was discarded whenever that (possibly phantom) success count was
non-zero. Lichess being fully down, or an upsert failing mid-run, both
produced `status=succeeded, error=NULL`. Now (`cmd/ic/sync_games.go`):

- Lichess call failures go through `stats.lichessFailed`, which counts
  them, logs them, and keeps the first ten messages in
  `detail.lichess_errors`. They are never returned as errors, so the
  run carries on.
- Every other error — a query, an upsert, a mapping failure — is
  returned from `doSyncGames` as fatal, and the run stops there.
- `lichessOK` is counted only after a call and its stream both
  complete; the re-check makes no call when no pairing has a game id.
- `syncGamesOutcome(stats, fatal)` is the single, pure place the rule in
  spec §7.2 lives: fatal → `failed`; every attempted call failed →
  `failed`; otherwise `succeeded`, with any failed calls still named in
  `error`. A failed run returns an error so river retries it.
- A `settings.Load` failure now also goes through `FinishJobRun` instead
  of leaving the row at `running` (the `sync-games` half of REVIEW S-12).
- `runSyncGames` takes `gen.DBTX` instead of `*pgxpool.Pool`, so an
  integration test can pass its transaction and read the `job_runs` row
  back.

Tests: a unit table for `syncGamesOutcome` covering each branch,
including the original bug (a database error after a successful call);
the message cap; and three integration tests — Lichess unreachable is
recorded as `failed` with the error text and `runSyncGames` returns an
error; a partial Lichess failure is recorded as `succeeded` with the
failed player named in `error`; and a real Postgres error (accuracy
overflowing `numeric(5,2)`) after a successful re-check call is fatal.
`lichess.Fake` gained `Err`, which makes every call fail.

The context the final `FinishJobRun` uses could still be cancelled by
river's job timeout, leaving the row at `running`; that is fixed under
B-3 below.

**B-3 — timed-out and interrupted runs are recorded, not left `running`.**
River v0.47 defaults `JobTimeout` to one minute, and a single 429 costs a
60s wait on its own, so real `sync-games` runs would have been cancelled
mid-flight. Every job body then wrote its outcome on that cancelled
context, the write failed, and the `job_runs` row stayed `running`
forever — `/health` never went red and `/jobs` showed a row that never
resolved. The same happened on every SIGTERM: without a `SoftStopTimeout`
river treats cancelling its start context as `StopAndCancel`, so the 30s
drain Step 7 described never applied to jobs. Now:

- `internal/jobs` sets `JobTimeout = 15 * time.Minute` (exported) — a
  429 wait plus full 300-id batches for several white players, with
  headroom — and `softStopTimeout = 20s`, under serve's 30s shutdown
  deadline because the grace runs concurrently with the HTTP drain. River
  derives its stuck-job rescue window from `JobTimeout`, so nothing else
  is configured.
- All three job bodies call `FinishJobRun(context.WithoutCancel(ctx), …)`.
  That is three identical sites with the same comment — one more argument
  for REVIEW S-11's single `runJob` wrapper, which stays a separate change.
- `runRecompute` loads settings inside the region that always ends in
  `FinishJobRun`, like `runSyncGames` already did (the `recompute` half of
  REVIEW S-12), and takes `gen.DBTX` so a test can pass its transaction.
- `runSyncGames` treats a cancelled context as fatal (`interrupted: …`).
  Not in the original plan for this fix, found by its integration test: a
  cancellation after one successful Lichess call turned every remaining
  call into a "context canceled" failure, which `syncGamesOutcome` then
  forgave as a partial Lichess failure and recorded as `succeeded` — with
  pairings left unprocessed.
- The Lichess client's 429 wait selects on `ctx.Done()` and returns
  without retrying when the context ends. `WithSleepFunc` keeps its
  signature. `Retry-After` (REVIEW N-11) and the flat HTTP client timeout
  (S-13) are untouched.
- `serve` runs a new `FailInterruptedJobRuns` query after opening the pool
  and before starting river: any row still `running` belonged to a process
  that died, and is marked `failed` with "interrupted: the server restarted
  before this run finished" (logged at Warn with the count). Known
  limitation: a one-shot `ic sync-games` that happens to be running when
  `serve` starts has its row marked failed too — operator error, and rare.
  No "running too long" rule was added to `/health`: with the above, a row
  can only stay `running` while the process is alive if the database write
  itself fails, which `/health` already reports through `pool.Ping`.

Tests: a unit test that a 429 wait of an hour returns within a second
when the context's 50ms deadline passes, with `context.DeadlineExceeded`
and no retry; and three integration tests — `runSyncGames` whose context
is cancelled at its first user-games call (after a successful re-check
call) records `failed` with "interrupted: context canceled"; the startup
sweep marks a `running` row failed with the message and `finished_at` set
while leaving a `succeeded` row alone; and a malformed `pairing.last_k`
setting makes `runRecompute` record `failed` with "load settings" instead
of leaving the row `running`.

---

## Web (public pages, no auth)

chi router; `html/template` with a `layout.html` + one file per page; htmx only for sort/filter on standings (falls back to plain query-string links, so the pages work without JS). Dark-by-default, Lichess-like: near-black background, muted borders, dense tables — Tailwind utility classes only, no component library.

| Route | Source query | Notes |
|---|---|---|
| `/` | recent finished games (20), in-progress games with days since last move, top 5 by `power_rating`, active count | Rules/FAQ text is a static template in Phase 1 (admin editing is Phase 6) |
| `/standings` | `player_standings JOIN users JOIN player_profiles` | Columns per §8.1; `?sort=col&dir=asc&active=1&q=name` server-side; unrated players show "—" with an "unrated" badge |
| `/levels` | same table ordered by xp desc | XP rule explanation inline; last level-up shows the round number (always known now) |
| `/players/{username}` | header from standings; game list from `games` | Charts: rating over time from `rating_snapshots`, XP over time derived from finished games in order. Rendered as inline SVG server-side (no chart library) — keep it boring |
| `/health` | `SELECT 1` + last `job_runs` row per job | JSON: `{db:"ok", jobs:{sync-games:{last_success, last_status, last_error}}}`; returns 503 if db down or any job's last run failed. This is the Phase 1 "outcome visible somewhere" |
| `/jobs` | last 50 `job_runs` + pairings flagged `match_ambiguous` | Plain HTML table, read-only, no secrets. Satisfies the DoD until the admin health page (Phase 6) replaces it |

All standings SQL is written to read like the spreadsheet formulas it replaces, with a comment naming the sheet cell (`-- Standings_Backend!U`) above each expression.

---

## Build order

Each step is a self-contained commit point (the maintainer commits; see `CLAUDE.md`).

1. **Scaffold**: `go.mod`, `cmd/ic` with `serve`/`migrate`, config, Makefile, docker-compose, `.gitignore`, README stub. `make db-up && make migrate` runs migrations 0001–0009 on an empty DB.
2. **Scoring package** with full table-driven tests (no DB needed). Done before anything reads it.
3. **sqlc queries + generated code** for players, games upsert, standings aggregates, settings, job_runs.
4. **Done.** Lichess client + fixtures + fake; `ic refresh-ratings` wired and run end to end against the live API — verified with one real account (thibault) manually inserted via SQL, since `seed-players` doesn't exist until step 5; confirmed the inserted `rating_snapshots` row matched the live API response exactly, then the test row was deleted.
5. **Done.** `seed-players` and `import-pairings` CLIs — see their own section above for what was built and verified.
6. **Done.** Matching + ingest + standings recompute; `ic sync-games` wired and run live. See the dedicated section above for what was built, the two real bugs live verification caught, and the deterministic integration test suite that now covers the orchestration (including the error path live testing couldn't reliably reach).
7. **Done.** River wiring — `serve` starts HTTP + river, periodic jobs registered; `internal/jobs` + `ic recompute`. See the dedicated section above.
8. **Done.** Web pages (`internal/web`, all six routes) + Tailwind build. See the dedicated section above.
9. **Done.** README documents both the `make` targets and the one-shot `ic` admin subcommands; `CLAUDE.md`'s "Repository state" section now reflects Phase 1 being built rather than the empty-repo bootstrap text, and points at README/PLAN for the rest. This closes out Phase 1's build order (spec §12) — Phase 2 onward (identity, self-service, pairing, automation, polish, optional history migration) is not started.

---

## Testing

- **`internal/scoring`** — table-driven, pure:
  - `BaseRating`: every branch of §5.1, simplified 2026-09-07 to drop provisional-vs-established distinctions entirely — any correspondence rating wins (established or provisional; provisional-over-established-classical is the case that changed), classical only when correspondence is absent, neither → unrated.
  - `PerfDelta`: all 11 FIDE points at `k=5` exactly; interpolation midpoints (e.g. 2.25/5 → −110.5); `k=3` and `k=10` sanity; clamping at 0% / 100%.
  - `PowerRating` on both sides of `min_games_for_perf`.
  - `XP`/`Level` at boundaries: xp 0,1,3,4,8,9,15,16.
  - `ColorScore` sign convention.
  - **`CapacityAllows(nil, 999) == true`** — the NULL-is-unlimited test the brief requires — plus `(4,3)=true`, `(4,4)=false`, `(4,7)=false`.
- **`internal/ingest`** — fixtures from real Lichess JSON (recorded in `lichess/testdata`): a decisive game, a resign, a flag, a bare draw (→ `draw_other`), stalemate, insufficient material, a `cheat` game, an in-progress game; first-move extraction; duration; rating-at-game captured; acpl/accuracy present vs absent; `aborted` → pairing failed, no `games` row.
- **`internal/matching`** — table-driven over fixture game lists: exactly one candidate → matched; reversed colours → not a candidate; a 3-day game → not a candidate; a game created before `pair_at−1d` → not a candidate; two candidates → `match_ambiguous`, nothing attached; a game already attached to another pairing → skipped; casual game between the same players → ignored. This is the test that proves outside-league games are never ingested.
- **`internal/lichess`** — `httptest.Server` serving fixtures: ndjson streaming, `prov` absent-means-false, 429 → sleeps (clock injected) and retries once, batching of >300 ids.
- **Integration (`-tags integration`, needs `TEST_DATABASE_URL`)** — runs goose from empty; `import-pairings` twice → one round, no duplicate pairings; ingest the same game JSON twice → exactly one row, standings unchanged on the second pass; in-progress → finished transition updates `games` and `pairings.status` and recomputes both players; `RecomputeAll` equals incremental results; `/standings` handler renders the expected order.
- **Determinism guard**: standings recompute iterates over sorted slices from SQL, never Go maps; a test asserts `RecomputeAll` output is byte-identical across two runs.
- No test touches lichess.org. `ic refresh-ratings`/`ic sync-games` against the live API are manual verification steps, not tests.

---

## Spec amendments

Applied to `docs/infinite-correspondence-spec.md` on 2026-09-07 (see its §15 changelog): §3.4, §4.1 (Pairing + Game), §4.2, §5.1, §5.2, §6.3, §7, §7.1, §7.2, new §7.3, §8.1, §9, §12, §13, §14. Two questions remain open in §14 (organiser account — Phase 5; timezone — Phase 2) plus a new one (§14.6, whether the sheet's accuracy data came from analysis requested outside the API).

---

## Verification (end of Phase 1)

1. `make db-up && make migrate` on an empty volume → all migrations apply; `make migrate-down` reverses them.
2. `make test` green; `make test-integration` green.
3. `ic seed-players players.txt` with the current active roster → users/profiles/snapshots exist; audit_log rows present.
4. `ic import-pairings round-NNN.csv` for the current open round(s) from the sheet → rounds/pairings rows; re-run → no change.
5. `ic sync-games` → `job_runs` row `succeeded`, pairings matched, `games` rows created (in progress or finished); re-run immediately → zero new matches, no row changes except `updated_at`. Confirm a member's known non-league correspondence game was **not** ingested.
6. `ic serve`; open `/standings`, `/levels`, `/players/{name}`, `/` — tables populated; sorting/filtering works with JS disabled; `/health` returns 200 with `sync-games.last_status = succeeded`; `/jobs` lists the runs and any ambiguous matches.
7. Hand-check five players' rating / last-5 score / XP against the live spreadsheet. Expect differences **only** where classical > correspondence (§5.1), where the opponent's current rating differs from their rating at game time (§5.2), or where the sheet counts games from before the imported rounds — document each.
8. Stop Postgres while `serve` is running → `/health` returns 503; restart → river resumes and the next hourly run logs a `job_runs` row.
