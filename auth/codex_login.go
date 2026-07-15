package auth

// codex_login.go drives the ChatGPT (Codex) OAuth sign-in via a transient
// loopback listener, mirroring the Start/Poll session pattern of kiro_sso.go.
// StartCodexLogin binds 127.0.0.1:1455 and returns the sign-in URL; the operator
// opens it in a browser ON THE SAME HOST (the OAuth redirect targets
// 127.0.0.1:1455/auth/callback); PollCodexAuth reports pending until the listener
// captures the authorization code, then exchanges it for tokens.

import (
	"context"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const codexLoginTimeout = 10 * time.Minute

// CodexSession holds the transient state for one Codex sign-in attempt.
type CodexSession struct {
	ID        string
	Verifier  string
	State     string
	ProxyURL  string
	ExpiresAt time.Time

	srv       *http.Server
	resultCh  chan codexCapture
	once      sync.Once
	closeOnce sync.Once
	timer     *time.Timer
}

// codexCapture is the raw outcome delivered by the loopback listener.
type codexCapture struct {
	code string
	err  error
}

// CodexResult is the resolved credential returned once the code is exchanged.
type CodexResult struct {
	AccessToken  string
	RefreshToken string
	AccountID    string
	Email        string
	ExpiresAt    int64
}

var (
	codexSessions   = make(map[string]*CodexSession)
	codexSessionsMu sync.RWMutex
	codexStartMu    sync.Mutex
)

// codexCallbackBindAddrs returns the address(es) the callback listener binds.
// Loopback only by default; override with CODEX_CALLBACK_BIND for containers.
func codexCallbackBindAddrs() []string {
	if bind := strings.TrimSpace(os.Getenv("CODEX_CALLBACK_BIND")); bind != "" {
		return []string{net.JoinHostPort(bind, codexRedirectPort)}
	}
	return []string{"127.0.0.1:" + codexRedirectPort, "[::1]:" + codexRedirectPort}
}

// StartCodexLogin binds the loopback listener and returns the session + sign-in URL.
func StartCodexLogin() (*CodexSession, string, error) {
	// Port 1455 is fixed by the registered OAuth redirect, so only one callback
	// listener can exist at a time. A browser refresh loses its client-side
	// session id while the backend session remains alive; reuse that session
	// instead of failing the next Start request with EADDRINUSE.
	codexStartMu.Lock()
	defer codexStartMu.Unlock()

	now := time.Now()
	if existing := reusableCodexSession(now); existing != nil {
		challenge := generateCodeChallenge(existing.Verifier)
		return existing, codexGenerateAuthURL(existing.State, challenge), nil
	}

	verifier := generateCodeVerifier()
	challenge := generateCodeChallenge(verifier)
	state := uuid.New().String()

	session := &CodexSession{
		ID:        uuid.New().String(),
		Verifier:  verifier,
		State:     state,
		ProxyURL:  config.GetProxyURL(),
		ExpiresAt: now.Add(codexLoginTimeout),
		resultCh:  make(chan codexCapture, 1),
	}

	if err := session.startListener(); err != nil {
		return nil, "", err
	}

	signInURL := codexGenerateAuthURL(state, challenge)

	codexSessionsMu.Lock()
	codexSessions[session.ID] = session
	codexSessionsMu.Unlock()

	session.timer = time.AfterFunc(codexLoginTimeout, func() {
		session.close()
		removeCodexSession(session.ID)
	})

	return session, signInURL, nil
}

// reusableCodexSession returns the active singleton login session, cleaning up
// any expired entries first. StartCodexLogin serializes calls around this helper.
func reusableCodexSession(now time.Time) *CodexSession {
	var active *CodexSession
	var expired []*CodexSession

	codexSessionsMu.Lock()
	for id, session := range codexSessions {
		if !now.Before(session.ExpiresAt) {
			delete(codexSessions, id)
			expired = append(expired, session)
			continue
		}
		if active == nil {
			active = session
		}
	}
	codexSessionsMu.Unlock()

	for _, session := range expired {
		session.close()
	}
	return active
}

// PollCodexAuth reports login status: ("", "pending", nil) until the code is
// captured, then exchanges it and returns the credential with status "completed".
func PollCodexAuth(sessionID string) (*CodexResult, string, error) {
	codexSessionsMu.RLock()
	session, ok := codexSessions[sessionID]
	codexSessionsMu.RUnlock()
	if !ok {
		return nil, "", fmt.Errorf("session not found or expired")
	}

	select {
	case capture := <-session.resultCh:
		session.close()
		removeCodexSession(sessionID)
		if capture.err != nil {
			return nil, "", capture.err
		}
		client := GetAuthClientForProxy(session.ProxyURL)
		td, err := exchangeCodexCode(context.Background(), client, capture.code, session.Verifier)
		if err != nil {
			return nil, "", fmt.Errorf("codex token exchange failed: %w", err)
		}
		return &CodexResult{
			AccessToken:  td.AccessToken,
			RefreshToken: td.RefreshToken,
			AccountID:    td.AccountID,
			Email:        td.Email,
			ExpiresAt:    td.ExpiresAt,
		}, "completed", nil
	default:
		if time.Now().After(session.ExpiresAt) {
			session.close()
			removeCodexSession(sessionID)
			return nil, "", fmt.Errorf("codex login timed out after %s", codexLoginTimeout)
		}
		return nil, "pending", nil
	}
}

// CompleteCodexLogin finishes a login using a callback URL (or bare code) pasted
// by the operator, for when the browser can't reach the loopback listener (e.g.
// signing in on a different machine). It accepts the full redirect URL
// (http://localhost:1455/auth/callback?code=...&state=...) or just the code.
func CompleteCodexLogin(sessionID, callbackURL string) (*CodexResult, error) {
	codexSessionsMu.RLock()
	session, ok := codexSessions[sessionID]
	codexSessionsMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session not found or expired")
	}

	code, state := parseCodexCallback(callbackURL)
	if code == "" {
		return nil, fmt.Errorf("no authorization code found in the pasted URL")
	}
	// If a state is present it must match; a bare code (no state) is accepted.
	if state != "" && state != session.State {
		return nil, fmt.Errorf("state mismatch — paste the URL from this login attempt")
	}

	session.close()
	removeCodexSession(sessionID)

	client := GetAuthClientForProxy(session.ProxyURL)
	td, err := exchangeCodexCode(context.Background(), client, code, session.Verifier)
	if err != nil {
		return nil, fmt.Errorf("codex token exchange failed: %w", err)
	}
	return &CodexResult{
		AccessToken:  td.AccessToken,
		RefreshToken: td.RefreshToken,
		AccountID:    td.AccountID,
		Email:        td.Email,
		ExpiresAt:    td.ExpiresAt,
	}, nil
}

// parseCodexCallback extracts the code and state from a pasted value that is
// either a full redirect URL or a bare authorization code.
func parseCodexCallback(raw string) (code, state string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	if u, err := url.Parse(raw); err == nil && u.RawQuery != "" {
		q := u.Query()
		if c := strings.TrimSpace(q.Get("code")); c != "" {
			return c, strings.TrimSpace(q.Get("state"))
		}
	}
	// Not a URL with a code query: treat the whole value as the code.
	return raw, ""
}

// CancelCodexLogin tears down an in-flight session, freeing the loopback port.
func CancelCodexLogin(sessionID string) {
	codexSessionsMu.RLock()
	session, ok := codexSessions[sessionID]
	codexSessionsMu.RUnlock()
	if !ok {
		return
	}
	session.close()
	removeCodexSession(sessionID)
}

func (s *CodexSession) startListener() error {
	addrs := codexCallbackBindAddrs()
	ln, err := net.Listen("tcp", addrs[0])
	if err != nil {
		return fmt.Errorf("cannot bind %s for the Codex callback (is the port already in use?): %w", addrs[0], err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleCallback)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	serve := func(l net.Listener) {
		go func() {
			if errServe := s.srv.Serve(l); errServe != nil && errServe != http.ErrServerClosed {
				logger.Debugf("[Codex] callback listener (%s) stopped: %v", l.Addr(), errServe)
			}
		}()
	}
	serve(ln)
	for _, addr := range addrs[1:] {
		if extra, errExtra := net.Listen("tcp", addr); errExtra == nil {
			serve(extra)
		} else {
			logger.Debugf("[Codex] secondary callback bind %s skipped: %v", addr, errExtra)
		}
	}
	return nil
}

func (s *CodexSession) close() {
	s.closeOnce.Do(func() {
		if s.timer != nil {
			s.timer.Stop()
		}
		if s.srv != nil {
			_ = s.srv.Close()
		}
	})
}

func (s *CodexSession) deliver(capture codexCapture) {
	s.once.Do(func() { s.resultCh <- capture })
}

func (s *CodexSession) handleCallback(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if req.URL.Path != codexCallbackPath {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	q := req.URL.Query()
	code := strings.TrimSpace(q.Get("code"))
	state := strings.TrimSpace(q.Get("state"))
	errParam := strings.TrimSpace(q.Get("error"))

	if s.State == "" || state != s.State {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if errParam != "" {
		desc := strings.TrimSpace(q.Get("error_description"))
		writeCodexCallbackPage(w, false)
		s.deliver(codexCapture{err: fmt.Errorf("codex authorization error: %s %s", errParam, desc)})
		return
	}
	if code == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeCodexCallbackPage(w, true)
	s.deliver(codexCapture{code: code})
}

func writeCodexCallbackPage(w http.ResponseWriter, ok bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	msg := "ChatGPT sign-in complete. You can close this tab and return to the admin panel."
	if !ok {
		msg = "ChatGPT sign-in failed. Return to the admin panel and try again."
	}
	_, _ = fmt.Fprintf(w, "<!doctype html><html><head><meta charset=\"utf-8\"><title>ChatGPT Sign-In</title></head><body style=\"font-family:sans-serif;padding:2rem\"><p>%s</p></body></html>", msg)
}

func removeCodexSession(sessionID string) {
	codexSessionsMu.Lock()
	delete(codexSessions, sessionID)
	codexSessionsMu.Unlock()
}
