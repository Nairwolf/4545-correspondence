package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/settings"
	"github.com/nairwolf/4545-correspondence/internal/tokenhealth"
)

// jobNames are the background jobs /health reports on, in display
// order. A generate-round run that failed because a draft was already
// waiting shows up here too: that is the point of recording it as a
// failure rather than a silent no-op (spec §6.1).
var jobNames = []string{
	"sync-games", "refresh-ratings", "recompute-aggregates",
	"validate-tokens", "generate-round", "publish-round", "publish-round-sweep",
	"create-games",
}

type healthJob struct {
	LastStatus  string  `json:"last_status"` // "succeeded" | "failed" | "running" | "never_run"
	LastError   *string `json:"last_error"`
	LastRunAt   string  `json:"last_run_at,omitempty"`
	LastSuccess string  `json:"last_success,omitempty"`
}

type healthResponse struct {
	DB   string               `json:"db"`
	Jobs map[string]healthJob `json:"jobs"`
	// Settings is "ok", or "invalid" when a row fails validation: every
	// job that loads them would fail too.
	Settings string `json:"settings"`
	// GameCreation is pairing.game_creation, which decides whether the
	// organiser token matters.
	GameCreation   string          `json:"game_creation,omitempty"`
	OrganiserToken organiserHealth `json:"organiser_token"`
}

// organiserHealth is the organiser token as the latest validate-tokens
// run that reached Lichess found it (spec §8.5).
type organiserHealth struct {
	// State is "valid", "expired", "invalid", "missing_scope",
	// "not_set", "unknown" before any check, or "unreadable" when the
	// last check's record could not be read.
	State     string `json:"state"`
	Account   string `json:"account,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
}

// handleHealth is the Phase 1 "outcome visible somewhere" endpoint
// (spec §8.1): 200 only when the database is reachable, the settings
// load and no job's most recent run failed; 503 otherwise, so an uptime
// check catches a stuck pipeline, not just a dead process. When the
// site creates games it is also 503 while the organiser token cannot
// create them: the P1 alert of spec §3.2. Under manual nothing uses the
// token, and its state is reported only.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resp := healthResponse{DB: "ok", Jobs: map[string]healthJob{}, Settings: "ok"}
	healthy := true

	if err := s.pool.Ping(ctx); err != nil {
		resp.DB = "unavailable"
		healthy = false
	}

	for _, name := range jobNames {
		hj := healthJob{LastStatus: "never_run"}

		last, err := s.q.GetLatestJobRunByName(ctx, name)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// never run — not a failure on a fresh deploy
		case err != nil:
			resp.DB = "unavailable"
			healthy = false
		default:
			hj.LastStatus = string(last.Status)
			hj.LastError = last.Error
			if last.StartedAt.Valid {
				hj.LastRunAt = last.StartedAt.Time.UTC().Format("2006-01-02T15:04:05Z")
			}
			if last.Status == gen.JobRunStatusFailed {
				healthy = false
			}
		}

		if ok, err := s.q.GetLatestSuccessfulJobRunByName(ctx, name); err == nil && ok.FinishedAt.Valid {
			hj.LastSuccess = ok.FinishedAt.Time.UTC().Format("2006-01-02T15:04:05Z")
		}

		resp.Jobs[name] = hj
	}

	cfg, err := settings.Load(ctx, s.q)
	if err != nil {
		resp.Settings = "invalid"
		healthy = false
	} else {
		resp.GameCreation = string(cfg.GameCreation)
	}

	now := time.Now()
	check, err := s.organiserToken(ctx)
	resp.OrganiserToken = organiserHealthOf(check, now)
	if err != nil {
		resp.OrganiserToken.State = "unreadable"
		healthy = false
	}
	organiserBroken := check.Organiser != nil && !check.Organiser.Usable(now)
	if cfg.GameCreation == settings.GameCreationLichess && organiserBroken {
		healthy = false
	}

	w.Header().Set("Content-Type", "application/json")
	if !healthy {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func organiserHealthOf(check organiserCheck, now time.Time) organiserHealth {
	o := check.Organiser
	if o == nil {
		return organiserHealth{State: "unknown"}
	}
	h := organiserHealth{
		State:     string(o.State),
		Account:   o.Account,
		CheckedAt: check.CheckedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if o.Expires != nil {
		h.ExpiresAt = o.Expires.UTC().Format("2006-01-02T15:04:05Z")
		if o.State == tokenhealth.OrganiserValid && !o.Usable(now) {
			h.State = "expired"
		}
	}
	return h
}

type jobsData struct {
	base
	Runs      []jobRunView
	Ambiguous []gen.ListAmbiguousPairingsRow
}

type jobRunView struct {
	gen.JobRun
	Detail string // pretty-printed one-line detail JSON
}

// handleJobs is the read-only run log (spec §8.1) — the Phase 1
// stand-in for the admin health page: the last 50 job runs and any
// pairings sync-games flagged as ambiguous.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	runs, err := s.q.ListRecentJobRuns(ctx, 50)
	if err != nil {
		serverError(w, err)
		return
	}
	ambiguous, err := s.q.ListAmbiguousPairings(ctx)
	if err != nil {
		serverError(w, err)
		return
	}

	views := make([]jobRunView, len(runs))
	for i, run := range runs {
		views[i] = jobRunView{JobRun: run, Detail: compactJSON(run.Detail)}
	}

	s.render(w, "jobs", jobsData{
		base:      s.adminPage(r, "Jobs", "jobs"),
		Runs:      views,
		Ambiguous: ambiguous,
	})
}

func compactJSON(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(out)
}
