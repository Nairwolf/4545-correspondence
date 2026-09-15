package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRateLimiter(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	l := newRateLimiter(10, 3)
	l.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		assert.True(t, l.allow("a"), "burst request %d", i)
	}
	assert.False(t, l.allow("a"), "bucket empty")
	assert.True(t, l.allow("b"), "another address has its own bucket")

	now = now.Add(6 * time.Second) // 10/min refills one token per 6s
	assert.True(t, l.allow("a"))
	assert.False(t, l.allow("a"))

	now = now.Add(time.Hour)
	for i := 0; i < 3; i++ {
		assert.True(t, l.allow("a"), "full again after a long idle, capped at burst")
	}
	assert.False(t, l.allow("a"))
}

func TestRateLimiterMiddleware(t *testing.T) {
	l := newRateLimiter(10, 1)
	h := l.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	call := func(addr string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/login", nil)
		req.RemoteAddr = addr
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	assert.Equal(t, http.StatusOK, call("10.0.0.1:1111"))
	assert.Equal(t, http.StatusTooManyRequests, call("10.0.0.1:2222"), "same IP, different port")
	assert.Equal(t, http.StatusOK, call("10.0.0.2:1111"))
}
