package web

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestHumanAge(t *testing.T) {
	assert.Equal(t, "3 years", humanAge(3*365*24*time.Hour+5*24*time.Hour))
	assert.Equal(t, "1 year", humanAge(400*24*time.Hour))
	assert.Equal(t, "5 months", humanAge(160*24*time.Hour))
	assert.Equal(t, "12 days", humanAge(12*24*time.Hour+3*time.Hour))
	assert.Equal(t, "1 day", humanAge(30*time.Hour))
	assert.Equal(t, "0 days", humanAge(time.Hour))
}
