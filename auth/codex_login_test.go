package auth

import (
	"net/url"
	"reflect"
	"testing"
	"time"
)

func TestCodexCallbackBindAddrs(t *testing.T) {
	t.Setenv("CODEX_CALLBACK_BIND", "")
	if got, want := codexCallbackBindAddrs(), []string{"127.0.0.1:1455", "[::1]:1455"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("default bind addrs = %v, want %v", got, want)
	}

	t.Setenv("CODEX_CALLBACK_BIND", "0.0.0.0")
	if got, want := codexCallbackBindAddrs(), []string{"0.0.0.0:1455"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("container bind addrs = %v, want %v", got, want)
	}
}

func TestStartCodexLoginReusesActiveSession(t *testing.T) {
	existing := &CodexSession{
		ID:        "existing-session",
		Verifier:  "existing-verifier",
		State:     "existing-state",
		ExpiresAt: time.Now().Add(time.Minute),
		resultCh:  make(chan codexCapture, 1),
	}

	codexSessionsMu.Lock()
	previous := codexSessions
	codexSessions = map[string]*CodexSession{existing.ID: existing}
	codexSessionsMu.Unlock()
	t.Cleanup(func() {
		codexSessionsMu.Lock()
		codexSessions = previous
		codexSessionsMu.Unlock()
	})

	got, signInURL, err := StartCodexLogin()
	if err != nil {
		t.Fatalf("StartCodexLogin returned error: %v", err)
	}
	if got != existing {
		t.Fatalf("StartCodexLogin returned session %p, want existing %p", got, existing)
	}

	parsed, err := url.Parse(signInURL)
	if err != nil {
		t.Fatalf("parse sign-in URL: %v", err)
	}
	if state := parsed.Query().Get("state"); state != existing.State {
		t.Fatalf("sign-in state = %q, want %q", state, existing.State)
	}
	if challenge := parsed.Query().Get("code_challenge"); challenge != generateCodeChallenge(existing.Verifier) {
		t.Fatalf("sign-in challenge does not match the existing verifier")
	}
	if originator := parsed.Query().Get("originator"); originator != codexOriginator {
		t.Fatalf("sign-in originator = %q, want %q", originator, codexOriginator)
	}
	if prompt := parsed.Query().Get("prompt"); prompt != "" {
		t.Fatalf("sign-in URL unexpectedly forces prompt=%q", prompt)
	}
	if scope := parsed.Query().Get("scope"); scope != codexScope {
		t.Fatalf("sign-in scope = %q, want %q", scope, codexScope)
	}
}

func TestReusableCodexSessionRemovesExpired(t *testing.T) {
	expired := &CodexSession{
		ID:        "expired-session",
		ExpiresAt: time.Now().Add(-time.Minute),
		resultCh:  make(chan codexCapture, 1),
	}

	codexSessionsMu.Lock()
	previous := codexSessions
	codexSessions = map[string]*CodexSession{expired.ID: expired}
	codexSessionsMu.Unlock()
	t.Cleanup(func() {
		codexSessionsMu.Lock()
		codexSessions = previous
		codexSessionsMu.Unlock()
	})

	if got := reusableCodexSession(time.Now()); got != nil {
		t.Fatalf("reusableCodexSession returned expired session %q", got.ID)
	}
	codexSessionsMu.RLock()
	_, exists := codexSessions[expired.ID]
	codexSessionsMu.RUnlock()
	if exists {
		t.Fatalf("expired session was not removed")
	}
}
