package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Login through Home Assistant. Home Assistant accepts any web page as an
// OAuth2 client whose redirect URI has the same host as its client ID
// (IndieAuth), so no registration is needed: the bridge sends the browser to
// Home Assistant's login, exchanges the code for a token, asks who logged in
// and keeps its own session cookie. The Home Assistant token is revoked right
// away; the bridge never acts on behalf of the user.

const (
	sessionCookie = "a2dp_relay_session"
	stateCookie   = "a2dp_relay_login"
	sessionTTL    = 30 * 24 * time.Hour
	loginTTL      = 10 * time.Minute
)

// AuthConfig enables the login; HomeAssistant is the URL users log in at.
// SessionFile keeps logins across restarts (empty: in memory only).
type AuthConfig struct {
	HomeAssistant string
	AdminOnly     bool
	SessionFile   string
}

type session struct {
	User    string    `json:"user"`
	Expires time.Time `json:"expires"`
}

type pendingLogin struct {
	next     string
	clientID string
	expires  time.Time
}

type auth struct {
	cfg  AuthConfig
	log  *slog.Logger
	http *http.Client

	mu       sync.Mutex
	sessions map[string]session // by sessionKey of the cookie value
	pending  map[string]pendingLogin
}

func newAuth(cfg AuthConfig, log *slog.Logger) *auth {
	cfg.HomeAssistant = strings.TrimRight(cfg.HomeAssistant, "/")
	a := &auth{cfg: cfg, log: log.With("component", "auth"), http: &http.Client{Timeout: 10 * time.Second},
		sessions: map[string]session{}, pending: map[string]pendingLogin{}}
	a.loadSessions()
	return a
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sessionKey is what the bridge stores for a session cookie: a hash, so the
// session file does not contain usable cookies.
func sessionKey(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func (a *auth) loadSessions() {
	if a.cfg.SessionFile == "" {
		return
	}
	data, err := os.ReadFile(a.cfg.SessionFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			a.log.Warn("reading sessions failed", "err", err)
		}
		return
	}
	if err := json.Unmarshal(data, &a.sessions); err != nil {
		a.log.Warn("reading sessions failed", "err", err)
		a.sessions = map[string]session{}
		return
	}
	now := time.Now()
	for k, s := range a.sessions {
		if now.After(s.Expires) {
			delete(a.sessions, k)
		}
	}
}

// saveSessions writes the sessions; a.mu must be held.
func (a *auth) saveSessions() {
	if a.cfg.SessionFile == "" {
		return
	}
	data, _ := json.Marshal(a.sessions)
	tmp := a.cfg.SessionFile + ".tmp"
	err := os.WriteFile(tmp, data, 0o600)
	if err == nil {
		err = os.Rename(tmp, a.cfg.SessionFile)
	}
	if err != nil {
		a.log.Warn("saving sessions failed", "err", err)
	}
}

// origin is the address the browser uses for the bridge (behind a reverse
// proxy: its forwarded host and scheme).
func origin(r *http.Request) (scheme, host string) {
	scheme = "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host = r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = strings.TrimSpace(strings.Split(fh, ",")[0])
	}
	return scheme, host
}

func (a *auth) user(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := sessionKey(c.Value)
	s, ok := a.sessions[key]
	if !ok {
		return "", false
	}
	if time.Now().After(s.Expires) {
		delete(a.sessions, key)
		a.saveSessions()
		return "", false
	}
	return s.User, true
}

// wrap protects every route except the login itself.
func (a *auth) wrap(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", a.login)
	mux.HandleFunc("GET /auth/callback", a.callback)
	mux.HandleFunc("GET /auth/logout", a.logout)
	mux.HandleFunc("GET /api/whoami", func(w http.ResponseWriter, r *http.Request) {
		u, _ := a.user(r)
		writeJSON(w, http.StatusOK, map[string]string{"user": u})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/auth/") {
			mux.ServeHTTP(w, r)
			return
		}
		if _, ok := a.user(r); !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeErr(w, http.StatusUnauthorized, errors.New("login required"))
				return
			}
			http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		if r.URL.Path == "/api/whoami" {
			mux.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *auth) login(w http.ResponseWriter, r *http.Request) {
	scheme, host := origin(r)
	clientID := scheme + "://" + host + "/"
	next := r.URL.Query().Get("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	state := randomToken()
	a.mu.Lock()
	now := time.Now()
	for k, p := range a.pending {
		if now.After(p.expires) {
			delete(a.pending, k)
		}
	}
	a.pending[state] = pendingLogin{next: next, clientID: clientID, expires: now.Add(loginTTL)}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: state, Path: "/auth/", HttpOnly: true,
		Secure: scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: int(loginTTL.Seconds())})
	q := url.Values{"response_type": {"code"}, "client_id": {clientID},
		"redirect_uri": {clientID + "auth/callback"}, "state": {state}}
	http.Redirect(w, r, a.cfg.HomeAssistant+"/auth/authorize?"+q.Encode(), http.StatusFound)
}

func (a *auth) callback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	c, err := r.Cookie(stateCookie)
	a.mu.Lock()
	p, ok := a.pending[state]
	delete(a.pending, state)
	a.mu.Unlock()
	if err != nil || c.Value != state || !ok || time.Now().After(p.expires) {
		a.page(w, http.StatusBadRequest, "Anmeldung abgelaufen oder ungültig. <a href=\"/\">Erneut versuchen</a>")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		a.page(w, http.StatusBadRequest, "Home Assistant hat die Anmeldung abgebrochen. <a href=\"/\">Erneut versuchen</a>")
		return
	}
	user, admin, err := a.verify(r.Context(), code, p.clientID)
	if err != nil {
		a.log.Warn("login failed", "err", err)
		a.page(w, http.StatusBadGateway, "Anmeldung bei Home Assistant fehlgeschlagen: "+html.EscapeString(err.Error()))
		return
	}
	if a.cfg.AdminOnly && !admin {
		a.log.Warn("login refused: not an administrator", "user", user)
		a.page(w, http.StatusForbidden, "Nur Home-Assistant-Administratoren haben Zugriff. <a href=\"/auth/logout\">Abmelden</a>")
		return
	}
	token := randomToken()
	a.mu.Lock()
	a.sessions[sessionKey(token)] = session{User: user, Expires: time.Now().Add(sessionTTL)}
	a.saveSessions()
	a.mu.Unlock()
	scheme, _ := origin(r)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds())})
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/auth/", MaxAge: -1})
	a.log.Info("logged in", "user", user)
	http.Redirect(w, r, p.next, http.StatusFound)
}

func (a *auth) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.mu.Lock()
		delete(a.sessions, sessionKey(c.Value))
		a.saveSessions()
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	a.page(w, http.StatusOK, "Abgemeldet. <a href=\"/\">Wieder anmelden</a>")
}

func (a *auth) page(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">`+
		`<title>A2DP Relay</title><body style="font-family:system-ui;padding:2em">%s</body>`, body)
}

// verify exchanges the code and asks Home Assistant who logged in.
func (a *auth) verify(ctx context.Context, code, clientID string) (user string, admin bool, err error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}}
	resp, err := a.http.PostForm(a.cfg.HomeAssistant+"/auth/token", form)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || resp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		return "", false, fmt.Errorf("token exchange failed (HTTP %d) %s", resp.StatusCode, tok.Error)
	}
	defer func() {
		// The bridge keeps its own session: revoke the refresh token so that
		// no login lingers in Home Assistant.
		if tok.RefreshToken != "" {
			if r, err := a.http.PostForm(a.cfg.HomeAssistant+"/auth/token",
				url.Values{"action": {"revoke"}, "token": {tok.RefreshToken}}); err == nil {
				r.Body.Close()
			}
		}
	}()

	wsURL := "ws" + strings.TrimPrefix(a.cfg.HomeAssistant, "http") + "/api/websocket"
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(cctx, wsURL, nil)
	if err != nil {
		return "", false, fmt.Errorf("websocket: %w", err)
	}
	defer conn.CloseNow()
	var msg map[string]any
	if err := wsjson.Read(cctx, conn, &msg); err != nil { // auth_required
		return "", false, err
	}
	if err := wsjson.Write(cctx, conn, map[string]any{"type": "auth", "access_token": tok.AccessToken}); err != nil {
		return "", false, err
	}
	if err := wsjson.Read(cctx, conn, &msg); err != nil || msg["type"] != "auth_ok" {
		return "", false, fmt.Errorf("websocket authentication failed: %v", msg["type"])
	}
	if err := wsjson.Write(cctx, conn, map[string]any{"id": 1, "type": "auth/current_user"}); err != nil {
		return "", false, err
	}
	var res struct {
		Success bool `json:"success"`
		Result  struct {
			Name    string `json:"name"`
			IsAdmin bool   `json:"is_admin"`
		} `json:"result"`
	}
	if err := wsjson.Read(cctx, conn, &res); err != nil || !res.Success {
		return "", false, errors.New("could not read the user")
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	return res.Result.Name, res.Result.IsAdmin, nil
}
