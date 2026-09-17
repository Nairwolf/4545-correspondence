package jobs

import (
	"testing"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClient_RejectsAnUnparseableCron(t *testing.T) {
	// pairing.cron is typed by hand into the settings table until the
	// Phase 6 UI exists. A schedule river cannot parse must stop the
	// server starting, rather than leaving the league silently unpaired
	// until someone notices no round appeared on Monday. The pool is
	// nil because the parse happens before anything touches it.
	_, err := NewClient(nil, Handlers{}, "every monday please", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pairing.cron")
}

func TestPairingCronDefault_IsMondayNoon(t *testing.T) {
	// The default in internal/settings is a five-field expression, the
	// dialect ParseStandard reads; a six-field (seconds-first) one
	// would parse as something else entirely.
	schedule, err := cron.ParseStandard("0 12 * * 1")
	require.NoError(t, err)

	next := schedule.Next(mustParse(t, "2026-09-17T09:00:00Z"))
	assert.Equal(t, mustParse(t, "2026-09-21T12:00:00Z"), next, "the next firing is Monday at noon")
}

func mustParse(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return parsed
}
