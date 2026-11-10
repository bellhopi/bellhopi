package dialer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/enetx/g"
)

func testConfig(t *testing.T) (Config, ed25519.PrivateKey) {
	t.Helper()
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return Config{OpenAIKey: "fake-openai-key", TelnyxKey: "fake-telnyx-key", TelnyxPublicKey: g.String(base64.StdEncoding.EncodeToString(pub)), ConnectionID: "test-connection", From: "+15555550100", PublicURL: "https://calls.example.test", APIToken: "01234567890123456789012345678901", Listen: "127.0.0.1:8787", StateDir: g.String(dir), LiveModel: "gpt-live-1", BackendModel: "gpt-6-luna", Voice: "marin", MaxCallSeconds: 30}, private
}
func signed(key ed25519.PrivateKey, payload []byte) http.Header {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := ed25519.Sign(key, append([]byte(ts+"|"), payload...))
	return http.Header{"Telnyx-Timestamp": []string{ts}, "Telnyx-Signature-Ed25519": []string{base64.StdEncoding.EncodeToString(sig)}}
}
func await(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition timed out")
}
func request() Request {
	return Request{ID: "task-00000001", To: "+15555550101", Task: "Call the business, navigate to billing and ask its opening hours.", Information: "Private test information must not be persisted."}
}

func TestValidationAndSignatures(t *testing.T) {
	cfg, key := testConfig(t)
	body := []byte(`{"example":true}`)
	headers := signed(key, body)
	if !verifySignature(cfg.TelnyxPublicKey, headers, body, time.Now()) {
		t.Fatal("valid signature rejected")
	}
	if verifySignature(cfg.TelnyxPublicKey, headers, []byte(`{"example":false}`), time.Now()) {
		t.Fatal("tampering accepted")
	}
	if verifySignature(cfg.TelnyxPublicKey, headers, body, time.Now().Add(6*time.Minute)) {
		t.Fatal("stale signature accepted")
	}
	for _, phone := range []g.String{"5551234", "+15555550101,123", "sip:example.test", "+15555550101\n"} {
		r := request()
		r.To = phone
		if r.Validate().IsOk() {
			t.Fatalf("accepted destination %q", phone)
		}
	}
	s := NewServer(cfg).Unwrap()
	c := newCall(s, request(), receipt{Commands: g.NewMap[g.String, commandRecord]()})
	for _, digits := range []g.String{"", "111,222", "wWWW", "1<script>", g.String("1").Repeat(33)} {
		if c.dtmf("command-0001", digits, 250).IsOk() {
			t.Fatalf("accepted DTMF %q", digits)
		}
	}
	for _, duration := range []g.Int{99, 501, -1} {
		if c.dtmf("command-0002", "1", duration).IsOk() {
			t.Fatalf("accepted duration %d", duration)
		}
	}
	r := httptest.NewRequest("POST", "/v1/calls", bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("missing auth accepted")
	}
}

type fakeProviders struct {
	mu           sync.Mutex
	voice        *websocket.Conn
	messages     chan g.Map[g.String, json.RawMessage]
	dials        int
	dtmfs        int
	hangups      int
	unknownDial  bool
	unknownDTMF  bool
	startupError bool
	dialBody     g.Map[g.String, any]
	onDial       func()
}

func (f *fakeProviders) counts() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials, f.dtmfs, f.hangups
}
func (f *fakeProviders) send(t *testing.T, v any) {
	t.Helper()
	f.mu.Lock()
	conn := f.voice
	f.mu.Unlock()
	if err := writeWS(context.Background(), conn, v); err != nil {
		t.Fatal(err)
	}
}
func (f *fakeProviders) next(t *testing.T, want g.String) g.Map[g.String, json.RawMessage] {
	t.Helper()
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	for {
		select {
		case msg := <-f.messages:
			var typ g.String
			_ = json.Unmarshal(msg["type"], &typ)
			if typ == want {
				return msg
			}
		case <-timer.C:
			t.Fatalf("no %s event", want)
			return nil
		}
	}
}
func fixture(t *testing.T, unknownDial, startupError bool) (*Server, *fakeProviders, ed25519.PrivateKey, *httptest.Server) {
	t.Helper()
	cfg, key := testConfig(t)
	s := NewServer(cfg).Unwrap()
	f := &fakeProviders{messages: make(chan g.Map[g.String, json.RawMessage], 128), unknownDial: unknownDial, startupError: startupError}
	voice := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-openai-key" {
			t.Error("wrong OpenAI authorization")
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		f.mu.Lock()
		f.voice = conn
		f.mu.Unlock()
		for {
			_, b, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var msg g.Map[g.String, json.RawMessage]
			if json.Unmarshal(b, &msg) != nil {
				return
			}
			f.messages <- msg
			var typ g.String
			_ = json.Unmarshal(msg["type"], &typ)
			switch typ {
			case "session.start":
				if f.startupError {
					_ = writeWS(r.Context(), conn, g.Map[g.String, any]{"type": "error", "error": g.Map[g.String, any]{"code": "responses_validation_failed"}})
				} else {
					_ = writeWS(r.Context(), conn, g.Map[g.String, any]{"type": "session.started"})
				}
			case "session.close":
				_ = writeWS(r.Context(), conn, g.Map[g.String, any]{"type": "session.closed", "usage": g.Map[g.String, any]{"seconds": 1}})
				return
			}
		}
	}))
	telnyx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-telnyx-key" {
			t.Error("wrong Telnyx authorization")
		}
		f.mu.Lock()
		onDial := f.onDial
		f.mu.Unlock()
		if r.URL.Path == "/calls" && r.Method == "POST" && onDial != nil {
			onDial()
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/calls" && r.Method == "POST":
			f.dials++
			_ = json.NewDecoder(r.Body).Decode(&f.dialBody)
			if f.unknownDial {
				respond(w, 503, g.Map[g.String, any]{"errors": g.Slice[any]{g.Map[g.String, any]{"code": "unknown"}}})
				return
			}
			respond(w, 200, g.Map[g.String, any]{"data": g.Map[g.String, any]{"call_control_id": "v2:test-call"}})
		case g.String(r.URL.Path).EndsWith("/send_dtmf"):
			f.dtmfs++
			if f.unknownDTMF {
				respond(w, 503, g.Map[g.String, any]{"errors": g.Slice[any]{g.Map[g.String, any]{"code": "unknown"}}})
				return
			}
			respond(w, 200, g.Map[g.String, any]{"data": g.Map[g.String, any]{"result": "ok"}})
		case g.String(r.URL.Path).EndsWith("/hangup"):
			f.hangups++
			respond(w, 200, g.Map[g.String, any]{"data": g.Map[g.String, any]{"result": "ok"}})
		case r.Method == "GET":
			respond(w, 200, g.Map[g.String, any]{"data": g.Map[g.String, any]{"is_alive": false}})
		default:
			http.NotFound(w, r)
		}
	}))
	s.telnyxURL = g.String(telnyx.URL)
	s.liveURL = g.String(voice.URL).Replace("http://", "ws://", 1)
	app := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		s.mu.Lock()
		for c := range s.calls.Iter().Values() {
			c.cancel()
			c.mu.Lock()
			if c.live != nil {
				c.live.CloseNow()
			}
			if c.mediaConn != nil {
				c.mediaConn.CloseNow()
			}
			c.mu.Unlock()
		}
		s.mu.Unlock()
		app.Close()
		voice.Close()
		telnyx.Close()
	})
	return s, f, key, app
}
func hook(t *testing.T, s *Server, key ed25519.PrivateKey, eventID, eventType g.String, task Request) int {
	t.Helper()
	b, _ := json.Marshal(g.Map[g.String, any]{"data": g.Map[g.String, any]{"id": eventID, "event_type": eventType, "payload": g.Map[g.String, any]{"call_control_id": "v2:test-call", "connection_id": s.cfg.ConnectionID, "from": s.cfg.From, "to": task.To, "client_state": base64.StdEncoding.EncodeToString(task.ID.Bytes())}}})
	r := httptest.NewRequest("POST", "/webhooks/telnyx", bytes.NewReader(b))
	r.Header = signed(key, b)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w.Code
}
func delegate(t *testing.T, f *fakeProviders, responseID, toolID, name, args g.String) {
	t.Helper()
	envelope := func(event any) g.Map[g.String, any] {
		return g.Map[g.String, any]{"type": "response.event", "delegation_id": "delegation-1", "event": event}
	}
	f.send(t, envelope(g.Map[g.String, any]{"type": "response.created", "response": g.Map[g.String, any]{"id": responseID}}))
	f.send(t, envelope(g.Map[g.String, any]{"type": "response.output_item.done", "item": g.Map[g.String, any]{"type": "function_call", "call_id": toolID, "name": name, "arguments": args}}))
	completed := envelope(g.Map[g.String, any]{"type": "response.completed", "response": g.Map[g.String, any]{"id": responseID, "output": g.Slice[any]{}}})
	f.send(t, completed)
	f.send(t, completed)
}

func TestCallAudioToolsAndReceipts(t *testing.T) {
	s, f, key, app := fixture(t, false, false)
	task := request()
	created := s.start(task)
	if created.IsErr() {
		t.Fatal(created.Err())
	}
	c := s.lookup(task.ID).Unwrap()
	start := f.next(t, "session.start")
	var session struct {
		Model g.String `json:"model"`
		Audio struct {
			Format struct {
				Type g.String `json:"type"`
				Rate g.Int    `json:"rate"`
			} `json:"format"`
		} `json:"audio"`
	}
	if json.Unmarshal(start["session"], &session) != nil || session.Model != "gpt-live-1" || session.Audio.Format.Type != "audio/pcmu" || session.Audio.Format.Rate != 8000 {
		t.Fatal("wrong Live handshake")
	}
	await(t, func() bool { return c.snapshot().Status == "ringing" })
	if s.start(task).IsErr() {
		t.Fatal("same request must be idempotent")
	}
	d, _, _ := f.counts()
	if d != 1 {
		t.Fatal("duplicate dial")
	}
	changed := task
	changed.Task = "Different request"
	if s.start(changed).IsOk() {
		t.Fatal("request id input collision accepted")
	}
	if hook(t, s, key, "answered-1", "call.answered", task) != 200 {
		t.Fatal("answered webhook rejected")
	}
	mediaURL := g.String(app.URL).Replace("http://", "ws://", 1) + "/media/" + task.ID
	conn, _, err := websocket.Dial(context.Background(), mediaURL.Std(), &websocket.DialOptions{HTTPHeader: signed(key, c.streamURL().Bytes())})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	err = writeWS(context.Background(), conn, g.Map[g.String, any]{"event": "start", "stream_id": "stream-1", "start": g.Map[g.String, any]{"call_control_id": "v2:test-call", "client_state": base64.StdEncoding.EncodeToString(task.ID.Bytes()), "from": s.cfg.From, "to": task.To, "media_format": g.Map[g.String, any]{"encoding": "PCMU", "sample_rate": 8000, "channels": 1}}})
	if err != nil {
		t.Fatal(err)
	}
	first := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfe}, 160))
	second := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfd}, 160))
	for _, packet := range []struct{ n, p string }{{"2", second}, {"1", first}, {"1", first}} {
		if err = writeWS(context.Background(), conn, g.Map[g.String, any]{"event": "media", "stream_id": "stream-1", "media": g.Map[g.String, any]{"track": "inbound", "chunk": packet.n, "payload": packet.p}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{first, second} {
		msg := f.next(t, "session.input_audio.append")
		var audio string
		_ = json.Unmarshal(msg["audio"], &audio)
		if audio != want {
			t.Fatal("audio reordered incorrectly or modified")
		}
	}
	f.send(t, g.Map[g.String, any]{"type": "session.output_audio.delta", "delta": first})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, out, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var outEvent mediaEvent
	if json.Unmarshal(out, &outEvent) != nil || outEvent.Media.Payload != g.String(first) {
		t.Fatal("output PCMU not forwarded")
	}
	f.send(t, g.Map[g.String, any]{"type": "session.input_transcript.delta", "delta": "For billing, press one.", "start_ms": 0, "end_ms": 100})
	delegate(t, f, "response-1", "tool-1", "send_dtmf", `{"digits":"1","duration_millis":250,"reason":"billing"}`)
	f.next(t, "response.item.create")
	f.next(t, "response.create")
	_, tones, _ := f.counts()
	if tones != 1 {
		t.Fatalf("DTMF count %d", tones)
	}
	duplicate := c.dtmf("tool_tool-1", "1", 250)
	if duplicate.IsErr() {
		t.Fatal(duplicate.Err())
	}
	_, tones, _ = f.counts()
	if tones != 1 {
		t.Fatal("duplicate tool resent tones")
	}
	if c.dtmf("tool_tool-1", "2", 250).IsOk() {
		t.Fatal("conflicting duplicate accepted")
	}
	delegate(t, f, "response-2", "question-1", "ask_user", `{"question":"Which day works for you?"}`)
	await(t, func() bool { return c.snapshot().Question != nil })
	if c.reply("wrong-question", "Tuesday").IsOk() {
		t.Fatal("wrong question accepted")
	}
	if answer := c.reply("question-1", "Tuesday"); answer.IsErr() {
		t.Fatal(answer.Err())
	}
	answer := f.next(t, "response.item.create")
	if !bytes.Contains(answer["item"], []byte("Tuesday")) {
		t.Fatal("answer did not reach backend")
	}
	f.next(t, "response.create")
	await(t, func() bool { return c.snapshot().Question == nil })
	if c.reply("question-1", "Thursday").IsOk() {
		t.Fatal("expired answer accepted")
	}
	if err = c.stop("test completed"); err != nil {
		t.Fatal(err)
	}
	if hook(t, s, key, "ended-1", "call.hangup", task) != 200 {
		t.Fatal("hangup rejected")
	}
	hook(t, s, key, "answered-late", "call.answered", task)
	if c.snapshot().Status != "ended" {
		t.Fatal("late answer revived call")
	}
	await(t, func() bool { return len(c.snapshot().VoiceUsage) > 0 })
	b, err := os.ReadFile(s.receiptPath(task.ID).Std())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, task.Information.Bytes()) || bytes.Contains(b, []byte("For billing")) {
		t.Fatal("private task details or transcript persisted")
	}
	if _, err = os.Stat(s.activePath().Std()); !os.IsNotExist(err) {
		t.Fatal("active reservation not released")
	}
	restarted := NewServer(s.cfg).Unwrap()
	if result := restarted.start(task); result.IsErr() || result.Unwrap().Status != "ended" {
		t.Fatal("completed request not restored idempotently")
	}
	d, tones, h := f.counts()
	if d != 1 || tones != 1 || h != 1 {
		t.Fatalf("unexpected operations: %d/%d/%d", d, tones, h)
	}
}

func TestUnknownDialSurvivesRestartWithoutRedial(t *testing.T) {
	s, f, key, _ := fixture(t, true, false)
	task := request()
	if s.start(task).IsErr() {
		t.Fatal("start failed")
	}
	c := s.lookup(task.ID).Unwrap()
	await(t, func() bool { return c.snapshot().Status == "uncertain" })
	restarted := NewServer(s.cfg).Unwrap()
	restarted.telnyxURL = s.telnyxURL
	if restarted.active.snapshot().Status != "recovery_required" {
		t.Fatal("unknown outcome discarded")
	}
	next := task
	next.ID = "task-00000002"
	if restarted.start(next).IsOk() {
		t.Fatal("new call allowed with unresolved active receipt")
	}
	if hook(t, restarted, key, "late-id-1", "call.initiated", task) != 200 {
		t.Fatal("late provider correlation rejected")
	}
	await(t, func() bool { _, _, n := f.counts(); return n == 1 })
	if err := restarted.active.reconcile(); err != nil {
		t.Fatal(err)
	}
	if restarted.active.snapshot().Status != "ended" {
		t.Fatal("provider-ended state not reconciled")
	}
	d, _, _ := f.counts()
	if d != 1 {
		t.Fatal("dial was retried")
	}
}

func TestFailedModelSetupNeverDials(t *testing.T) {
	s, f, _, _ := fixture(t, false, true)
	task := request()
	s.start(task)
	c := s.lookup(task.ID).Unwrap()
	await(t, func() bool { return c.snapshot().Status == "failed" })
	d, _, _ := f.counts()
	if d != 0 {
		t.Fatal("dialed after failed model setup")
	}
}

func TestStreamingFailureWithoutClientStateEndsKnownCall(t *testing.T) {
	s, f, key, _ := fixture(t, false, false)
	task := request()
	s.start(task)
	c := s.lookup(task.ID).Unwrap()
	await(t, func() bool { return c.snapshot().Status == "ringing" })
	body, _ := json.Marshal(g.Map[g.String, any]{"data": g.Map[g.String, any]{"id": "media-failed-1", "event_type": "streaming.failed", "payload": g.Map[g.String, any]{"call_control_id": "v2:test-call", "stream_url": c.streamURL()}}})
	r := httptest.NewRequest("POST", "/webhooks/telnyx", bytes.NewReader(body))
	r.Header = signed(key, body)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("stream failure callback rejected")
	}
	await(t, func() bool { _, _, n := f.counts(); return n == 1 })
	if c.snapshot().LastError != "Telnyx media streaming failed" {
		t.Fatal("stream failure lost")
	}
}

func TestUncertainDialWithKnownIDRequestsHangup(t *testing.T) {
	s, f, key, _ := fixture(t, false, false)
	task := request()
	s.start(task)
	c := s.lookup(task.ID).Unwrap()
	await(t, func() bool { return c.snapshot().Status == "ringing" })
	hook(t, s, key, "early-answer", "call.answered", task)
	c.uncertainDial("Dial accepted without a call identifier; reconcile in Telnyx")
	_, _, hangups := f.counts()
	if hangups != 1 {
		t.Fatal("known answered call was left running")
	}
	if c.snapshot().LastError.IsEmpty() {
		t.Fatal("unknown outcome not retained")
	}
}

func TestConcurrentRequestsReserveOneCall(t *testing.T) {
	s, f, _, _ := fixture(t, false, false)
	task := request()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if result := s.start(task); result.IsErr() {
				t.Error(result.Err())
			}
		})
	}
	wg.Wait()
	c := s.lookup(task.ID).Unwrap()
	await(t, func() bool { return c.snapshot().Status == "ringing" })
	d, _, _ := f.counts()
	if d != 1 {
		t.Fatal("concurrent retries dialed multiple times")
	}
	other := task
	other.ID = "task-00000002"
	if s.start(other).IsOk() {
		t.Fatal("overlapping call allowed")
	}
}

func TestWebhookRejectsForgeryAndWrongCall(t *testing.T) {
	s, _, key, _ := fixture(t, false, false)
	task := request()
	s.start(task)
	c := s.lookup(task.ID).Unwrap()
	await(t, func() bool { return c.snapshot().Status == "ringing" })
	r := httptest.NewRequest("POST", "/webhooks/telnyx", bytes.NewBufferString(`{"data":{}}`))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("forged webhook accepted")
	}
	wrong := task
	wrong.To = "+15555550999"
	if hook(t, s, key, "wrong-answer", "call.answered", wrong) == 200 {
		t.Fatal("wrong call accepted")
	}
	if c.snapshot().Status != "ringing" {
		t.Fatal("wrong call changed state")
	}
	r = httptest.NewRequest("GET", "/media/"+task.ID.Std(), nil)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("unsigned media accepted")
	}
}

func TestConfigPrivateAndEnv(t *testing.T) {
	path := g.String(t.TempDir() + "/config.json")
	created := InitConfig(path)
	if created.IsErr() {
		t.Fatal(created.Err())
	}
	if InitConfig(path).IsOk() {
		t.Fatal("config overwritten")
	}
	if created.Unwrap().APIToken.Len() < 32 {
		t.Fatal("weak operator token")
	}
	if LoadConfig(path).IsErr() {
		t.Fatal("cannot load config")
	}
	t.Setenv("OPENAI_API_KEY", "environment-secret")
	if LoadConfig(path).Unwrap().OpenAIKey != "environment-secret" {
		t.Fatal("environment override failed")
	}
	if err := os.Chmod(path.Std(), 0644); err != nil {
		t.Fatal(err)
	}
	if LoadConfig(path).IsOk() {
		t.Fatal("public secret file accepted")
	}
}

func TestQuestionCannotReviveStoppedCall(t *testing.T) {
	cfg, _ := testConfig(t)
	s := NewServer(cfg).Unwrap()
	c := newCall(s, request(), receipt{RequestHash: "test", Snapshot: Snapshot{ID: request().ID, Status: "ended"}})
	c.stopRequested = true
	if c.askUser("late-question", "Can you approve this?").IsOk() {
		t.Fatal("late question accepted")
	}
	if c.snapshot().Status != "ended" || c.snapshot().Question != nil {
		t.Fatal("question revived ended call")
	}
}

func TestShutdownWaitsForVoiceFinalization(t *testing.T) {
	s, _, _, _ := fixture(t, false, false)
	task := request()
	s.start(task)
	c := s.lookup(task.ID).Unwrap()
	await(t, func() bool { return c.snapshot().Status == "ringing" })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.snapshot().VoiceUsage) == 0 {
		t.Fatal("shutdown returned before final session usage")
	}
}

func TestShutdownRejectsNewCalls(t *testing.T) {
	cfg, _ := testConfig(t)
	s := NewServer(cfg).Unwrap()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.start(request()).IsOk() {
		t.Fatal("new call accepted during shutdown")
	}
}

func TestUnknownDTMFBlocksAnotherSelection(t *testing.T) {
	s, f, key, _ := fixture(t, false, false)
	task := request()
	s.start(task)
	c := s.lookup(task.ID).Unwrap()
	await(t, func() bool { return c.snapshot().Status == "ringing" })
	hook(t, s, key, "answered-1", "call.answered", task)
	f.mu.Lock()
	f.unknownDTMF = true
	f.mu.Unlock()
	if c.dtmf("first-command", "1", 250).IsOk() {
		t.Fatal("unknown DTMF reported success")
	}
	c.mu.Lock()
	blocked := c.stopRequested
	c.mu.Unlock()
	if !blocked {
		t.Fatal("uncertain result did not atomically block tools")
	}
	if c.dtmf("second-command", "2", 250).IsOk() {
		t.Fatal("another DTMF selection allowed after uncertain outcome")
	}
	_, tones, _ := f.counts()
	if tones != 1 {
		t.Fatal("extra DTMF sent")
	}
}

func TestReceiptFailureStillHangsUp(t *testing.T) {
	s, f, _, _ := fixture(t, false, false)
	task := request()
	s.start(task)
	c := s.lookup(task.ID).Unwrap()
	await(t, func() bool { return c.snapshot().Status == "ringing" })
	path := s.receiptPath(task.ID).Std()
	if err := os.Rename(path, path+".preserved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := c.stop("receipt fault test"); err == nil {
		t.Fatal("persistence failure discarded")
	}
	_, _, hangups := f.counts()
	if hangups != 1 {
		t.Fatal("receipt fault prevented hangup")
	}
	if _, err := os.Stat(path + ".preserved"); err != nil {
		t.Fatal("previous receipt not preserved")
	}
}

func TestQuestionReceiptFailureClearsPendingQuestion(t *testing.T) {
	cfg, _ := testConfig(t)
	s := NewServer(cfg).Unwrap()
	task := request()
	c := newCall(s, task, receipt{RequestHash: "test", Snapshot: Snapshot{ID: task.ID, Status: "active"}})
	if err := os.Mkdir(s.receiptPath(task.ID).Std(), 0700); err != nil {
		t.Fatal(err)
	}
	if c.askUser("question-1", "Which day?").IsOk() {
		t.Fatal("failed question receipt reported success")
	}
	if c.snapshot().Question != nil || c.snapshot().Status != "active" {
		t.Fatal("failed question remained pending")
	}
}

func TestDelayedCompletedEventDoesNotRunNextResponsesTools(t *testing.T) {
	s, f, key, _ := fixture(t, false, false)
	task := request()
	s.start(task)
	c := s.lookup(task.ID).Unwrap()
	await(t, func() bool { return c.snapshot().Status == "ringing" })
	hook(t, s, key, "answered-1", "call.answered", task)
	delegate(t, f, "response-A", "tool-A", "send_dtmf", `{"digits":"1","duration_millis":250,"reason":"billing"}`)
	f.next(t, "response.item.create")
	f.next(t, "response.create")
	envelope := func(event any) g.Map[g.String, any] {
		return g.Map[g.String, any]{"type": "response.event", "delegation_id": "delegation-1", "event": event}
	}
	f.send(t, envelope(g.Map[g.String, any]{"type": "response.created", "response": g.Map[g.String, any]{"id": "response-B"}}))
	f.send(t, envelope(g.Map[g.String, any]{"type": "response.output_item.done", "item": g.Map[g.String, any]{"type": "function_call", "call_id": "tool-B", "name": "send_dtmf", "arguments": `{"digits":"2","duration_millis":250,"reason":"new menu"}`}}))
	f.send(t, envelope(g.Map[g.String, any]{"type": "response.completed", "response": g.Map[g.String, any]{"id": "response-A"}}))
	f.send(t, g.Map[g.String, any]{"type": "session.input_transcript.delta", "delta": "barrier"})
	await(t, func() bool { return len(c.snapshot().Transcript) > 0 })
	select {
	case <-f.messages:
		t.Fatal("old completion continued a different response")
	case <-time.After(100 * time.Millisecond):
	}
	_, tones, _ := f.counts()
	if tones != 1 {
		t.Fatal("next response tool executed early")
	}
	f.send(t, envelope(g.Map[g.String, any]{"type": "response.completed", "response": g.Map[g.String, any]{"id": "response-B"}}))
	f.next(t, "response.item.create")
	f.next(t, "response.create")
	_, tones, _ = f.counts()
	if tones != 2 {
		t.Fatal("new response did not execute its tool")
	}
}

func TestModelFailureDuringDialKeepsReceiptAndHangsUp(t *testing.T) {
	s, f, _, _ := fixture(t, false, false)
	task := request()
	f.mu.Lock()
	f.onDial = func() {
		f.send(t, g.Map[g.String, any]{"type": "error", "error": g.Map[g.String, any]{"code": "responses_validation_failed"}})
		c := s.lookup(task.ID).Unwrap()
		await(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.stopRequested })
		if terminal(c.snapshot().Status) {
			t.Error("dial in progress marked terminal before provider outcome")
		}
	}
	f.mu.Unlock()
	if s.start(task).IsErr() {
		t.Fatal("start failed")
	}
	await(t, func() bool { _, _, n := f.counts(); return n == 1 })
	if _, err := os.Stat(s.activePath().Std()); err != nil {
		t.Fatal("unconfirmed call reservation released")
	}
	if err := s.lookup(task.ID).Unwrap().reconcile(); err != nil {
		t.Fatal(err)
	}
}
