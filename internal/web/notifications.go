package web

import (
	"net/http"
	"slices"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/notify"
)

// The notification centre's pages (spec §8.3, §10): the list at
// /account/notifications, marking one or all read, and the opt-outs
// the dashboard sets. Notifications are written by internal/notify
// where each event happens; nothing here creates one.

// notificationsShown is how many notifications the list shows, newest
// first.
const notificationsShown = 50

type notificationsData struct {
	base
	Notifications []gen.Notification
	More          bool // older notifications exist beyond the list
}

// handleNotifications renders the list.
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	list, err := s.q.ListNotificationsForUser(r.Context(), gen.ListNotificationsForUserParams{
		UserID: user.ID,
		Limit:  notificationsShown + 1,
	})
	if err != nil {
		serverError(w, err)
		return
	}
	data := notificationsData{base: s.page(r, "Notifications", "notifications")}
	if len(list) > notificationsShown {
		data.More = true
		list = list[:notificationsShown]
	}
	data.Notifications = list
	s.render(w, "notifications", data)
}

// handleMarkNotificationRead marks one of the player's notifications
// read. An id that is not theirs, or already read, changes nothing.
func (s *Server) handleMarkNotificationRead(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad notification id", http.StatusBadRequest)
		return
	}
	_, err = s.q.MarkNotificationRead(r.Context(), gen.MarkNotificationReadParams{ID: id, UserID: user.ID})
	if err != nil {
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/account/notifications", http.StatusSeeOther)
}

// handleMarkAllNotificationsRead marks every unread notification read.
func (s *Server) handleMarkAllNotificationsRead(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	if _, err := s.q.MarkAllNotificationsRead(r.Context(), user.ID); err != nil {
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/account/notifications", http.StatusSeeOther)
}

// notificationPref is one checkbox of the dashboard's Notifications
// block.
type notificationPref struct {
	Category notify.Category
	Label    string
	On       bool
}

// notificationPrefsFor lists every opt-out-able category with whether
// the player still receives it.
func notificationPrefsFor(muted []string) []notificationPref {
	prefs := make([]notificationPref, 0, len(notify.OptOuts))
	for _, o := range notify.OptOuts {
		prefs = append(prefs, notificationPref{
			Category: o.Category,
			Label:    o.Label,
			On:       !slices.Contains(muted, string(o.Category)),
		})
	}
	return prefs
}

// handleSetNotificationPreferences saves the dashboard's checkboxes. A
// ticked box means "tell me"; an unticked one is stored as an opt-out
// row (spec §4.1). The account-critical categories have no box: they
// are always sent (§10).
func (s *Server) handleSetNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.settingsRequest(w, r)
	if !ok {
		return
	}
	before, err := s.q.ListMutedNotificationCategories(r.Context(), user.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	keep := r.PostForm["send"]
	after := []string{} // [] rather than null in the audit row
	for _, o := range notify.OptOuts {
		if !slices.Contains(keep, string(o.Category)) {
			after = append(after, string(o.Category))
		}
	}
	slices.Sort(after) // the order ListMutedNotificationCategories returns
	if slices.Equal(before, after) {
		redirectToDashboard(w, r, "notifications")
		return
	}
	if before == nil {
		before = []string{}
	}

	err = s.inTx(r.Context(), func(q *gen.Queries) error {
		for _, o := range notify.OptOuts {
			category := string(o.Category)
			var err error
			if slices.Contains(after, category) {
				err = q.MuteNotificationCategory(r.Context(), gen.MuteNotificationCategoryParams{
					UserID:   user.ID,
					Category: category,
				})
			} else {
				err = q.UnmuteNotificationCategory(r.Context(), gen.UnmuteNotificationCategoryParams{
					UserID:   user.ID,
					Category: category,
				})
			}
			if err != nil {
				return err
			}
		}
		return audit(r.Context(), q, user.ID, "profile.notifications", user.ID,
			map[string][]string{"muted": before},
			map[string][]string{"muted": after},
		)
	})
	if err != nil {
		serverError(w, err)
		return
	}
	redirectToDashboard(w, r, "notifications")
}
