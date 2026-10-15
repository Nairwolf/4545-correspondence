package rounds

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
)

func TestChallengeSides(t *testing.T) {
	// Spec §3.3: exactly one working token → a challenge from that
	// player, in their colour; the other must have authorised the league
	// at least once.
	white := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	black := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	p := gen.ListPairingsToCreateRow{
		WhiteUserID:   white,
		BlackUserID:   black,
		WhiteUsername: "alice",
		BlackUsername: "bob",
	}
	tokensOf := func(working, stored []pgtype.UUID) playerTokens {
		tokens := playerTokens{working: map[pgtype.UUID]workingToken{}, stored: map[pgtype.UUID]bool{}}
		for _, u := range working {
			tokens.working[u] = workingToken{}
			tokens.stored[u] = true
		}
		for _, u := range stored {
			tokens.stored[u] = true
		}
		return tokens
	}

	for _, tc := range []struct {
		name     string
		tokens   playerTokens
		ok       bool
		from, to side
	}{
		{
			name:   "white's token works, black's lapsed",
			tokens: tokensOf([]pgtype.UUID{white}, []pgtype.UUID{black}),
			ok:     true,
			from:   side{user: white, colour: "white", opponent: "bob"},
			to:     side{user: black, colour: "black", opponent: "alice"},
		},
		{
			name:   "black's token works, white's lapsed",
			tokens: tokensOf([]pgtype.UUID{black}, []pgtype.UUID{white}),
			ok:     true,
			from:   side{user: black, colour: "black", opponent: "alice"},
			to:     side{user: white, colour: "white", opponent: "bob"},
		},
		{
			name:   "both work: the bulk's",
			tokens: tokensOf([]pgtype.UUID{white, black}, nil),
		},
		{
			name:   "neither works: by hand",
			tokens: tokensOf(nil, []pgtype.UUID{white, black}),
		},
		{
			name:   "the opponent never authorised the league: by hand",
			tokens: tokensOf([]pgtype.UUID{white}, nil),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from, to, ok := challengeSides(p, tc.tokens)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.from, from)
			assert.Equal(t, tc.to, to)
		})
	}
}
