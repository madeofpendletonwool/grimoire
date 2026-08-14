package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/chat"
)

// sessionCookie is the browser's half of a session. The value is opaque — the
// server keeps the authoritative record — so there is nothing to tamper with.
const sessionCookie = "grimoire_session"

// errUnauthenticated is the only thing an unauthenticated API caller learns.
var errUnauthenticated = errors.New("sign in to continue")

// userCtxKey carries the authenticated user down to the handlers.
type ctxKey int

const userCtxKey ctxKey = 0

func withUser(ctx context.Context, u *auth.User) context.Context {
	return context.WithValue(ctx, userCtxKey, u)
}

func userFrom(ctx context.Context) (*auth.User, bool) {
	u, ok := ctx.Value(userCtxKey).(*auth.User)
	return u, ok
}

// openAPIPaths are the endpoints a signed-out browser must still reach: the
// handshake it uses to decide between the login and setup forms, the two ways
// of getting a session in the first place, and the invite-only signup that a
// friend follows from an admin's invite link.
var openAPIPaths = map[string]bool{
	"/api/auth/state":    true,
	"/api/auth/login":    true,
	"/api/auth/setup":    true,
	"/api/auth/register": true,
}

// requireSession gates everything under /api/ on a valid session. It is a
// prefix gate rather than a per-route wrapper so an endpoint added later is
// protected by default rather than by remembering; /healthz stays outside it
// because Docker's healthcheck has no session to present.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Match on the cleaned path. ServeMux redirects "//api/chats" and
		// "/api/auth/state/../chats" to their canonical form rather than
		// serving them, but the gate runs before the mux, so it does its own
		// cleaning instead of trusting the raw path to be canonical.
		p := path.Clean(r.URL.Path)
		if s.users == nil || !strings.HasPrefix(p, "/api/") || openAPIPaths[p] {
			next.ServeHTTP(w, r)
			return
		}
		u, err := s.users.Lookup(r.Context(), sessionToken(r))
		if err != nil {
			writeError(w, http.StatusUnauthorized, errUnauthenticated)
			return
		}
		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), u)))
	})
}

// currentUser resolves the caller outside the gated handlers — on / and on the
// open auth endpoints, where "not signed in" is an answer rather than an error.
func (s *Server) currentUser(r *http.Request) *auth.User {
	if s.users == nil {
		return nil
	}
	if u, ok := userFrom(r.Context()); ok {
		return u
	}
	u, err := s.users.Lookup(r.Context(), sessionToken(r))
	if err != nil {
		return nil
	}
	return u
}

func sessionToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// secureCookies reports whether the response travels over TLS, either directly
// or via a reverse proxy that terminated it. Marking the cookie Secure on a
// plain-HTTP LAN install would make the browser drop it, so this is detected
// per request rather than assumed.
func secureCookies(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i] // a chain of proxies appends; the client-facing hop is first
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sess.Token,
		Path:     "/",
		MaxAge:   int(s.users.TTL().Seconds()),
		HttpOnly: true,
		Secure:   secureCookies(r),
		// Lax, not Strict: the app is often opened from a bookmark or a link
		// on another site, and Strict would show the login screen on arrival
		// even though the session is perfectly good.
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secureCookies(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// authStateView tells the login page which form to draw. It is deliberately
// thin: an unclaimed install is the only thing a signed-out caller learns. The
// IsAdmin flag is the one piece of account detail that leaks to a signed-in
// caller, and only so the shell knows to draw the invite manager.
type authStateView struct {
	Authenticated    bool   `json:"authenticated"`
	Username         string `json:"username,omitempty"`
	IsAdmin          bool   `json:"is_admin"`
	SetupRequired    bool   `json:"setup_required"`
	RegistrationOpen bool   `json:"registration_open"`
}

func (s *Server) authState(r *http.Request) (authStateView, error) {
	v := authStateView{RegistrationOpen: s.openRegistration}
	if s.users == nil {
		// Authentication is not wired up at all; the caller is already through.
		v.Authenticated = true
		return v, nil
	}
	n, err := s.users.Count(r.Context())
	if err != nil {
		return v, err
	}
	v.SetupRequired = n == 0
	if u := s.currentUser(r); u != nil {
		v.Authenticated = true
		v.Username = u.Username
		v.IsAdmin = u.IsAdmin
	}
	return v, nil
}

func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	v, err := s.authState(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func decodeCredentials(r *http.Request) (credentials, error) {
	var c credentials
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		return c, fmt.Errorf("invalid request body")
	}
	return c, nil
}

// handleSetup creates an account. It is the first-run flow: on an unclaimed
// install anyone who can reach the app may claim it, which is the right trade
// for a personal server (locking the owner out of their own box is worse than
// the window between `docker compose up` and the first login). Once an account
// exists the endpoint is closed unless the operator opened registration.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled(w) {
		return
	}
	n, err := s.users.Count(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if n > 0 && !s.openRegistration {
		writeError(w, http.StatusForbidden, fmt.Errorf("this grimoire already has a keeper"))
		return
	}

	creds, err := decodeCredentials(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u, err := s.users.CreateUser(r.Context(), creds.Username, creds.Password)
	if err != nil {
		if errors.Is(err, auth.ErrUsernameTaken) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// The first keeper inherits everything written before there were accounts.
	if adopted, err := AdoptAnonymousChats(r.Context(), s.users, s.chats); err != nil {
		log.Printf("auth: adopt anonymous chats: %v", err)
	} else if adopted > 0 {
		log.Printf("auth: %d pre-existing conversations adopted by %q", adopted, u.Username)
	}

	s.startSession(w, r, u, http.StatusCreated)
}

// registrationRequest is the invite-gated signup body. Invite is the secret
// from an admin's invite link; without a valid one, no account is created.
type registrationRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Invite   string `json:"invite"`
}

// handleRegister signs a friend up from an invite link. Unlike setup, it always
// requires a valid, unused, unexpired invite — self-service creation stays off
// — and the new account is never an admin. The whole validate-create-consume
// run is one transaction in the store, so a taken name or a bad invite leaves
// nothing behind.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled(w) {
		return
	}
	var req registrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	u, err := s.users.RegisterWithInvite(r.Context(), req.Username, req.Password, req.Invite)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrInviteInvalid):
			writeError(w, http.StatusBadRequest, err)
		case errors.Is(err, auth.ErrInviteUsed), errors.Is(err, auth.ErrInviteExpired):
			writeError(w, http.StatusGone, err)
		case errors.Is(err, auth.ErrUsernameTaken):
			writeError(w, http.StatusConflict, err)
		default:
			writeError(w, http.StatusBadRequest, err)
		}
		return
	}
	s.startSession(w, r, u, http.StatusCreated)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled(w) {
		return
	}
	creds, err := decodeCredentials(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u, err := s.users.Authenticate(r.Context(), creds.Username, creds.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			writeError(w, http.StatusUnauthorized, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.startSession(w, r, u, http.StatusOK)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled(w) {
		return
	}
	if err := s.users.EndSession(r.Context(), sessionToken(r)); err != nil {
		log.Printf("auth: end session: %v", err)
	}
	clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *auth.User, status int) {
	sess, err := s.users.StartSession(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.setSessionCookie(w, r, sess)
	writeJSON(w, status, map[string]any{"user": map[string]any{"username": u.Username}})
}

// authEnabled reports whether accounts are available, writing the error
// response when they are not.
func (s *Server) authEnabled(w http.ResponseWriter) bool {
	if s.users == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("accounts are not available"))
		return false
	}
	return true
}

// AdoptAnonymousChats hands conversations written before authentication existed
// to the oldest account, so upgrading an install does not strand its history
// behind a login. It is idempotent: once the rows have moved there is nothing
// left under the anonymous owner, and on an unclaimed install there is nobody
// to move them to yet, so it does nothing.
func AdoptAnonymousChats(ctx context.Context, users *auth.Store, chats *chat.Store) (int64, error) {
	if users == nil || chats == nil {
		return 0, nil
	}
	owner, err := users.First(ctx)
	if errors.Is(err, auth.ErrNoUsers) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return chats.ReassignOwner(ctx, chat.AnonymousUser, owner.ID)
}

/* ---------- invites (admin only) ---------- */

// errForbidden is the only thing a non-admin caller learns from an admin
// endpoint: that they may not, and nothing about who can.
var errForbidden = errors.New("admin only")

// requireAdmin writes the standard response and returns false when the caller
// is not the admin. Authorization is re-checked against the store rather than
// trusted off the session's context, so a revoked or downgraded session cannot
// authorize an admin action even if its context still reads admin.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (*auth.User, bool) {
	u, ok := userFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, errUnauthenticated)
		return nil, false
	}
	isAdmin, err := s.users.IsAdmin(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return nil, false
	}
	if !isAdmin {
		writeError(w, http.StatusForbidden, errForbidden)
		return nil, false
	}
	return u, true
}

// inviteView is the JSON shape for an invite returned to the admin. Code is the
// raw secret and is present only on creation; the stored list never carries it.
type inviteView struct {
	ID        string    `json:"id"`
	Code      string    `json:"code,omitempty"`
	URL       string    `json:"url,omitempty"`
	Status    string    `json:"status"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	UsedAt    time.Time `json:"used_at,omitempty"`
}

func inviteStatus(inv auth.Invite) string {
	switch {
	case inv.Used():
		return "used"
	case inv.Expired():
		return "expired"
	default:
		return "pending"
	}
}

// inviteURL renders the link the admin copies and the invitee opens. It is
// built against the request's host so it works behind whatever proxy fronts
// the app; the code rides in the query string so the signed-out gate reads it.
func inviteURL(r *http.Request, code string) string {
	scheme := "http"
	if secureCookies(r) {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/?invite=%s", scheme, r.Host, code)
}

// handleCreateInvite mints a new single-use invite. The raw code is returned
// here and here only — the database keeps a digest, so this response is the
// admin's one chance to copy the link.
func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // note is optional; an empty body is fine

	inv, err := s.users.CreateInvite(r.Context(), u.ID, req.Note)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"invite": toInviteView(r, *inv)})
}

// handleListInvites returns every invite the admin has minted, newest first, so
// the invite manager can show which links are pending, spent, or expired.
func (s *Server) handleListInvites(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	invites, err := s.users.ListInvites(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]inviteView, 0, len(invites))
	for _, inv := range invites {
		views = append(views, toInviteView(r, inv))
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": views})
}

// handleRevokeInvite deletes an invite. Revoking a pending link closes it
// immediately; revoking a spent one just trims the list. An unknown id is not
// an error: the caller wanted it gone, and it is.
func (s *Server) handleRevokeInvite(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if err := s.users.RevokeInvite(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func toInviteView(r *http.Request, inv auth.Invite) inviteView {
	v := inviteView{
		ID:        inv.ID,
		Status:    inviteStatus(inv),
		Note:      inv.Note,
		CreatedAt: inv.CreatedAt,
	}
	// The raw code is only set on a freshly created invite (CreateInvite
	// populates it; the stored row never does). Build the link once, here.
	if inv.Code != "" {
		v.Code = inv.Code
		v.URL = inviteURL(r, inv.Code)
	}
	if !inv.ExpiresAt.IsZero() {
		v.ExpiresAt = inv.ExpiresAt
	}
	if !inv.UsedAt.IsZero() {
		v.UsedAt = inv.UsedAt
	}
	return v
}
