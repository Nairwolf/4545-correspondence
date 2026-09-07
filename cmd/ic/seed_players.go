package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
)

// runSeedPlayers implements `ic seed-players <file>` (spec §12 Phase 1):
// create approved players, directly, from a plain list of Lichess
// usernames — one per line, blank lines and lines starting with '#'
// ignored. There is no registration flow yet (that's Phase 2), so every
// user this creates is approved by the CLI operator's own authority.
//
// Re-running with an overlapping list is a no-op for usernames already
// seeded — only genuinely new usernames get a user row, a profile, a
// first rating snapshot, and an audit_log entry. Every write happens in
// one transaction: if any username in the file can't be resolved on
// Lichess, nothing is written, and the run reports every bad username
// at once rather than stopping at the first.
func runSeedPlayers(ctx context.Context, pool *pgxpool.Pool, client lichess.API, path string) error {
	usernames, err := readUsernameLines(path)
	if err != nil {
		return fmt.Errorf("seed-players: %w", err)
	}
	if len(usernames) == 0 {
		return errors.New("seed-players: no usernames in file")
	}

	// Lichess ids are canonically the lowercased username (confirmed
	// against the live API) — UsersByID's own response is still what
	// decides the match, this is just how the lookup ids are formed.
	ids := make([]string, len(usernames))
	for i, u := range usernames {
		ids[i] = strings.ToLower(u)
	}
	lichessUsers, err := client.UsersByID(ctx, ids)
	if err != nil {
		return fmt.Errorf("seed-players: fetch users: %w", err)
	}
	byID := make(map[string]lichess.User, len(lichessUsers))
	for _, lu := range lichessUsers {
		byID[lu.ID] = lu
	}

	var unresolved []string
	for i, username := range usernames {
		if _, ok := byID[ids[i]]; !ok {
			unresolved = append(unresolved, username)
		}
	}
	if len(unresolved) > 0 {
		return fmt.Errorf(
			"seed-players: %d username(s) not found on Lichess: %s",
			len(unresolved), strings.Join(unresolved, ", "),
		)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("seed-players: begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once committed

	q := gen.New(tx)
	created, skipped := 0, 0
	for i, username := range usernames {
		lu := byID[ids[i]]

		if _, err := q.GetUserByLichessUserID(ctx, lu.ID); err == nil {
			skipped++
			continue
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("seed-players: look up %s: %w", username, err)
		}

		user, err := q.CreateApprovedUser(ctx, gen.CreateApprovedUserParams{
			LichessUsername: lu.Username,
			LichessUserID:   lu.ID,
		})
		if err != nil {
			return fmt.Errorf("seed-players: create user %s: %w", username, err)
		}
		if _, err := q.CreatePlayerProfile(ctx, user.ID); err != nil {
			return fmt.Errorf("seed-players: create profile for %s: %w", username, err)
		}
		if _, err := q.InsertRatingSnapshot(ctx, ratingSnapshotParams(user.ID, lu)); err != nil {
			return fmt.Errorf("seed-players: insert rating snapshot for %s: %w", username, err)
		}

		after, _ := json.Marshal(map[string]string{
			"lichess_username": user.LichessUsername,
			"lichess_user_id":  user.LichessUserID,
		})
		if err := q.CreateAuditLogEntry(ctx, gen.CreateAuditLogEntryParams{
			Action:     "seed_players.create_user",
			EntityType: "user",
			EntityID:   user.ID.String(),
			After:      after,
		}); err != nil {
			return fmt.Errorf("seed-players: audit log for %s: %w", username, err)
		}
		created++
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("seed-players: commit: %w", err)
	}
	slog.Info("seed-players complete", "created", created, "already_existed", skipped)
	return nil
}

func readUsernameLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var usernames []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		usernames = append(usernames, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return usernames, nil
}
