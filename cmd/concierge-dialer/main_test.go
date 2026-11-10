package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/bellhopi/bellhopi/internal/dialer"
	"github.com/enetx/g"
)

func TestHelpAndInvalidCommands(t *testing.T) {
	for args := range (g.Slice[g.Slice[g.String]]{nil, {"help"}, {"--help"}}).Iter() {
		var output bytes.Buffer
		result := run(args, &output)
		if result.IsErr() || !json.Valid(output.Bytes()) {
			t.Fatalf("help %v: %v %s", args, result.Err(), output.String())
		}
	}
	for args := range (g.Slice[g.Slice[g.String]]{{"unknown"}, {"--unknown"}, {"init", "--config", "misplaced"}}).Iter() {
		var output bytes.Buffer
		if result := run(args, &output); result.IsOk() {
			t.Fatalf("accepted invalid command %v", args)
		}
	}
}

func TestOperatorBaseRejectsRemoteAndInvalidPorts(t *testing.T) {
	for listen, want := range (g.Map[g.String, g.String]{
		"127.0.0.1:8787": "http://127.0.0.1:8787",
		"[::1]:8787":     "http://[::1]:8787",
		"localhost:8787": "http://127.0.0.1:8787",
	}).Iter() {
		if result := operatorBase(listen); result.IsErr() || result.Unwrap() != want {
			t.Fatalf("%s: %v", listen, result)
		}
	}
	for listen := range g.SliceOf[g.String]("example.com:8787", "0.0.0.0:8787", ":8787", "[::]:8787", "192.168.1.2:8787", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:abc", "https://127.0.0.1:8787").Iter() {
		if operatorBase(listen).IsOk() {
			t.Fatalf("accepted unsafe listen address %s", listen)
		}
	}
}

func TestOperatorCommandsPreserveIDsAndPayloads(t *testing.T) {
	type captured struct {
		method, path, auth g.String
		body               g.Map[g.String, any]
	}
	received := make(chan captured, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := g.NewMap[g.String, any]()
		if r.Method != http.MethodGet {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("request JSON: %v", err)
			}
		}
		received <- captured{g.String(r.Method), g.String(r.URL.Path), g.String(r.Header.Get("Authorization")), body}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(g.Map[g.String, any]{"id": "request-0001", "status": "active"})
	}))
	defer server.Close()
	cfg := dialer.Config{Listen: g.String(server.Listener.Addr().String()), APIToken: "operator-secret"}
	request := privateFile(t, "request.json", `{"id":"request-0001","to":"+18005551234","task":"Ask about mailbox availability","information":"Downtown branch; month to month"}`)
	answer := privateFile(t, "answer.txt", "A small mailbox\n")
	tests := g.Slice[struct {
		command, method, path g.String
		args                  g.Slice[g.String]
		want                  g.Map[g.String, any]
	}]{
		{"call", "POST", "/v1/calls", g.SliceOf[g.String]("--request", request), g.Map[g.String, any]{"id": "request-0001", "to": "+18005551234", "task": "Ask about mailbox availability", "information": "Downtown branch; month to month"}},
		{"status", "GET", "/v1/calls/request-0001", g.SliceOf[g.String]("--id", "request-0001"), g.Map[g.String, any]{}},
		{"reply", "POST", "/v1/calls/request-0001/reply", g.SliceOf[g.String]("--id", "request-0001", "--question-id", "question-0001", "--answer-file", answer), g.Map[g.String, any]{"question_id": "question-0001", "answer": "A small mailbox"}},
		{"dtmf", "POST", "/v1/calls/request-0001/dtmf", g.SliceOf[g.String]("--id", "request-0001", "--digits", "1#", "--command-id", "c05636d9-bf26-4cb8-82c9-adccaf315ae9"), g.Map[g.String, any]{"digits": "1#", "duration_millis": float64(250), "command_id": "c05636d9-bf26-4cb8-82c9-adccaf315ae9"}},
		{"hangup", "POST", "/v1/calls/request-0001/hangup", g.SliceOf[g.String]("--id", "request-0001"), g.Map[g.String, any]{}},
		{"reconcile", "POST", "/v1/calls/request-0001/reconcile", g.SliceOf[g.String]("--id", "request-0001"), g.Map[g.String, any]{}},
	}
	for test := range tests.Iter() {
		t.Run(test.command.Std(), func(t *testing.T) {
			var output bytes.Buffer
			result := operatorCommand(test.command, test.args, cfg, &output)
			if result.IsErr() {
				t.Fatal(result.Err())
			}
			got := <-received
			if got.method != test.method || got.path != test.path || got.auth != "Bearer operator-secret" {
				t.Fatalf("unexpected request: %+v", got)
			}
			for key, want := range test.want.Iter() {
				if got.body[key] != want {
					t.Errorf("%s: got %v want %v", key, got.body[key], want)
				}
			}
			if !json.Valid(output.Bytes()) {
				t.Fatalf("not JSON: %s", output.String())
			}
		})
	}
}

func TestPrivateInputAndBadFlagsNeverSend(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid input reached operator API")
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	cfg := dialer.Config{Listen: g.String(server.Listener.Addr().String()), APIToken: "test"}
	public := privateFile(t, "public.json", `{"id":"request-0001","to":"+18005551234","task":"Check hours","information":""}`)
	if err := os.Chmod(public.Std(), 0644); err != nil {
		t.Fatal(err)
	}
	unknown := privateFile(t, "unknown.json", `{"id":"request-0001","to":"+18005551234","task":"Check hours","information":"","extra":true}`)
	trailing := privateFile(t, "trailing.json", `{"id":"request-0001","to":"+18005551234","task":"Check hours","information":""} {}`)
	noID := privateFile(t, "no-id.json", `{"to":"+18005551234","task":"Check hours","information":""}`)
	for test := range (g.Slice[struct {
		command g.String
		args    g.Slice[g.String]
	}]{
		{"call", g.SliceOf[g.String]("--request", public)},
		{"call", g.SliceOf[g.String]("--request", unknown)},
		{"call", g.SliceOf[g.String]("--request", trailing)},
		{"call", g.SliceOf[g.String]("--request", noID)},
		{"call", g.SliceOf[g.String]("--request")},
		{"status", g.SliceOf[g.String]("--id", "../request-0001")},
		{"status", g.SliceOf[g.String]("--id", "request-0001", "extra")},
		{"reply", g.SliceOf[g.String]("--id", "request-0001")},
		{"dtmf", g.SliceOf[g.String]("--id", "request-0001", "--digits", "1")},
		{"dtmf", g.SliceOf[g.String]("--id", "request-0001", "--duration", "abc")},
	}).Iter() {
		var output bytes.Buffer
		if result := operatorCommand(test.command, test.args, cfg, &output); result.IsOk() {
			t.Fatalf("accepted invalid %s %v", test.command, test.args)
		}
	}
}

func TestOperatorDoesNotFollowRedirects(t *testing.T) {
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("operator token followed a redirect")
	}))
	defer redirected.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirected.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	cfg := dialer.Config{Listen: g.String(server.Listener.Addr().String()), APIToken: "private-operator-token"}
	var output bytes.Buffer
	if result := requestOperator(cfg, "GET", "/v1/calls/request-0001", nil, &output); result.IsOk() {
		t.Fatal("redirect accepted")
	}
}

func TestOperatorResponsesRedactCredentials(t *testing.T) {
	for status := range g.SliceOf(http.StatusOK, http.StatusUnauthorized).Iter() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(g.Map[g.String, any]{"error": `private-openai private-telnyx private-token`, "nested": g.SliceOf(g.Map[g.String, any]{"private-token": `private-openai`})})
		}))
		cfg := dialer.Config{Listen: g.String(server.Listener.Addr().String()), APIToken: "private-token", OpenAIKey: "private-openai", TelnyxKey: "private-telnyx"}
		var output bytes.Buffer
		result := requestOperator(cfg, "GET", "/v1/calls/request-0001", nil, &output)
		text := g.String(output.String())
		if result.IsErr() {
			text += g.String(result.Err().Error())
		}
		if text.ContainsAny(cfg.APIToken, cfg.OpenAIKey, cfg.TelnyxKey) || !text.Contains("[redacted]") {
			t.Fatalf("credential redaction failed: %s", text)
		}
		if status == http.StatusUnauthorized && result.IsOk() {
			t.Fatal("HTTP error did not fail")
		}
		server.Close()
	}
}

func privateFile(t *testing.T, name, content g.String) g.String {
	t.Helper()
	path := g.String(filepath.Join(t.TempDir(), name.Std()))
	if err := os.WriteFile(path.Std(), content.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
