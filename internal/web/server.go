// Package web serves the bridge's web interface: status, pairing and
// headphone management.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/sl1288/a2dp-relay-bridge/internal/bridge"
)

//go:embed static
var static embed.FS

// Server is the HTTP server of the web interface.
type Server struct {
	b       *bridge.Bridge
	log     *slog.Logger
	mux     *http.ServeMux
	handler http.Handler
}

// New creates the web interface for b. With ac.HomeAssistant set, every page
// needs a login through Home Assistant.
func New(b *bridge.Bridge, log *slog.Logger, ac AuthConfig) *Server {
	s := &Server{b: b, log: log, mux: http.NewServeMux()}
	s.handler = s.mux
	if ac.HomeAssistant != "" {
		s.handler = newAuth(ac, log).wrap(s.mux)
	}
	sub, _ := fs.Sub(static, "static")
	s.mux.Handle("GET /", http.FileServerFS(sub))
	s.mux.HandleFunc("GET /api/status", s.status)
	s.mux.HandleFunc("GET /api/events", s.events)
	s.mux.HandleFunc("POST /api/inquiry", s.inquiry)
	s.mux.HandleFunc("POST /api/ble-discovery", s.bleDiscovery)
	s.mux.HandleFunc("POST /api/pair", s.pair)
	s.mux.HandleFunc("POST /api/headphones/{id}/rename", s.rename)
	s.mux.HandleFunc("POST /api/headphones/{id}/ble", s.setBLE)
	s.mux.HandleFunc("POST /api/headphones/{id}/ble-pair", s.blePair)
	s.mux.HandleFunc("POST /api/headphones/{id}/ble-irk", s.setIRK)
	s.mux.HandleFunc("DELETE /api/headphones/{id}/ble-irk", s.clearIRK)
	s.mux.HandleFunc("POST /api/headphones/{id}/codec", s.setCodec)
	s.mux.HandleFunc("POST /api/headphones/{id}/connect", s.connect)
	s.mux.HandleFunc("POST /api/headphones/{id}/disconnect", s.disconnect)
	s.mux.HandleFunc("DELETE /api/headphones/{id}", s.forget)
	return s
}

// ListenAndServe serves until ctx ends.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	s.log.Info("web interface listening", "addr", addr)
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr answers with the error; a bridge.CodedError also carries its code
// and arguments, which the web interface translates.
func writeErr(w http.ResponseWriter, code int, err error) {
	body := map[string]any{"error": err.Error()}
	var ce *bridge.CodedError
	if errors.As(err, &ce) {
		body["code"], body["args"] = ce.Code, ce.Args
	}
	writeJSON(w, code, body)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.b.Snapshot())
}

// events streams the status as server-sent events on every change, at most
// four times per second and at least every two seconds.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	changes, unsubscribe := s.b.Subscribe()
	defer unsubscribe()
	send := func() bool {
		data, err := json.Marshal(s.b.Snapshot())
		if err != nil {
			return false
		}
		if _, err := w.Write(append(append([]byte("data: "), data...), '\n', '\n')); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send() {
		return
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-changes:
			time.Sleep(250 * time.Millisecond) // coalesce bursts
		case <-tick.C:
		}
		if !send() {
			return
		}
	}
}

func (s *Server) inquiry(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Node    string `json:"node"`
		Seconds int    `json:"seconds"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Seconds <= 0 {
		req.Seconds = 12
	}
	if err := s.b.StartInquiry(req.Node, req.Seconds); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) bleDiscovery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Node    string `json:"node"`
		Seconds int    `json:"seconds"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Seconds <= 0 {
		req.Seconds = 15
	}
	if err := s.b.StartBLEDiscovery(req.Node, time.Duration(req.Seconds)*time.Second); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Node string `json:"node"`
		Addr string `json:"addr"`
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	h, err := s.b.Pair(r.Context(), req.Node, req.Addr, req.Name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) rename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.b.Rename(r.PathValue("id"), req.Name); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) setBLE(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Addr string `json:"addr"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.b.SetBLEIdentity(r.PathValue("id"), req.Name, req.Addr); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// blePair learns the headphone's identity key by pairing over BLE.
func (s *Server) blePair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Node     string `json:"node"`
		Addr     string `json:"addr"`
		AddrType uint8  `json:"addr_type"`
	}
	if !decode(w, r, &req) {
		return
	}
	if _, err := s.b.BLEPair(r.Context(), r.PathValue("id"), req.Node, req.Addr, req.AddrType); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// setIRK stores an identity key copied from another system.
func (s *Server) setIRK(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IRK string `json:"irk"`
	}
	if !decode(w, r, &req) {
		return
	}
	matched, err := s.b.SetBLEIRK(r.PathValue("id"), req.IRK)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "matched": matched})
}

func (s *Server) clearIRK(w http.ResponseWriter, r *http.Request) {
	if err := s.b.ClearBLEIRK(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) setCodec(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Codec       string `json:"codec"`
		LDACQuality string `json:"ldac_quality"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.b.SetCodec(r.PathValue("id"), req.Codec, req.LDACQuality); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Node string `json:"node"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.b.Connect(r.PathValue("id"), req.Node); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) disconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.b.Disconnect(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) forget(w http.ResponseWriter, r *http.Request) {
	if err := s.b.Forget(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
