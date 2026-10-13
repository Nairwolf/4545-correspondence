package tokenhealth

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
)

func TestPlayer(t *testing.T) {
	now := time.Date(2026, 10, 12, 11, 0, 0, 0, time.UTC)
	ts := func(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
	none := pgtype.Timestamptz{}
	issued := ts(now.AddDate(0, -6, 0))
	daysAgo := func(d int) time.Time { return now.AddDate(0, 0, -d) }

	tests := []struct {
		name         string
		issued       pgtype.Timestamptz
		revoked      pgtype.Timestamptz
		expires      pgtype.Timestamptz
		wantStored   bool
		wantValid    bool
		invalidSince time.Time
		wantRevoked  bool
		// excluded under a 14-day grace period
		wantExcluded bool
	}{
		{
			name:         "no token row: excluded at once, with no grace",
			issued:       none,
			wantExcluded: true,
		},
		{
			name:       "valid, never expires",
			issued:     issued,
			wantStored: true,
			wantValid:  true,
		},
		{
			name:       "valid, expires later",
			issued:     issued,
			expires:    ts(now.AddDate(0, 1, 0)),
			wantStored: true,
			wantValid:  true,
		},
		{
			name:         "revoked three days ago: still in the grace period",
			issued:       issued,
			revoked:      ts(daysAgo(3)),
			wantStored:   true,
			invalidSince: daysAgo(3),
			wantRevoked:  true,
		},
		{
			name:         "expired three days ago: invalid since its expiry",
			issued:       issued,
			expires:      ts(daysAgo(3)),
			wantStored:   true,
			invalidSince: daysAgo(3),
		},
		{
			name:         "revoked exactly fourteen days ago: not yet more than the grace",
			issued:       issued,
			revoked:      ts(daysAgo(14)),
			wantStored:   true,
			invalidSince: daysAgo(14),
			wantRevoked:  true,
		},
		{
			name:         "revoked fourteen days and a minute ago: excluded",
			issued:       issued,
			revoked:      ts(daysAgo(14).Add(-time.Minute)),
			wantStored:   true,
			invalidSince: daysAgo(14).Add(-time.Minute),
			wantRevoked:  true,
			wantExcluded: true,
		},
		{
			name:         "expired, then found revoked: the earlier date counts",
			issued:       issued,
			expires:      ts(daysAgo(20)),
			revoked:      ts(daysAgo(2)),
			wantStored:   true,
			invalidSince: daysAgo(20),
			wantExcluded: true,
		},
		{
			name:         "revoked, with an expiry still to come",
			issued:       issued,
			expires:      ts(now.AddDate(0, 3, 0)),
			revoked:      ts(daysAgo(2)),
			wantStored:   true,
			invalidSince: daysAgo(2),
			wantRevoked:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tok := PlayerOf(tc.issued, tc.revoked, tc.expires, now)
			assert.Equal(t, tc.wantStored, tok.Stored)
			assert.Equal(t, tc.wantValid, tok.Valid())
			assert.Equal(t, tc.invalidSince, tok.InvalidSince)
			assert.Equal(t, tc.wantRevoked, tok.Revoked)
			assert.Equal(t, tc.wantExcluded, tok.Excluded(14, now))
		})
	}
}

func TestPlayer_ZeroGraceExcludesAsSoonAsInvalid(t *testing.T) {
	now := time.Date(2026, 10, 12, 11, 0, 0, 0, time.UTC)
	tok := Player{Stored: true, InvalidSince: now.Add(-time.Second)}
	assert.True(t, tok.Excluded(0, now))
	assert.Equal(t, now.Add(-time.Second), tok.GraceEnds(0))
	assert.Equal(t, now.Add(-time.Second).AddDate(0, 0, 14), tok.GraceEnds(14))
}

func TestPlayer_GraceDaysLeft(t *testing.T) {
	now := time.Date(2026, 10, 12, 11, 0, 0, 0, time.UTC)
	lapsed := func(ago time.Duration) Player { return Player{Stored: true, InvalidSince: now.Add(-ago)} }
	day := 24 * time.Hour

	assert.Equal(t, 14, lapsed(0).GraceDaysLeft(14, now), "just lapsed: the whole grace")
	assert.Equal(t, 14, lapsed(time.Hour).GraceDaysLeft(14, now), "13 days 23 hours rounds up")
	assert.Equal(t, 11, lapsed(3*day).GraceDaysLeft(14, now))
	assert.Equal(t, 1, lapsed(14*day-time.Minute).GraceDaysLeft(14, now), "a minute left is a day left")
	assert.Equal(t, 0, lapsed(14*day).GraceDaysLeft(14, now))
	assert.Equal(t, 0, lapsed(30*day).GraceDaysLeft(14, now))
	assert.Equal(t, 0, lapsed(time.Second).GraceDaysLeft(0, now), "no grace")
}
