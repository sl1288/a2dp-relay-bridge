package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// fakeHA answers the token exchange and auth/current_user like Home Assistant.
func fakeHA(t *testing.T, admin bool) (*httptest.Server, *[]string) {
	var revoked []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("action") == "revoke" {
			revoked = append(revoked, r.Form.Get("token"))
			return
		}
		if r.Form.Get("code") != "good" || !strings.HasPrefix(r.Form.Get("client_id"), "https://relay.example/") {
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"access_token": "acc", "refresh_token": "ref", "token_type": "Bearer"})
	})
	mux.HandleFunc("/api/websocket", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		_ = wsjson.Write(ctx, c, map[string]any{"type": "auth_required"})
		var m map[string]any
		_ = wsjson.Read(ctx, c, &m)
		if m["access_token"] != "acc" {
			_ = wsjson.Write(ctx, c, map[string]any{"type": "auth_invalid"})
			return
		}
		_ = wsjson.Write(ctx, c, map[string]any{"type": "auth_ok"})
		_ = wsjson.Read(ctx, c, &m)
		_ = wsjson.Write(ctx, c, map[string]any{"id": 1, "type": "result", "success": true,
			"result": map[string]any{"name": "Tester", "is_admin": admin}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &revoked
}

func newTestAuth(ha string) http.Handler {
	a := newAuth(AuthConfig{HomeAssistant: ha, AdminOnly: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return a.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secret") }))
}

func get(h http.Handler, path string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.Host = "relay.example"
	r.Header.Set("X-Forwarded-Proto", "https")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestLoginFlow(t *testing.T) {
	ha, revoked := fakeHA(t, true)
	h := newTestAuth(ha.URL)

	if w := get(h, "/api/status", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("API without login: %d", w.Code)
	}
	w := get(h, "/index.html", nil)
	if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), "/auth/login") {
		t.Fatalf("page without login: %d %s", w.Code, w.Header().Get("Location"))
	}

	w = get(h, "/auth/login?next=/index.html", nil)
	loc, _ := url.Parse(w.Header().Get("Location"))
	q := loc.Query()
	if !strings.HasPrefix(loc.String(), ha.URL+"/auth/authorize") || q.Get("client_id") != "https://relay.example/" ||
		q.Get("redirect_uri") != "https://relay.example/auth/callback" {
		t.Fatalf("authorize redirect %s", loc)
	}
	state := q.Get("state")
	loginCookies := w.Result().Cookies()

	w = get(h, "/auth/callback?code=good&state="+state, loginCookies)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/index.html" {
		t.Fatalf("callback: %d %s %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	var session []*http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			if !c.Secure || !c.HttpOnly {
				t.Error("session cookie not secure/http-only")
			}
			session = append(session, c)
		}
	}
	if w := get(h, "/api/status", session); w.Body.String() != "secret" {
		t.Fatalf("API with session: %d %s", w.Code, w.Body.String())
	}
	if len(*revoked) != 1 || (*revoked)[0] != "ref" {
		t.Errorf("refresh token not revoked: %v", *revoked)
	}
	// A replayed state is refused.
	if w := get(h, "/auth/callback?code=good&state="+state, loginCookies); w.Code != http.StatusBadRequest {
		t.Errorf("replayed login: %d", w.Code)
	}
	get(h, "/auth/logout", session)
	if w := get(h, "/api/status", session); w.Code != http.StatusUnauthorized {
		t.Errorf("API after logout: %d", w.Code)
	}
}

func TestSessionSurvivesRestart(t *testing.T) {
	ha, _ := fakeHA(t, true)
	file := filepath.Join(t.TempDir(), "sessions.json")
	start := func() http.Handler {
		a := newAuth(AuthConfig{HomeAssistant: ha.URL, AdminOnly: true, SessionFile: file}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		return a.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secret") }))
	}
	h := start()
	w := get(h, "/auth/login", nil)
	loc, _ := url.Parse(w.Header().Get("Location"))
	w = get(h, "/auth/callback?code=good&state="+loc.Query().Get("state"), w.Result().Cookies())
	session := w.Result().Cookies()

	data, _ := os.ReadFile(file)
	for _, c := range session {
		if c.Name == sessionCookie && strings.Contains(string(data), c.Value) {
			t.Error("session file contains the cookie value")
		}
	}
	h = start()
	if w := get(h, "/api/status", session); w.Body.String() != "secret" {
		t.Fatalf("session lost after restart: %d", w.Code)
	}
	get(h, "/auth/logout", session)
	if w := get(start(), "/api/status", session); w.Code != http.StatusUnauthorized {
		t.Errorf("logout not persisted: %d", w.Code)
	}
}

func TestLoginRefusesNonAdmin(t *testing.T) {
	ha, _ := fakeHA(t, false)
	h := newTestAuth(ha.URL)
	w := get(h, "/auth/login", nil)
	loc, _ := url.Parse(w.Header().Get("Location"))
	w = get(h, "/auth/callback?code=good&state="+loc.Query().Get("state"), w.Result().Cookies())
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin login: %d", w.Code)
	}
}
