package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/rounds"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

// Admin round management (spec §8.5): generating, reviewing, publishing
// and hand-editing the week's pairings. Generation runs in-request —
// the engine on a pool of a few hundred is milliseconds and makes no
// Lichess call — so this file needs no river client; a draft this
// creates is published either by its own scheduled job (server mode)
// or, for a CLI-generated one, the hourly sweep, at most an hour late.
//
// Every action that runs the engine reads the settings inside its own
// transaction instead of using the server's start-up copy, so a pairing
// setting changed with `ic setting` — pairing.solver, the weights —
// applies from the next click, the way the scheduled job already does,
// rather than from the next restart.

// roundRow is one row of the round list.
type roundRow struct {
	ID        int32
	Number    int32
	State     gen.RoundState
	Generated pgtype.Timestamptz
	PublishAt pgtype.Timestamptz
	Source    gen.RoundSource
	Pairings  int32
	Byes      int32
}

type roundsListData struct {
	base
	Rows  []roundRow
	Error string
}

func (s *Server) handleAdminRounds(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stored, err := s.q.ListRounds(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	rows := make([]roundRow, 0, len(stored))
	for _, round := range stored {
		rows = append(rows, roundRow{
			ID:        round.ID,
			Number:    round.Number,
			State:     round.State,
			Generated: round.GeneratedAt,
			PublishAt: round.PublishAt,
			Source:    round.GeneratedBy,
			Pairings:  round.PairingCount,
			Byes:      round.ByeCount,
		})
	}
	s.render(w, "admin_rounds", roundsListData{
		base:  s.page(r, "Rounds", "rounds"),
		Rows:  rows,
		Error: roundsListError(r.URL.Query().Get("error")),
	})
}

func roundsListError(code string) string {
	if code == "draft-exists" {
		return "A draft is already waiting for review — publish, cancel or regenerate it before generating another."
	}
	return ""
}

// handleGenerateRound is "Generate now": rounds.Generate(source=manual)
// run synchronously, recording a job_runs row exactly as the scheduled
// job would, so /admin/jobs and /health show it the same way.
func (s *Server) handleGenerateRound(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin, _ := currentUser(r)

	jobRun, err := s.q.CreateJobRun(ctx, gen.CreateJobRunParams{JobName: "generate-round"})
	if err != nil {
		serverError(w, err)
		return
	}

	var outcome rounds.Outcome
	genErr := s.roundsTx(ctx, func(tx pgx.Tx) error {
		cfg, err := settings.Load(ctx, gen.New(tx))
		if err != nil {
			return err
		}
		outcome, err = rounds.Generate(ctx, tx, cfg, time.Now(), gen.RoundSourceManual, admin.ID, nil)
		return err
	})

	detail, _ := json.Marshal(generateJobDetail(outcome))
	status := gen.JobRunStatusSucceeded
	var errMsg *string
	if genErr != nil {
		status = gen.JobRunStatusFailed
		msg := genErr.Error()
		errMsg = &msg
	}
	if err := s.q.FinishJobRun(ctx, gen.FinishJobRunParams{
		ID:             jobRun.ID,
		Status:         status,
		ItemsProcessed: int32(len(outcome.Result.Pairings)),
		Error:          errMsg,
		Detail:         detail,
	}); err != nil {
		slog.Error("generate-round: record job outcome", "error", err)
	}

	switch {
	case errors.Is(genErr, rounds.ErrDraftExists):
		http.Redirect(w, r, "/admin/rounds?error=draft-exists", http.StatusSeeOther)
	case genErr != nil:
		serverError(w, genErr)
	default:
		http.Redirect(w, r, fmt.Sprintf("/admin/rounds/%d", outcome.Round.ID), http.StatusSeeOther)
	}
}

func generateJobDetail(outcome rounds.Outcome) map[string]any {
	detail := map[string]any{
		"round_number":    outcome.Round.Number,
		"pool_size":       outcome.Result.PoolSize,
		"pairings":        len(outcome.Result.Pairings),
		"exclusions":      len(outcome.Result.Exclusions),
		"repeat_pairings": outcome.Result.RepeatPairings,
	}
	if outcome.Round.OddPool != nil {
		detail["odd_pool"] = string(*outcome.Round.OddPool)
	}
	return detail
}

// --- round view ---------------------------------------------------

type exclusionGroup struct {
	Reason string
	Rows   []gen.ListExclusionsForRoundRow
}

type roundViewData struct {
	base
	Round      gen.Round
	Pairings   []gen.ListPairingsForRoundRow
	Exclusions []exclusionGroup
	Byes       []gen.ListByesForRoundRow
	Doubles    []gen.ListDoubleGamesForRoundRow
	Solver     string // empty for an imported round
	Error      string
}

// solverOf names the matching algorithm a round was generated with,
// from its settings snapshot. Every round generated before
// pairing.solver existed was greedy, and says nothing; an imported
// round has no snapshot at all.
func solverOf(round gen.Round) string {
	var snap rounds.Snapshot
	if len(round.SettingsUsed) == 0 || json.Unmarshal(round.SettingsUsed, &snap) != nil {
		return ""
	}
	if snap.Solver == "" {
		return string(settings.SolverGreedy)
	}
	return string(snap.Solver)
}

func (s *Server) handleRoundView(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := parseRoundID(w, r)
	if !ok {
		return
	}

	round, err := s.q.GetRoundByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	pairings, err := s.q.ListPairingsForRound(ctx, id)
	if err != nil {
		serverError(w, err)
		return
	}
	exclusions, err := s.q.ListExclusionsForRound(ctx, id)
	if err != nil {
		serverError(w, err)
		return
	}
	byes, err := s.q.ListByesForRound(ctx, id)
	if err != nil {
		serverError(w, err)
		return
	}
	doubles, err := s.q.ListDoubleGamesForRound(ctx, id)
	if err != nil {
		serverError(w, err)
		return
	}

	s.render(w, "admin_round", roundViewData{
		base:       s.page(r, fmt.Sprintf("Round %d", round.Number), "rounds"),
		Round:      round,
		Pairings:   pairings,
		Exclusions: groupExclusions(exclusions),
		Byes:       byes,
		Doubles:    doubles,
		Solver:     solverOf(round),
		Error:      roundViewError(r.URL.Query().Get("error")),
	})
}

// groupExclusions keeps §8.5's "grouped by reason" order stable and
// independent of however the query happened to sort.
func groupExclusions(rows []gen.ListExclusionsForRoundRow) []exclusionGroup {
	order := []gen.ExclusionReason{
		gen.ExclusionReasonAtCapacity, gen.ExclusionReasonInactive, gen.ExclusionReasonPaused,
		gen.ExclusionReasonAutoPaused, gen.ExclusionReasonBye, gen.ExclusionReasonRemovedByAdmin,
		gen.ExclusionReasonPendingApproval, gen.ExclusionReasonNoValidToken,
	}
	byReason := map[gen.ExclusionReason][]gen.ListExclusionsForRoundRow{}
	for _, row := range rows {
		byReason[row.Reason] = append(byReason[row.Reason], row)
	}
	groups := make([]exclusionGroup, 0, len(byReason))
	for _, reason := range order {
		if rows, ok := byReason[reason]; ok {
			groups = append(groups, exclusionGroup{Reason: string(reason), Rows: rows})
		}
	}
	return groups
}

func roundViewError(code string) string {
	if code == "reason" {
		return "A reason is required to cancel a round."
	}
	return ""
}

func parseRoundID(w http.ResponseWriter, r *http.Request) (int32, bool) {
	var id pgtype.Int4
	if err := id.Scan(chi.URLParam(r, "id")); err != nil || !id.Valid {
		http.NotFound(w, r)
		return 0, false
	}
	return id.Int32, true
}

func redirectToRound(w http.ResponseWriter, r *http.Request, id int32, errCode string) {
	target := fmt.Sprintf("/admin/rounds/%d", id)
	if errCode != "" {
		target += "?" + url.Values{"error": {errCode}}.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// roundsTx runs fn inside one transaction, committing only if it
// returns nil. It exists alongside inTx because the rounds/pairing
// service takes a pgx.Tx directly rather than a *gen.Queries.
func (s *Server) roundsTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once committed
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// writeActionError renders a state-conflict error as the HTTP status it
// actually is, rather than a PRG redirect: these are races (the review
// window ended, another admin already removed the pairing) an action
// button's own click cannot itself cause, so there is nothing on the
// round page worth re-rendering — the client reloads and sees the
// current state. ok reports whether err was handled.
func writeActionError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, rounds.ErrNotDraft):
		http.Error(w, "round is not a draft", http.StatusConflict)
		return true
	case errors.Is(err, rounds.ErrNotFound):
		http.Error(w, "pairing not found in this round", http.StatusNotFound)
		return true
	default:
		return false
	}
}

// --- round actions --------------------------------------------------

func (s *Server) handlePublishRound(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin, _ := currentUser(r)
	id, ok := parseRoundID(w, r)
	if !ok {
		return
	}

	err := s.roundsTx(ctx, func(tx pgx.Tx) error {
		_, _, err := rounds.Publish(ctx, tx, id, time.Now(), admin.ID)
		return err
	})
	if err != nil {
		serverError(w, err)
		return
	}
	redirectToRound(w, r, id, "")
}

func (s *Server) handleCancelRound(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin, _ := currentUser(r)
	id, ok := parseRoundID(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	reason := r.PostForm.Get("reason")
	if reason == "" {
		redirectToRound(w, r, id, "reason")
		return
	}

	err := s.roundsTx(ctx, func(tx pgx.Tx) error {
		_, err := rounds.Cancel(ctx, tx, id, reason, admin.ID)
		return err
	})
	switch {
	case writeActionError(w, err):
		return
	case err != nil:
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/rounds", http.StatusSeeOther)
}

func (s *Server) handleRegenerateRound(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin, _ := currentUser(r)
	id, ok := parseRoundID(w, r)
	if !ok {
		return
	}

	err := s.roundsTx(ctx, func(tx pgx.Tx) error {
		cfg, err := settings.Load(ctx, gen.New(tx))
		if err != nil {
			return err
		}
		_, err = rounds.Regenerate(ctx, tx, id, cfg, admin.ID, nil)
		return err
	})
	switch {
	case writeActionError(w, err):
		return
	case err != nil:
		serverError(w, err)
		return
	}
	redirectToRound(w, r, id, "")
}

func (s *Server) handleFlipPairing(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin, _ := currentUser(r)
	roundID, ok := parseRoundID(w, r)
	if !ok {
		return
	}
	pairingID, ok := parsePairingID(w, r, roundID)
	if !ok {
		return
	}

	err := s.roundsTx(ctx, func(tx pgx.Tx) error {
		_, err := rounds.FlipColours(ctx, tx, roundID, pairingID, admin.ID)
		return err
	})
	switch {
	case writeActionError(w, err):
		return
	case err != nil:
		serverError(w, err)
		return
	}
	redirectToRound(w, r, roundID, "")
}

func (s *Server) handleRemovePairing(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin, _ := currentUser(r)
	roundID, ok := parseRoundID(w, r)
	if !ok {
		return
	}
	pairingID, ok := parsePairingID(w, r, roundID)
	if !ok {
		return
	}

	err := s.roundsTx(ctx, func(tx pgx.Tx) error {
		return rounds.RemovePairing(ctx, tx, roundID, pairingID, admin.ID)
	})
	switch {
	case writeActionError(w, err):
		return
	case err != nil:
		serverError(w, err)
		return
	}
	redirectToRound(w, r, roundID, "")
}

func (s *Server) handleFailPairing(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin, _ := currentUser(r)
	roundID, ok := parseRoundID(w, r)
	if !ok {
		return
	}
	pairingID, ok := parsePairingID(w, r, roundID)
	if !ok {
		return
	}

	err := s.roundsTx(ctx, func(tx pgx.Tx) error {
		_, err := rounds.MarkFailed(ctx, tx, roundID, pairingID, admin.ID)
		return err
	})
	switch {
	case writeActionError(w, err):
		return
	case err != nil:
		serverError(w, err)
		return
	}
	redirectToRound(w, r, roundID, "")
}

func (s *Server) handleSwapPairings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin, _ := currentUser(r)
	roundID, ok := parseRoundID(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var a, b pgtype.UUID
	if err := a.Scan(r.PostForm.Get("a")); err != nil {
		http.Error(w, "bad pairing id", http.StatusBadRequest)
		return
	}
	if err := b.Scan(r.PostForm.Get("b")); err != nil {
		http.Error(w, "bad pairing id", http.StatusBadRequest)
		return
	}

	err := s.roundsTx(ctx, func(tx pgx.Tx) error {
		cfg, err := settings.Load(ctx, gen.New(tx))
		if err != nil {
			return err
		}
		return rounds.SwapPairings(ctx, tx, cfg, roundID, a, b, admin.ID)
	})
	switch {
	case writeActionError(w, err):
		return
	case err != nil:
		serverError(w, err)
		return
	}
	redirectToRound(w, r, roundID, "")
}

func parsePairingID(w http.ResponseWriter, r *http.Request, roundID int32) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if err := id.Scan(chi.URLParam(r, "pid")); err != nil {
		http.Error(w, "bad pairing id", http.StatusBadRequest)
		return pgtype.UUID{}, false
	}
	return id, true
}
