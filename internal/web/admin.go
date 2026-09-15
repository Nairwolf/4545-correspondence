package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/standings"
)

// The admin registration queue (spec §8.5): pending applications with
// the Lichess signals from §8.2, approve (singly or in bulk) and reject
// with a reason. Every route under /admin sits behind requireAdmin.

// newAccountDays is how young a Lichess account has to be for the queue
// to flag it.
const newAccountDays = 30

// registrationRow is one applicant as the queue shows them.
type registrationRow struct {
	ID       string
	Username string
	Applied  pgtype.Timestamptz
	Agreed   bool

	// Signals, all derived from users.lichess_profile at render time.
	ProfileMissing bool // seeded user, or a profile we could not decode
	AccountAge     string
	NewAccount     bool
	RatedGames     int
	Closed         bool
	TOSViolation   bool
	Correspondence *lichess.Perf
	Classical      *lichess.Perf

	RejectionReason *string
}

type registrationsData struct {
	base
	Tab           string
	Rows          []registrationRow
	PendingCount  int
	RejectedCount int
	Error         string
}

func (s *Server) handleAdminIndex(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/registrations", http.StatusFound)
}

// handleRegistrations renders one tab of the queue.
func (s *Server) handleRegistrations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tab := r.URL.Query().Get("tab")
	if tab != "rejected" {
		tab = "pending"
	}

	pending, err := s.q.ListUsersByStatus(ctx, gen.UserStatusPending)
	if err != nil {
		serverError(w, err)
		return
	}
	rejected, err := s.q.ListUsersByStatus(ctx, gen.UserStatusRejected)
	if err != nil {
		serverError(w, err)
		return
	}
	users := pending
	if tab == "rejected" {
		users = rejected
	}
	rows := make([]registrationRow, 0, len(users))
	for _, u := range users {
		rows = append(rows, registrationRowFor(u, time.Now()))
	}

	s.render(w, "admin_registrations", registrationsData{
		base:          s.page(r, "Registrations", "admin"),
		Tab:           tab,
		Rows:          rows,
		PendingCount:  len(pending),
		RejectedCount: len(rejected),
		Error:         queueError(r.URL.Query().Get("error")),
	})
}

// queueError turns the redirect-back error code into a sentence.
func queueError(code string) string {
	switch code {
	case "reason":
		return "A reason is required to reject an application."
	case "stale":
		return "One of the selected applications is no longer in that state; nothing was changed."
	case "bad-id":
		return "The request named an application that does not exist."
	}
	return ""
}

// registrationRowFor derives the queue's signals for one applicant.
func registrationRowFor(u gen.User, now time.Time) registrationRow {
	row := registrationRow{
		ID:              u.ID.String(),
		Username:        u.LichessUsername,
		Applied:         u.CreatedAt,
		Agreed:          u.FairPlayAgreedAt.Valid,
		RejectionReason: u.RejectionReason,
	}

	var acct lichess.Account
	if len(u.LichessProfile) == 0 || json.Unmarshal(u.LichessProfile, &acct) != nil {
		row.ProfileMissing = true
		return row
	}
	if acct.CreatedAt > 0 {
		age := now.Sub(acct.CreatedAtTime())
		row.AccountAge = humanAge(age)
		row.NewAccount = age < newAccountDays*24*time.Hour
	}
	row.RatedGames = acct.Count.Rated
	row.Closed = acct.Disabled
	row.TOSViolation = acct.TOSViolation
	row.Correspondence = acct.Perfs.Correspondence
	row.Classical = acct.Perfs.Classical
	return row
}

// humanAge renders an account age the way a reviewer thinks about it:
// "3 years", "5 months", "12 days".
func humanAge(d time.Duration) string {
	days := int(d.Hours() / 24)
	switch {
	case days >= 365:
		return plural(days/365, "year")
	case days >= 30:
		return plural(days/30, "month")
	default:
		return plural(days, "day")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// --- actions -----------------------------------------------------------

// redirectToQueue sends the admin back to the tab they acted from, with
// an optional error code for queueError.
func redirectToQueue(w http.ResponseWriter, r *http.Request, tab, errCode string) {
	q := url.Values{}
	if tab == "rejected" {
		q.Set("tab", tab)
	}
	if errCode != "" {
		q.Set("error", errCode)
	}
	target := "/admin/registrations"
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleApproveRegistrations approves every id in the form's ids field
// — one for the per-row button, several for "approve selected" — in a
// single transaction: a stale id rolls the whole thing back.
func (s *Server) handleApproveRegistrations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin, _ := currentUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	tab := r.PostForm.Get("tab")

	var ids []pgtype.UUID
	for _, raw := range r.PostForm["ids"] {
		var id pgtype.UUID
		if err := id.Scan(raw); err != nil {
			redirectToQueue(w, r, tab, "bad-id")
			return
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		redirectToQueue(w, r, tab, "")
		return
	}

	err := s.inTx(ctx, func(q *gen.Queries) error {
		for _, id := range ids {
			if err := s.approve(ctx, q, admin, id); err != nil {
				return err
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		redirectToQueue(w, r, tab, "stale")
	case err != nil:
		serverError(w, err)
	default:
		redirectToQueue(w, r, tab, "")
	}
}

// approve is one approval: the status change, the audit row, and the
// player's first standings row so they appear on /standings now rather
// than after the nightly recompute.
func (s *Server) approve(ctx context.Context, q *gen.Queries, admin gen.User, id pgtype.UUID) error {
	before, err := q.GetUserByID(ctx, id)
	if err != nil {
		return err // pgx.ErrNoRows included: an unknown id is as stale as an approved one
	}
	user, err := q.ApproveUser(ctx, gen.ApproveUserParams{ID: id, ApprovedBy: admin.ID})
	if err != nil {
		return err
	}
	if _, err := standings.Recompute(ctx, q, user.ID, s.cfg); err != nil {
		return err
	}
	return audit(ctx, q, admin.ID, "registration.approve", user.ID,
		map[string]string{"status": string(before.Status)},
		map[string]string{"status": string(user.Status)},
	)
}

// handleRejectRegistration rejects one pending application. The reason
// is required: it is what the applicant sees on their account page.
func (s *Server) handleRejectRegistration(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin, _ := currentUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	tab := r.PostForm.Get("tab")

	var id pgtype.UUID
	if err := id.Scan(chi.URLParam(r, "id")); err != nil {
		redirectToQueue(w, r, tab, "bad-id")
		return
	}
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	if reason == "" {
		redirectToQueue(w, r, tab, "reason")
		return
	}

	err := s.inTx(ctx, func(q *gen.Queries) error {
		user, err := q.RejectUser(ctx, gen.RejectUserParams{ID: id, RejectionReason: &reason})
		if err != nil {
			return err
		}
		return audit(ctx, q, admin.ID, "registration.reject", user.ID,
			map[string]string{"status": string(gen.UserStatusPending)},
			map[string]string{"status": string(user.Status), "reason": reason},
		)
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		redirectToQueue(w, r, tab, "stale")
	case err != nil:
		serverError(w, err)
	default:
		redirectToQueue(w, r, tab, "")
	}
}

// inTx runs fn inside one transaction, committing only if it returns nil.
func (s *Server) inTx(ctx context.Context, fn func(q *gen.Queries) error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once committed
	if err := fn(gen.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
