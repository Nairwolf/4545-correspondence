package tokenhealth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestOrganiser(t *testing.T) {
	now := time.Date(2026, 10, 12, 11, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	day := 24 * time.Hour

	tests := []struct {
		name       string
		organiser  Organiser
		wantUsable bool
		wantSoon   bool
	}{
		{"valid, never expires", Organiser{State: OrganiserValid}, true, false},
		{"valid for a year", Organiser{State: OrganiserValid, Expires: at(365 * day)}, true, false},
		{"valid for fifteen days", Organiser{State: OrganiserValid, Expires: at(15 * day)}, true, false},
		{"valid for fourteen days: warn", Organiser{State: OrganiserValid, Expires: at(14 * day)}, true, true},
		{"valid for an hour: warn", Organiser{State: OrganiserValid, Expires: at(time.Hour)}, true, true},
		{"expired since the check", Organiser{State: OrganiserValid, Expires: at(-time.Hour)}, false, false},
		{"not set", Organiser{State: OrganiserNotSet}, false, false},
		{"invalid", Organiser{State: OrganiserInvalid}, false, false},
		{"missing scope", Organiser{State: OrganiserMissingScope}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantUsable, tc.organiser.Usable(now))
			assert.Equal(t, tc.wantSoon, tc.organiser.ExpiresSoon(now))
		})
	}
}
