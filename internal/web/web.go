// Package web serves the Oscar UI and its JSON API.
package web

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"oscar/internal/engine"
)

type Server struct {
	e      *engine.Engine
	static fs.FS
}

func New(e *engine.Engine, static fs.FS) *Server { return &Server{e: e, static: static} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("POST /api/chat", s.chat)
	mux.HandleFunc("GET /api/tasks/{id}/log", s.log)
	mux.HandleFunc("GET /api/tasks/{id}/diff", s.diff)
	mux.HandleFunc("POST /api/tasks/{id}/decide", s.decide)
	mux.HandleFunc("POST /api/tasks/{id}/message", s.message)
	mux.HandleFunc("POST /api/tasks/{id}/cancel", s.cancel)
	mux.HandleFunc("POST /api/tasks/{id}/outbox/{oid}", s.editOutbox)
	mux.HandleFunc("POST /api/captain/decide", s.decideCaptain)
	mux.HandleFunc("POST /api/self/update", s.selfUpdate)
	mux.HandleFunc("GET /api/allow", s.allowList)
	mux.HandleFunc("POST /api/allow/remove", s.allowRemove)
	mux.Handle("GET /", http.FileServerFS(s.static))
	return sameOrigin(mux)
}

// sameOrigin blocks cross-site POSTs (a page on another site cannot drive Oscar).
func sameOrigin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if o := r.Header.Get("Origin"); o != "" && !strings.HasSuffix(o, "://"+r.Host) {
				http.Error(w, "cross-origin request blocked", http.StatusForbidden)
				return
			}
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				http.Error(w, "json only", http.StatusUnsupportedMediaType)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func read(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v)
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.e.Snapshot()) }

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, stop := s.e.Subscribe()
	defer stop()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	_, _ = w.Write([]byte(": hi\n\n"))
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			fl.Flush()
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			fl.Flush()
		}
	}
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var a struct{ Text string }
	if err := read(w, r, &a); err != nil {
		fail(w, err)
		return
	}
	go func() { _ = s.e.Chat(a.Text) }()
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) log(w http.ResponseWriter, r *http.Request) {
	l, err := s.e.Log(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, l)
}

func (s *Server) diff(w http.ResponseWriter, r *http.Request) {
	d, err := s.e.Diff(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]string{"diff": d})
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	var a struct {
		PendingID string `json:"pending_id"`
		engine.Decision
	}
	if err := read(w, r, &a); err != nil {
		fail(w, err)
		return
	}
	if err := s.e.Decide(r.PathValue("id"), a.PendingID, a.Decision); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) decideCaptain(w http.ResponseWriter, r *http.Request) {
	var a struct {
		PendingID string `json:"pending_id"`
		engine.Decision
	}
	if err := read(w, r, &a); err != nil {
		fail(w, err)
		return
	}
	if err := s.e.Decide("", a.PendingID, a.Decision); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) message(w http.ResponseWriter, r *http.Request) {
	var a struct{ Text string }
	if err := read(w, r, &a); err != nil {
		fail(w, err)
		return
	}
	if err := s.e.MessageWorker(r.PathValue("id"), a.Text); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if err := s.e.Cancel(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) editOutbox(w http.ResponseWriter, r *http.Request) {
	var a struct{ Text string }
	if err := read(w, r, &a); err != nil {
		fail(w, err)
		return
	}
	if err := s.e.EditOutbox(r.PathValue("id"), r.PathValue("oid"), a.Text); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) selfUpdate(w http.ResponseWriter, r *http.Request) {
	var a struct {
		TaskID string `json:"task_id"`
	}
	if err := read(w, r, &a); err != nil {
		fail(w, err)
		return
	}
	if err := s.e.SelfUpdate(a.TaskID); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) allowList(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.e.AllowRules()) }

func (s *Server) allowRemove(w http.ResponseWriter, r *http.Request) {
	var a struct{ Kind, Subject string }
	if err := read(w, r, &a); err != nil {
		fail(w, err)
		return
	}
	if err := s.e.RemoveAllowRule(a.Kind, a.Subject); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
