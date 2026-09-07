package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
)

// pairingRow is one line of an import-pairings CSV.
type pairingRow struct {
	round        int
	white, black string
	gameID       string // "" if the CSV cell was blank
}

// runImportPairings implements `ic import-pairings <file> [pairAt]`
// (spec §9, §12): create a round and its manual_external pairings from
// a CSV taken from the spreadsheet during the transition — the only way
// pairings exist until the pairing engine (Phase 4). This is what lets
// sync-games find the league's games at all (spec §7.3): a game is
// ingested only if it matches a pairing.
//
// pairAt, if given, must be RFC3339; otherwise it defaults to the most
// recent Monday at 12:00 UTC, matching pairing.cron's default (spec
// §4.2: "0 12 * * 1"). Every username must already exist as a user
// (seed-players first) and is matched case-insensitively; an unresolved
// name, or a file mixing more than one round number, aborts the whole
// import before any write happens. Re-running the same file is a no-op:
// the round is reused if it already exists (by number), and each
// pairing is upserted rather than duplicated.
func runImportPairings(ctx context.Context, pool *pgxpool.Pool, path string, pairAtFlag string) error {
	rows, err := readPairingCSV(path)
	if err != nil {
		return fmt.Errorf("import-pairings: %w", err)
	}
	if len(rows) == 0 {
		return errors.New("import-pairings: no pairing rows in file")
	}

	roundNumber := rows[0].round
	for _, r := range rows[1:] {
		if r.round != roundNumber {
			return fmt.Errorf(
				"import-pairings: file mixes round %d and round %d — one round per file",
				roundNumber, r.round,
			)
		}
	}

	pairAt, err := resolvePairAt(pairAtFlag)
	if err != nil {
		return fmt.Errorf("import-pairings: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("import-pairings: begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once committed

	q := gen.New(tx)

	// Resolve every username up front, so a typo anywhere in the file
	// aborts before any write — matching seed-players' "report every bad
	// name at once" behaviour.
	userIDs := map[string]pgtype.UUID{}
	var unresolved []string
	for _, r := range rows {
		for _, username := range []string{r.white, r.black} {
			if _, ok := userIDs[strings.ToLower(username)]; ok {
				continue
			}
			user, err := q.GetUserByUsername(ctx, username)
			if errors.Is(err, pgx.ErrNoRows) {
				unresolved = append(unresolved, username)
				continue
			} else if err != nil {
				return fmt.Errorf("import-pairings: look up %s: %w", username, err)
			}
			userIDs[strings.ToLower(username)] = user.ID
		}
	}
	if len(unresolved) > 0 {
		return fmt.Errorf(
			"import-pairings: %d username(s) not found (run seed-players first): %s",
			len(unresolved), strings.Join(unresolved, ", "),
		)
	}

	round, roundCreated, err := getOrCreateRound(ctx, q, roundNumber, pairAt)
	if err != nil {
		return fmt.Errorf("import-pairings: %w", err)
	}
	if roundCreated {
		after, _ := json.Marshal(map[string]any{"number": round.Number, "pair_at": pairAt})
		if err := q.CreateAuditLogEntry(ctx, gen.CreateAuditLogEntryParams{
			Action:     "import_pairings.create_round",
			EntityType: "round",
			EntityID:   strconv.Itoa(int(round.Number)),
			After:      after,
		}); err != nil {
			return fmt.Errorf("import-pairings: audit log for round: %w", err)
		}
	}

	upserted := 0
	for _, r := range rows {
		whiteID := userIDs[strings.ToLower(r.white)]
		blackID := userIDs[strings.ToLower(r.black)]
		if whiteID == blackID {
			return fmt.Errorf("import-pairings: round %d: %s cannot be paired against themselves", roundNumber, r.white)
		}

		var gameID *string
		if r.gameID != "" {
			gameID = &r.gameID
		}

		pairing, err := q.UpsertManualPairing(ctx, gen.UpsertManualPairingParams{
			RoundID:        round.ID,
			WhiteUserID:    whiteID,
			BlackUserID:    blackID,
			CreationMethod: gen.PairingMethodManualExternal,
			LichessGameID:  gameID,
		})
		if err != nil {
			return fmt.Errorf("import-pairings: upsert pairing %s vs %s: %w", r.white, r.black, err)
		}

		after, _ := json.Marshal(map[string]any{"white": r.white, "black": r.black, "game_id": r.gameID})
		if err := q.CreateAuditLogEntry(ctx, gen.CreateAuditLogEntryParams{
			Action:     "import_pairings.upsert_pairing",
			EntityType: "pairing",
			EntityID:   pairing.ID.String(),
			After:      after,
		}); err != nil {
			return fmt.Errorf("import-pairings: audit log for pairing: %w", err)
		}
		upserted++
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("import-pairings: commit: %w", err)
	}
	slog.Info("import-pairings complete", "round", roundNumber, "pairings", upserted, "round_created", roundCreated)
	return nil
}

// getOrCreateRound returns the round for number, creating it (state
// published, generated_by imported) if it doesn't exist yet.
func getOrCreateRound(ctx context.Context, q *gen.Queries, number int, pairAt time.Time) (gen.Round, bool, error) {
	existing, err := q.GetRoundByNumber(ctx, int32(number))
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return gen.Round{}, false, fmt.Errorf("look up round %d: %w", number, err)
	}

	ts := pgtype.Timestamptz{Time: pairAt, Valid: true}
	round, err := q.CreateRound(ctx, gen.CreateRoundParams{
		Number:      int32(number),
		State:       gen.RoundStatePublished,
		PublishAt:   ts,
		PublishedAt: ts,
		PairAt:      ts,
		GeneratedBy: gen.RoundSourceImported,
	})
	if err != nil {
		return gen.Round{}, false, fmt.Errorf("create round %d: %w", number, err)
	}
	return round, true, nil
}

// resolvePairAt parses flag as RFC3339 if non-empty, otherwise defaults
// to the most recent Monday at 12:00 UTC.
func resolvePairAt(flag string) (time.Time, error) {
	if flag == "" {
		return mostRecentMonday(time.Now()), nil
	}
	t, err := time.Parse(time.RFC3339, flag)
	if err != nil {
		return time.Time{}, fmt.Errorf("--pair-at must be RFC3339 (e.g. 2026-09-07T12:00:00Z): %w", err)
	}
	return t, nil
}

func mostRecentMonday(now time.Time) time.Time {
	now = now.UTC()
	offset := (int(now.Weekday()) - int(time.Monday) + 7) % 7
	monday := now.AddDate(0, 0, -offset)
	return time.Date(monday.Year(), monday.Month(), monday.Day(), 12, 0, 0, 0, time.UTC)
}

// readPairingCSV reads columns round,white,black[,game_id] with a
// header row.
func readPairingCSV(path string) ([]pairingRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // game_id column is optional

	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("read header of %s: %w", path, err)
	}
	if len(header) < 3 || strings.ToLower(header[0]) != "round" ||
		strings.ToLower(header[1]) != "white" || strings.ToLower(header[2]) != "black" {
		return nil, fmt.Errorf("%s: expected header \"round,white,black[,game_id]\", got %v", path, header)
	}

	var rows []pairingRow
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if len(rec) < 3 {
			return nil, fmt.Errorf("%s: row %v has fewer than 3 columns", path, rec)
		}
		round, err := strconv.Atoi(strings.TrimSpace(rec[0]))
		if err != nil {
			return nil, fmt.Errorf("%s: invalid round number %q: %w", path, rec[0], err)
		}
		row := pairingRow{
			round: round,
			white: strings.TrimSpace(rec[1]),
			black: strings.TrimSpace(rec[2]),
		}
		if len(rec) > 3 {
			row.gameID = strings.TrimSpace(rec[3])
		}
		rows = append(rows, row)
	}
	return rows, nil
}
