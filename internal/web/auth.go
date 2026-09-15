package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/standings"
)

// Registration and sign-in (spec §8.2, §3.1). Both start the same
// Lichess OAuth flow and converge on one callback; the difference is
// the intent recorded in the state cookie: /join may create a pending
// user, /login only ever signs an existing one in.

const (
	oauthCookieName = "ic_oauth"
	oauthCookieTTL  = 10 * time.Minute

	intentJoin  = "join"
	intentLogin = "login"
)

// scopes is what every player grants. msg:write (Lichess PMs, spec §10)
// is not requested yet; Phase 5 asks players to re-authorise for it.
var scopes = []string{"challenge:write"}

// oauthState is what the browser carries between the redirect to
// Lichess and the callback, signed so it cannot be forged: the state
// Lichess must echo back, the PKCE verifier, and what the user set out
// to do.
type oauthState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Intent   string `json:"i"`
	Expires  int64  `json:"e"` // unix seconds
}

// --- state cookie ------------------------------------------------------

// signState encodes st as base64url(json) + "." + base64url(hmac).
func signState(secret []byte, st oauthState) (string, error) {
	payload, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString(payload)
	return enc + "." + sign(secret, enc), nil
}

// verifyState reverses signState, rejecting a bad signature or an
// expired state. now is injected so the expiry is testable.
func verifyState(secret []byte, value string, now time.Time) (oauthState, bool) {
	enc, sig, ok := strings.Cut(value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(sign(secret, enc))) {
		return oauthState{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return oauthState{}, false
	}
	var st oauthState
	if err := json.Unmarshal(payload, &st); err != nil || now.Unix() >= st.Expires {
		return oauthState{}, false
	}
	return st, true
}

func sign(secret []byte, msg string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// --- handlers ----------------------------------------------------------

type joinData struct {
	base
	Error string
}

// handleJoin shows the join page: what the league is and the fair-play
// agreement, which must be ticked before the redirect to Lichess.
func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	if _, ok := currentUser(r); ok {
		http.Redirect(w, r, "/account", http.StatusFound)
		return
	}
	s.render(w, "join", joinData{base: s.page(r, "Join the league", "")})
}

// handleJoinPost validates the agreement and starts the OAuth flow
// with intent=join.
func (s *Server) handleJoinPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("agree") != "on" {
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, "join", joinData{
			base:  s.page(r, "Join the league", ""),
			Error: "You need to agree to the fair-play rules to join.",
		})
		return
	}
	s.beginOAuth(w, r, intentJoin)
}

// handleLogin starts the OAuth flow with intent=login. There is no
// page: signing in is one click on Lichess.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := currentUser(r); ok {
		http.Redirect(w, r, "/account", http.StatusFound)
		return
	}
	s.beginOAuth(w, r, intentLogin)
}

// beginOAuth sets the signed state cookie and redirects to Lichess.
func (s *Server) beginOAuth(w http.ResponseWriter, r *http.Request, intent string) {
	state, err := randomToken()
	if err != nil {
		serverError(w, err)
		return
	}
	st := oauthState{
		State:    state,
		Verifier: lichess.GenerateVerifier(),
		Intent:   intent,
		Expires:  time.Now().Add(oauthCookieTTL).Unix(),
	}
	value, err := signState(s.stateSecret, st)
	if err != nil {
		serverError(w, err)
		return
	}
	// Lax, not Strict: the callback is a top-level navigation from
	// lichess.org, and Strict would withhold the cookie on exactly that
	// request.
	http.SetCookie(w, &http.Cookie{
		Name:     oauthCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(oauthCookieTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.auth.AuthCodeURL(st.State, st.Verifier, scopes), http.StatusFound)
}

func (s *Server) clearOAuthCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     oauthCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

// authMessage is the data for the auth_error page: every outcome of a
// sign-in that isn't "signed in" is explained on it.
type authMessage struct {
	base
	Heading  string
	Message  string
	LinkHref string
	LinkText string
}

func (s *Server) renderAuthMessage(w http.ResponseWriter, r *http.Request, status int, heading, message, linkHref, linkText string) {
	w.WriteHeader(status)
	s.render(w, "auth_error", authMessage{
		base:     s.page(r, heading, ""),
		Heading:  heading,
		Message:  message,
		LinkHref: linkHref,
		LinkText: linkText,
	})
}

// handleCallback is where Lichess sends the browser back. Order matters:
// the state is checked before anything else is believed, the token is
// exchanged and the account fetched, and only then is anything written
// — all of it in one transaction (completeSignIn).
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.clearOAuthCookie(w)

	q := r.URL.Query()
	var st oauthState
	if c, err := r.Cookie(oauthCookieName); err == nil {
		st, _ = verifyState(s.stateSecret, c.Value, time.Now())
	}
	if st.State == "" || st.State != q.Get("state") {
		s.renderAuthMessage(w, r, http.StatusBadRequest,
			"Sign-in could not be verified",
			"The sign-in link is stale or was not started from this site. Please start again.",
			"/login", "Sign in again")
		return
	}

	if code := q.Get("error"); code != "" {
		if code == "access_denied" {
			s.renderAuthMessage(w, r, http.StatusOK,
				"Sign-in cancelled",
				"You cancelled on Lichess, so nothing was changed here. You can come back whenever you like.",
				"/", "Back to the overview")
			return
		}
		slog.Warn("lichess oauth error", "error", code, "description", q.Get("error_description"))
		s.renderAuthMessage(w, r, http.StatusBadGateway,
			"Lichess refused the sign-in",
			"Lichess reported: "+code+". Please try again in a moment.",
			"/login", "Try again")
		return
	}

	tok, err := s.auth.Exchange(ctx, q.Get("code"), st.Verifier)
	if err != nil {
		slog.Error("lichess token exchange failed", "error", err)
		s.renderAuthMessage(w, r, http.StatusBadGateway,
			"Lichess did not complete the sign-in",
			"The code Lichess sent back could not be exchanged for a token. Please try again.",
			"/login", "Try again")
		return
	}

	acct, raw, err := s.lichess.Account(ctx, tok.AccessToken)
	if err != nil {
		slog.Error("lichess account lookup failed", "error", err)
		s.renderAuthMessage(w, r, http.StatusBadGateway,
			"Could not read your Lichess account",
			"Lichess authorised the sign-in but did not return your profile. Please try again.",
			"/login", "Try again")
		return
	}

	user, outcome, err := s.completeSignIn(ctx, st.Intent, tok, acct, raw)
	if err != nil {
		serverError(w, err)
		return
	}
	slog.Info("sign-in callback", "intent", st.Intent, "lichess_id", acct.ID, "outcome", outcome)

	switch outcome {
	case outcomeNotMember:
		s.revokeQuietly(ctx, tok)
		s.renderAuthMessage(w, r, http.StatusOK,
			"You're not a member yet",
			"There is no league account for "+acct.Username+". Joining takes one click and an admin's approval.",
			"/join", "Join the league")
		return
	case outcomeBanned:
		s.revokeQuietly(ctx, tok)
		s.renderAuthMessage(w, r, http.StatusForbidden,
			"This account cannot sign in",
			"The league account for "+acct.Username+" has been closed by the admins.",
			"/", "Back to the overview")
		return
	}

	if err := s.sessions.Create(ctx, w, user.ID); err != nil {
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/account", http.StatusFound)
}

// revokeQuietly gives back a token we will not keep. Failing to revoke
// is logged, not surfaced: the user's page is the same either way.
func (s *Server) revokeQuietly(ctx context.Context, tok lichess.Token) {
	if err := s.lichess.RevokeToken(ctx, tok.AccessToken); err != nil {
		slog.Warn("could not revoke unwanted lichess token", "error", err)
	}
}

type signInOutcome string

const (
	outcomeSignedIn  signInOutcome = "signed_in"
	outcomeNotMember signInOutcome = "not_member"
	outcomeBanned    signInOutcome = "banned"
)

// completeSignIn does every database write of a sign-in in one
// transaction: match the Lichess id to a user (creating one for a
// join), store the token, refresh the profile, and apply the bootstrap
// admin rule. On outcomeNotMember and outcomeBanned nothing is written.
func (s *Server) completeSignIn(
	ctx context.Context,
	intent string,
	tok lichess.Token,
	acct lichess.Account,
	rawProfile []byte,
) (gen.User, signInOutcome, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return gen.User{}, "", fmt.Errorf("sign-in: begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once committed
	q := gen.New(tx)

	user, err := q.GetUserByLichessUserID(ctx, acct.ID)
	switch {
	case err == nil:
		if user.Status == gen.UserStatusBanned {
			return gen.User{}, outcomeBanned, nil
		}
		if err := s.signInExisting(ctx, q, &user, tok, acct, rawProfile); err != nil {
			return gen.User{}, "", err
		}
	case errors.Is(err, pgx.ErrNoRows) && intent == intentJoin:
		user, err = s.register(ctx, q, tok, acct, rawProfile)
		if err != nil {
			return gen.User{}, "", err
		}
	case errors.Is(err, pgx.ErrNoRows):
		return gen.User{}, outcomeNotMember, nil
	default:
		return gen.User{}, "", fmt.Errorf("sign-in: look up user: %w", err)
	}

	if s.adminIDs[acct.ID] {
		if err := bootstrapAdmin(ctx, q, &user); err != nil {
			return gen.User{}, "", err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return gen.User{}, "", fmt.Errorf("sign-in: commit: %w", err)
	}
	return user, outcomeSignedIn, nil
}

// signInExisting is a returning member: follow a Lichess rename, refresh
// the stored profile, and replace the token (spec §3.1's one-click
// re-authorisation is exactly this).
func (s *Server) signInExisting(
	ctx context.Context,
	q *gen.Queries,
	user *gen.User,
	tok lichess.Token,
	acct lichess.Account,
	rawProfile []byte,
) error {
	if user.LichessUsername != acct.Username {
		if err := q.RenameUser(ctx, gen.RenameUserParams{ID: user.ID, LichessUsername: acct.Username}); err != nil {
			return fmt.Errorf("sign-in: rename: %w", err)
		}
		if err := audit(ctx, q, user.ID, "user.rename", user.ID,
			map[string]string{"lichess_username": user.LichessUsername},
			map[string]string{"lichess_username": acct.Username},
		); err != nil {
			return err
		}
		user.LichessUsername = acct.Username
	}

	if err := q.UpdateUserLichessProfile(ctx, gen.UpdateUserLichessProfileParams{ID: user.ID, LichessProfile: rawProfile}); err != nil {
		return fmt.Errorf("sign-in: store profile: %w", err)
	}

	action := "auth.sign_in"
	if _, err := q.GetOAuthToken(ctx, user.ID); err == nil {
		action = "auth.reauthorise"
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("sign-in: read token: %w", err)
	}
	if err := s.storeToken(ctx, q, user.ID, tok); err != nil {
		return err
	}
	return audit(ctx, q, user.ID, action, user.ID, nil, map[string]any{"scopes": scopes})
}

// register creates a pending applicant: user, profile, first rating
// snapshot and token, all from the one /api/account response.
func (s *Server) register(
	ctx context.Context,
	q *gen.Queries,
	tok lichess.Token,
	acct lichess.Account,
	rawProfile []byte,
) (gen.User, error) {
	user, err := q.CreatePendingUser(ctx, gen.CreatePendingUserParams{
		LichessUsername: acct.Username,
		LichessUserID:   acct.ID,
		LichessProfile:  rawProfile,
	})
	if err != nil {
		return gen.User{}, fmt.Errorf("register: create user: %w", err)
	}
	if _, err := q.CreatePlayerProfile(ctx, user.ID); err != nil {
		return gen.User{}, fmt.Errorf("register: create profile: %w", err)
	}
	if _, err := q.InsertRatingSnapshot(ctx, standings.SnapshotParams(user.ID, acct.User)); err != nil {
		return gen.User{}, fmt.Errorf("register: rating snapshot: %w", err)
	}
	if err := s.storeToken(ctx, q, user.ID, tok); err != nil {
		return gen.User{}, err
	}
	err = audit(ctx, q, user.ID, "registration.apply", user.ID, nil, map[string]any{
		"lichess_username": user.LichessUsername,
		"lichess_user_id":  user.LichessUserID,
		"scopes":           scopes,
	})
	return user, err
}

// storeToken encrypts and upserts the player's token.
func (s *Server) storeToken(ctx context.Context, q *gen.Queries, userID pgtype.UUID, tok lichess.Token) error {
	sealed, err := s.tokenKey.Seal(tok.AccessToken)
	if err != nil {
		return err
	}
	var expires pgtype.Timestamptz
	if !tok.ExpiresAt.IsZero() {
		expires = pgtype.Timestamptz{Time: tok.ExpiresAt, Valid: true}
	}
	err = q.UpsertOAuthToken(ctx, gen.UpsertOAuthTokenParams{
		UserID:      userID,
		AccessToken: sealed,
		Scopes:      scopes,
		ExpiresAt:   expires,
	})
	if err != nil {
		return fmt.Errorf("sign-in: store token: %w", err)
	}
	return nil
}

// bootstrapAdmin applies ADMIN_LICHESS_USERNAMES: the listed account is
// approved (the first admin has nobody else to do it) and made an admin.
// A no-op, and no audit row, when it already is both.
func bootstrapAdmin(ctx context.Context, q *gen.Queries, user *gen.User) error {
	changed := false
	if user.Status != gen.UserStatusApproved {
		approved, err := q.ApproveUser(ctx, gen.ApproveUserParams{ID: user.ID}) // approved_by NULL: the system
		if err != nil {
			return fmt.Errorf("bootstrap admin: approve: %w", err)
		}
		*user = approved
		changed = true
	}
	if user.Role != gen.UserRoleAdmin {
		if err := q.PromoteToAdmin(ctx, user.ID); err != nil {
			return fmt.Errorf("bootstrap admin: promote: %w", err)
		}
		user.Role = gen.UserRoleAdmin
		changed = true
	}
	if !changed {
		return nil
	}
	return audit(ctx, q, user.ID, "auth.bootstrap_admin", user.ID, nil, map[string]string{
		"status": string(user.Status), "role": string(user.Role),
	})
}

// audit writes one audit_log row about a user, by a user.
func audit(ctx context.Context, q *gen.Queries, actor pgtype.UUID, action string, subject pgtype.UUID, before, after any) error {
	var beforeJSON, afterJSON []byte
	if before != nil {
		beforeJSON, _ = json.Marshal(before)
	}
	if after != nil {
		afterJSON, _ = json.Marshal(after)
	}
	err := q.CreateAuditLogEntry(ctx, gen.CreateAuditLogEntryParams{
		ActorUserID: actor,
		Action:      action,
		EntityType:  "user",
		EntityID:    subject.String(),
		Before:      beforeJSON,
		After:       afterJSON,
	})
	if err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}

// handleLogout ends the session. POST only, so a cross-site link cannot
// sign someone out.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.sessions.Destroy(r.Context(), w, r); err != nil {
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

type accountData struct {
	base
	User      gen.User
	Token     *gen.OauthToken // nil when none stored
	TokenOK   bool            // stored, not revoked, not expired
	ExpiresOn string
}

// handleAccount is the Phase 2 account page: application status in
// plain words, and the state of the Lichess authorisation. Phase 3
// grows it into the dashboard.
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	data := accountData{base: s.page(r, "Your account", "account"), User: user}

	tok, err := s.q.GetOAuthToken(r.Context(), user.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// seeded member who has never signed in before: no token yet
	case err != nil:
		serverError(w, err)
		return
	default:
		data.Token = &tok
		data.TokenOK = !tok.RevokedAt.Valid && (!tok.ExpiresAt.Valid || tok.ExpiresAt.Time.After(time.Now()))
		if tok.ExpiresAt.Valid {
			data.ExpiresOn = tok.ExpiresAt.Time.UTC().Format("2 January 2006")
		}
	}
	s.render(w, "account", data)
}
