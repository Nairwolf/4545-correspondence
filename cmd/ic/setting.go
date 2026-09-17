package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

// runSetting writes one spec §4.2 setting, with an audit row, until the
// Phase 6 settings UI exists. The value is JSON, as stored: a number is
// 6, a string is "0 12 * * 1", a boolean is true.
//
// The write is validated by reading the whole settings table back
// inside the same transaction: a value the settings package rejects
// (out of range, wrong type, not one of an enum's values) rolls the
// write back here, rather than failing the next generate-round run at
// midday on a Monday. An unknown key is refused for the same reason —
// Load ignores keys it doesn't know, so a typo would otherwise look
// like it worked and change nothing.
func runSetting(ctx context.Context, db txBeginner, key, value string) error {
	if !settings.Known(key) {
		return fmt.Errorf("ic setting: unknown key %q", key)
	}
	if !json.Valid([]byte(value)) {
		return fmt.Errorf("ic setting: %s: value must be JSON (a string needs quotes), got %s", key, value)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ic setting: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)

	previous, err := q.GetSetting(ctx, key)
	before := previous.Value
	if err != nil {
		before = nil // no override yet: the default was in force
	}

	if _, err := q.UpsertSetting(ctx, gen.UpsertSettingParams{Key: key, Value: []byte(value)}); err != nil {
		return fmt.Errorf("ic setting: write %s: %w", key, err)
	}
	if _, err := settings.Load(ctx, q); err != nil {
		return fmt.Errorf("ic setting: %w (not written)", err)
	}
	if err := q.CreateAuditLogEntry(ctx, gen.CreateAuditLogEntryParams{
		ActorUserID: pgtype.UUID{},
		Action:      "setting.update",
		EntityType:  "setting",
		EntityID:    key,
		Before:      before,
		After:       []byte(value),
	}); err != nil {
		return fmt.Errorf("ic setting: audit: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ic setting: commit: %w", err)
	}
	fmt.Printf("%s = %s\n", key, value)
	return nil
}
