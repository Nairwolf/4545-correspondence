package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
)

// jobNames are the background jobs /health reports on, in display order.
var jobNames = []string{"sync-games", "refresh-ratings", "recompute-aggregates"}

type healthJob struct {
	LastStatus  string  `json:"last_status"` // "succeeded" | "failed" | "running" | "never_run"
	LastError   *string `json:"last_error"`
	LastRunAt   string  `json:"last_run_at,omitempty"`
	LastSuccess string  `json:"last_success,omitempty"`
}

type healthResponse struct {
	DB   string               `json:"db"`
	Jobs map[string]healthJob `json:"jobs"`
}

// handleHealth is the Phase 1 "outcome visible somewhere" endpoint
// (spec §8.1): 200 only when the database is reachable and no job's most
// recent run failed; 503 otherwise, so an uptime check catches a stuck
// pipeline, not just a dead process.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resp := healthResponse{DB: "ok", Jobs: map[string]healthJob{}}
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

	w.Header().Set("Content-Type", "application/json")
	if !healthy {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(resp)
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
		base:      s.page(r, "Jobs", "jobs"),
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
