package dialer

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/enetx/g"
)

type Server struct {
	cfg          Config
	mu           sync.Mutex
	calls        g.Map[g.String, *call]
	active       *call
	shuttingDown bool
	http         *http.Client
	telnyxURL    g.String
	liveURL      g.String
}

func NewServer(cfg Config) g.Result[*Server] {
	if err := cfg.validate(); err != nil {
		return g.Err[*Server](err)
	}
	if err := os.MkdirAll(cfg.StateDir.Std(), 0700); err != nil {
		return g.Err[*Server](err)
	}
	st, err := os.Stat(cfg.StateDir.Std())
	if err != nil {
		return g.Err[*Server](err)
	}
	if st.Mode().Perm()&0077 != 0 {
		return g.Err[*Server](errors.New("state_dir must have permissions 700"))
	}
	s := &Server{cfg: cfg, calls: g.NewMap[g.String, *call](), http: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, telnyxURL: "https://api.telnyx.com/v2", liveURL: "wss://api.openai.com/v1/live/sessions"}
	b, err := os.ReadFile(s.activePath().Std())
	if err == nil {
		stored := s.loadReceipt(g.String(b))
		if stored.IsErr() {
			return g.Err[*Server](stored.Err())
		}
		c := newCall(s, Request{}, stored.Unwrap())
		s.calls[c.r.Snapshot.ID] = c
		if terminal(c.r.Snapshot.Status) {
			if err = os.Remove(s.activePath().Std()); err != nil {
				return g.Err[*Server](err)
			}
		} else {
			c.r.Snapshot.Status = "recovery_required"
			c.r.Snapshot.LiveConnected = false
			c.r.Snapshot.MediaConnected = false
			c.r.Snapshot.Question = nil
			c.stopRequested = true
			s.active = c
			if err = c.saveLocked(); err != nil {
				return g.Err[*Server](err)
			}
		}
	} else if !os.IsNotExist(err) {
		return g.Err[*Server](err)
	}
	return g.Ok(s)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, g.Map[g.String, any]{"status": "ok"}) })
	mux.HandleFunc("POST /webhooks/telnyx", s.webhook)
	mux.HandleFunc("GET /media/{id}", s.media)
	mux.Handle("/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		provided := g.String(r.Header.Get("Authorization")).StripPrefix("Bearer ")
		if subtle.ConstantTimeCompare(provided.Bytes(), s.cfg.APIToken.Bytes()) != 1 {
			respond(w, 401, g.Map[g.String, any]{"error": "unauthorized"})
			return
		}
		s.operator(w, r)
	}))
	return mux
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func failHTTP(w http.ResponseWriter, status int, err error) {
	respond(w, status, g.Map[g.String, any]{"error": err.Error()})
}

func (s *Server) operator(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	if r.URL.Path == "/v1/calls" && r.Method == http.MethodPost {
		var task Request
		if err := decode(r.Body, &task); err != nil {
			failHTTP(w, 400, err)
			return
		}
		result := s.start(task)
		if result.IsErr() {
			failHTTP(w, 409, result.Err())
			return
		}
		respond(w, 202, result.Unwrap())
		return
	}
	parts := g.Slice[g.String](g.String(r.URL.Path).StripPrefix("/v1/calls/").Split("/"))
	if len(parts) < 1 || len(parts) > 2 || !idPattern.MatchString(parts[0].Std()) {
		http.NotFound(w, r)
		return
	}
	found := s.lookup(parts[0])
	if found.IsErr() {
		failHTTP(w, 404, errors.New("call not found"))
		return
	}
	c := found.Unwrap()
	if len(parts) == 1 && r.Method == http.MethodGet {
		respond(w, 200, c.snapshot())
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var err error
	switch parts[1] {
	case "reply":
		var body struct {
			QuestionID g.String `json:"question_id"`
			Answer     g.String `json:"answer"`
		}
		err = decode(r.Body, &body)
		if err == nil {
			err = c.reply(body.QuestionID, body.Answer).Err()
		}
	case "dtmf":
		var body struct {
			Digits    g.String `json:"digits"`
			Duration  g.Int    `json:"duration_millis"`
			CommandID g.String `json:"command_id"`
		}
		err = decode(r.Body, &body)
		if err == nil {
			if !idPattern.MatchString(body.CommandID.Std()) {
				err = errors.New("command_id must be a stable 8-80 character id")
			} else {
				result := c.dtmf("manual_"+body.CommandID, body.Digits, body.Duration)
				if result.IsErr() {
					err = result.Err()
				} else {
					respond(w, 200, result.Unwrap())
					return
				}
			}
		}
	case "hangup":
		err = c.stop("operator requested hangup")
	case "reconcile":
		err = c.reconcile()
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		failHTTP(w, 409, err)
		return
	}
	respond(w, 200, c.snapshot())
}

func (s *Server) lookup(id g.String) g.Result[*call] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.calls.Get(id); c.IsSome() {
		return g.Ok(c.Unwrap())
	}
	r := s.loadReceipt(id)
	if r.IsErr() {
		return g.Err[*call](r.Err())
	}
	c := newCall(s, Request{}, r.Unwrap())
	s.calls[id] = c
	return g.Ok(c)
}

func (s *Server) start(task Request) g.Result[Snapshot] {
	if v := task.Validate(); v.IsErr() {
		return g.Err[Snapshot](v.Err())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := fingerprint(task)
	if existing := s.calls.Get(task.ID); existing.IsSome() {
		c := existing.Unwrap()
		c.mu.Lock()
		same := c.r.RequestHash == hash
		c.mu.Unlock()
		if !same {
			return g.Err[Snapshot](errors.New("request id already belongs to different input"))
		}
		return g.Ok(c.snapshot())
	}
	if stored := s.loadReceipt(task.ID); stored.IsOk() {
		if stored.Unwrap().RequestHash != hash {
			return g.Err[Snapshot](errors.New("request id already belongs to different input"))
		}
		return g.Ok(stored.Unwrap().Snapshot)
	} else if !os.IsNotExist(stored.Err()) {
		return g.Err[Snapshot](stored.Err())
	}
	if s.shuttingDown {
		return g.Err[Snapshot](errors.New("service is shutting down; no new calls accepted"))
	}
	if err := s.reserve(task.ID); err != nil {
		return g.Err[Snapshot](err)
	}
	now := time.Now().UTC()
	c := newCall(s, task, receipt{RequestHash: hash, Snapshot: Snapshot{ID: task.ID, To: task.To, Status: "starting", StartedAt: now, UpdatedAt: now, Events: g.Slice[Event]{}, Transcript: g.Slice[Transcript]{}}, Commands: g.NewMap[g.String, commandRecord]()})
	if err := c.saveLocked(); err != nil {
		_ = os.Remove(s.activePath().Std())
		return g.Err[Snapshot](err)
	}
	s.calls[task.ID] = c
	s.active = c
	go c.run()
	return g.Ok(c.snapshot())
}

type webhookEvent struct {
	Data struct {
		ID      g.String `json:"id"`
		Type    g.String `json:"event_type"`
		Payload struct {
			ControlID    g.String `json:"call_control_id"`
			ClientState  g.String `json:"client_state"`
			ConnectionID g.String `json:"connection_id"`
			From         g.String `json:"from"`
			To           g.String `json:"to"`
			HangupCause  g.String `json:"hangup_cause"`
			StreamURL    g.String `json:"stream_url"`
		} `json:"payload"`
	} `json:"data"`
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 65536))
	if err != nil {
		failHTTP(w, 400, errors.New("webhook too large"))
		return
	}
	if !verifySignature(s.cfg.TelnyxPublicKey, r.Header, b, time.Now()) {
		failHTTP(w, 401, errors.New("invalid Telnyx signature"))
		return
	}
	var e webhookEvent
	if json.Unmarshal(b, &e) != nil || e.Data.ID.IsEmpty() {
		failHTTP(w, 400, errors.New("invalid webhook"))
		return
	}
	var c *call
	p := e.Data.Payload
	if !p.ClientState.IsEmpty() {
		id, decodeErr := base64.StdEncoding.DecodeString(p.ClientState.Std())
		if decodeErr == nil && idPattern.Match(id) {
			if got := s.lookup(g.String(id)); got.IsOk() {
				c = got.Unwrap()
			}
		}
	} else {
		s.mu.Lock()
		active := s.active
		s.mu.Unlock()
		if active != nil {
			active.mu.Lock()
			matches := (!active.r.ControlID.IsEmpty() && active.r.ControlID == p.ControlID) || (!p.StreamURL.IsEmpty() && p.StreamURL == active.streamURL())
			active.mu.Unlock()
			if matches {
				c = active
			}
		}
	}
	if c == nil {
		respond(w, 200, g.Map[g.String, any]{"ignored": true})
		return
	}
	if err = c.webhook(e); err != nil {
		failHTTP(w, 500, err)
		return
	}
	respond(w, 200, g.Map[g.String, any]{"received": true})
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shuttingDown = true
	c := s.active
	s.mu.Unlock()
	if c == nil {
		return nil
	}
	result := make(chan error, 1)
	go func() { result <- c.stop("service shutdown") }()
	select {
	case err := <-result:
		c.finalize()
		select {
		case <-c.ctx.Done():
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		c.finalize()
		return ctx.Err()
	}
}
