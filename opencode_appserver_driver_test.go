package agentbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenCodeAppServerDriverNativeEnqueue(t *testing.T) {
	messageStarted := make(chan struct{})
	releaseMessage := make(chan struct{})
	requests := make(chan string, 4)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/event":
			serveOpenCodeEventStream(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			requests <- "create"
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "session-1"})
		case r.Method == http.MethodPost && r.URL.Path == "/session/session-1/message":
			requests <- "message"
			close(messageStarted)
			<-releaseMessage
			_ = json.NewEncoder(w).Encode(map[string]any{
				"info": map[string]any{
					"tokens": map[string]any{
						"input":  3,
						"output": 5,
						"cache":  map[string]any{"read": 1, "write": 0},
					},
				},
				"parts": []map[string]any{{"type": "text", "text": "done"}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/session/session-1/prompt_async":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !strings.Contains(mustJSON(t, body), "second") {
				t.Fatalf("prompt_async body = %#v, want second prompt", body)
			}
			requests <- "prompt_async"
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	driver := newOpenCodeAppServerDriver(OpenCodeConfig{ServerURL: server.URL})
	session := NewInteractiveSession(driver)
	defer session.Close()

	first, err := session.Submit(context.Background(), RunRequest{UserText: "first"})
	if err != nil {
		t.Fatalf("first submit failed: %v", err)
	}
	if first.Mode != SubmitStarted {
		t.Fatalf("first mode = %q, want %q", first.Mode, SubmitStarted)
	}
	<-messageStarted

	second, err := session.Submit(context.Background(), RunRequest{UserText: "second"})
	if err != nil {
		t.Fatalf("second submit failed: %v", err)
	}
	if second.Mode != SubmitSteered {
		t.Fatalf("second mode = %q, want %q", second.Mode, SubmitSteered)
	}
	waitForRequest(t, requests, "prompt_async")

	close(releaseMessage)
	waitForTurnEvent(t, session.Events(), TurnEventCompleted)
}

func TestOpenCodeAppServerDriverInterruptUsesAbort(t *testing.T) {
	messageStarted := make(chan struct{})
	releaseMessage := make(chan struct{})
	abortCalled := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/event":
			serveOpenCodeEventStream(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/session/session-1/message":
			close(messageStarted)
			<-releaseMessage
			_ = json.NewEncoder(w).Encode(map[string]any{"info": map[string]any{}, "parts": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/session/session-1/abort":
			close(abortCalled)
			_ = json.NewEncoder(w).Encode(true)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	driver := newOpenCodeAppServerDriver(OpenCodeConfig{ServerURL: server.URL})
	session := NewInteractiveSession(driver)
	defer session.Close()

	if _, err := session.Submit(context.Background(), RunRequest{ThreadID: "session-1", UserText: "first"}); err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	<-messageStarted
	if err := session.Interrupt(context.Background()); err != nil {
		t.Fatalf("interrupt failed: %v", err)
	}
	waitClosed(t, abortCalled, "abort should be called")
	close(releaseMessage)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func serveOpenCodeEventStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	<-r.Context().Done()
}

func waitForRequest(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-ch:
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for request %q", want)
		}
	}
}

func waitForTurnEvent(t *testing.T, events <-chan TurnEvent, want TurnEventKind) TurnEvent {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("events closed while waiting for %q", want)
			}
			if event.Kind == want {
				return event
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event %q", want)
		}
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal(message)
	}
}
