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
// handshake it uses to decide between the login and setup forms, and the two
// ways of getting a session in the first place.
var openAPIPaths = map[string]bool{
	"/api/auth/state": true,
	"/api/auth/login": true,
	"/api/auth/setup": true,
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
// thin: an unclaimed install is the only thing a signed-out caller learns.
type authStateView struct {
	Authenticated    bool   `json:"authenticated"`
	Username         string `json:"username,omitempty"`
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
