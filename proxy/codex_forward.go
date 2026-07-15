package proxy

// codex_forward.go forwards OpenAI-format requests to the ChatGPT (Codex)
// backend using a codex-authenticated account from the pool. The account's
// OAuth access token is the bearer; the ChatGPT account id goes in the
// chatgpt-account-id header. The /v1/responses format matches the backend so
// the request body is forwarded largely as-is.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

const codexBackendResponsesURL = "https://chatgpt.com/backend-api/codex/responses"

// Header values mirroring the real Codex CLI so ChatGPT/Cloudflare see a genuine
// client rather than a proxy. Bump codexClientVersion when the CLI updates.
const (
	codexClientVersion = "0.135.0"
	codexUserAgent     = "codex-tui/0.135.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.135.0)"
	codexOriginator    = "codex-tui"
)

// applyCodexHeaders sets the upstream headers a real Codex CLI sends. sessionID
// is kept stable per account (not random per request) so the account presents a
// consistent fingerprint like a single real user.
func applyCodexHeaders(req *http.Request, account *config.Account, sessionID string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Connection", "Keep-Alive")
	req.Header.Set("Authorization", "Bearer "+account.AccessToken)
	if account.CodexAccountID != "" {
		req.Header.Set("chatgpt-account-id", account.CodexAccountID)
	}
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("User-Agent", codexUserAgent)
	req.Header.Set("Version", codexClientVersion)
	req.Header.Set("Originator", codexOriginator)
	req.Header.Set("Session_id", sessionID)
}

// codexSessionID derives a stable per-account session id from the account id so
// every request from an account reuses the same session, matching how the real
// Codex CLI keeps one session per user rather than a fresh id each call.
func codexSessionID(account *config.Account) string {
	if account == nil || account.ID == "" {
		return uuid.New().String()
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("codex-session:"+account.ID)).String()
}

// codexModelsURL is the ChatGPT backend model-catalog endpoint the real Codex
// CLI queries. It returns the models an account is entitled to.
const codexModelsURL = "https://chatgpt.com/backend-api/codex/models"

// discoverCodexModels queries the ChatGPT model catalog for an account and
// returns the entitled model IDs. It is the Codex analogue of Kiro's
// ListAvailableModels, so routing can pick an account by real model support
// rather than blindly trusting the prefix.
func (h *Handler) discoverCodexModels(account *config.Account) ([]string, error) {
	if err := h.ensureValidToken(account); err != nil {
		return nil, err
	}
	u := codexModelsURL + "?client_version=" + codexClientVersion
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	applyCodexHeaders(req, account, codexSessionID(account))
	req.Header.Set("Accept", "application/json")

	resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("codex models HTTP %d: %s", resp.StatusCode, string(body))
	}

	var out struct {
		Models []struct {
			Slug           string `json:"slug"`
			ID             string `json:"id"`
			SupportedInAPI *bool  `json:"supported_in_api"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse codex models: %w", err)
	}
	ids := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		if m.SupportedInAPI != nil && !*m.SupportedInAPI {
			continue
		}
		id := strings.TrimSpace(m.Slug)
		if id == "" {
			id = strings.TrimSpace(m.ID)
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// refreshCodexModels discovers and caches the model list for one codex account.
func (h *Handler) refreshCodexModels(account *config.Account) {
	ids, err := h.discoverCodexModels(account)
	if err != nil {
		logger.Warnf("[Codex] model discovery failed for %s: %v", accountEmailForLog(account), err)
		return
	}
	h.pool.SetModelList(account.ID, ids)
	logger.Infof("[Codex] discovered %d models for %s", len(ids), accountEmailForLog(account))
}

// testCodexAccount sends a minimal request to the ChatGPT backend to verify a
// codex account works, and writes a JSON result. model may carry the routing
// prefix or be empty; it is normalized to a real model name.
func (h *Handler) testCodexAccount(w http.ResponseWriter, account *config.Account, model string) {
	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}
	// Normalize model: strip the codex prefix if present, else pick the first
	// discovered model, else a sensible default.
	realModel := strings.TrimSpace(model)
	if isCodex, stripped := isCodexModel(realModel); isCodex {
		realModel = stripped
	}
	if realModel == "" {
		if ids := h.pool.GetModelList(account.ID); len(ids) > 0 {
			realModel = ids[0]
		} else {
			realModel = "gpt-5"
		}
	}

	payload := map[string]interface{}{
		"model":  realModel,
		"input":  []codexResponsesInputItem{{Type: "message", Role: "user", Content: []codexResponsesContent{{Type: "input_text", Text: "say ok"}}}},
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, codexBackendResponsesURL, bytes.NewReader(body))
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	applyCodexHeaders(req, account, codexSessionID(account))

	resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(errBody))})
		return
	}

	var content strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev codexResponsesEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if ev.Type == "response.output_text.delta" {
			content.WriteString(ev.Delta)
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"reply":   content.String(),
		"model":   realModel,
	})
}

// apiStartCodex starts the ChatGPT (Codex) OAuth sign-in. It binds the loopback
// callback listener and returns the sign-in URL the operator opens in a browser
// ON THE SAME HOST as the proxy (the OAuth redirect targets 127.0.0.1:1455).
func (h *Handler) apiStartCodex(w http.ResponseWriter, r *http.Request) {
	session, signInURL, err := auth.StartCodexLogin()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId": session.ID,
		"signInUrl": signInURL,
		"interval":  2,
	})
}

// apiPollCodex reports the Codex sign-in status; on completion it stores the
// account and returns its id.
func (h *Handler) apiPollCodex(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	result, status, err := auth.PollCodexAuth(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if status != "completed" {
		json.NewEncoder(w).Encode(map[string]interface{}{"status": status})
		return
	}

	account := config.Account{
		ID:             auth.GenerateAccountID(),
		Email:          result.Email,
		AccessToken:    result.AccessToken,
		RefreshToken:   result.RefreshToken,
		CodexAccountID: result.AccountID,
		AuthMethod:     "codex",
		Provider:       "ChatGPT",
		ExpiresAt:      result.ExpiresAt,
		Enabled:        true,
	}
	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	go h.refreshCodexModels(&account)
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "id": account.ID})
}

// apiCompleteCodex finishes a Codex sign-in from a callback URL (or bare code)
// pasted by the operator, for when the browser cannot reach the loopback listener.
func (h *Handler) apiCompleteCodex(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackURL string `json:"callbackUrl"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	result, err := auth.CompleteCodexLogin(req.SessionID, req.CallbackURL)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	account := config.Account{
		ID:             auth.GenerateAccountID(),
		Email:          result.Email,
		AccessToken:    result.AccessToken,
		RefreshToken:   result.RefreshToken,
		CodexAccountID: result.AccountID,
		AuthMethod:     "codex",
		Provider:       "ChatGPT",
		ExpiresAt:      result.ExpiresAt,
		Enabled:        true,
	}
	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "id": account.ID})
}

// apiGetProviderModels returns the available models for a provider ("kiro" |
// "codex"): the union of models discovered from that provider's accounts plus
// operator-added custom models. ?provider= selects the provider (default kiro).
func (h *Handler) apiGetProviderModels(w http.ResponseWriter, r *http.Request) {
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	if provider == "" {
		provider = "kiro"
	}

	// For codex, "Load All" (refresh=1) queries the ChatGPT model catalog live per
	// account and caches it, so the list reflects real entitlements rather than a
	// stale cache. Kiro discovery is driven by the existing models-cache path.
	if provider == "codex" && strings.TrimSpace(r.URL.Query().Get("refresh")) != "" {
		for _, acc := range h.pool.GetAllAccounts() {
			if acc.AuthMethod == "codex" && acc.Enabled {
				a := acc
				h.refreshCodexModels(&a)
			}
		}
	}

	seen := map[string]bool{}
	var models []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[strings.ToLower(id)] {
			return
		}
		seen[strings.ToLower(id)] = true
		models = append(models, id)
	}

	for _, acc := range h.pool.GetAllAccounts() {
		isCodexAcc := acc.AuthMethod == "codex"
		if (provider == "codex") != isCodexAcc {
			continue
		}
		for _, m := range h.pool.GetModelList(acc.ID) {
			add(m)
		}
	}
	custom := config.GetCustomModels(provider)
	for _, m := range custom {
		add(m)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"provider": provider,
		"models":   models,
		"custom":   custom,
	})
}

// apiAddProviderModel adds a custom model id to a provider's list.
func (h *Handler) apiAddProviderModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	provider := strings.TrimSpace(req.Provider)
	model := strings.TrimSpace(req.Model)
	if provider == "" || model == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "provider and model are required"})
		return
	}
	if err := config.AddCustomModel(provider, model); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiRemoveProviderModel removes a custom model id from a provider's list.
func (h *Handler) apiRemoveProviderModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if err := config.RemoveCustomModel(strings.TrimSpace(req.Provider), strings.TrimSpace(req.Model)); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiUpdateCodexPrefix sets the model-name prefix that routes requests to codex.
func (h *Handler) apiUpdateCodexPrefix(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CodexRoutePrefix string `json:"codexRoutePrefix"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if err := config.UpdateCodexRoutePrefix(strings.TrimSpace(req.CodexRoutePrefix)); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiCancelCodex tears down an in-flight Codex sign-in, freeing the loopback port.
func (h *Handler) apiCancelCodex(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.SessionID != "" {
		auth.CancelCodexLogin(req.SessionID)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// splitProviderPrefix splits a model like "gpt/gpt-5.6" into ("gpt", "gpt-5.6").
// A model without a "/" yields an empty prefix and the model unchanged.
func splitProviderPrefix(model string) (prefix, rest string) {
	if i := strings.Index(model, "/"); i >= 0 {
		return model[:i], model[i+1:]
	}
	return "", model
}

// isCodexModel reports whether the model routes to a codex account, and returns
// the real model name with the prefix stripped.
func isCodexModel(model string) (bool, string) {
	prefix := strings.TrimSpace(config.GetCodexRoutePrefix())
	if prefix == "" {
		return false, model
	}
	p, rest := splitProviderPrefix(model)
	if strings.EqualFold(p, prefix) {
		return true, rest
	}
	return false, model
}

// selectCodexAccount returns the next enabled codex account from the pool, or
// nil if none is configured.
func (h *Handler) selectCodexAccount() *config.Account {
	for _, acc := range h.pool.GetAllAccounts() {
		if acc.AuthMethod == "codex" && acc.Enabled {
			a := acc
			return &a
		}
	}
	return nil
}

// handleCodexResponses forwards a /v1/responses request to the ChatGPT backend.
// rawBody is the original client request body; realModel is the model with the
// routing prefix already stripped.
func (h *Handler) handleCodexResponses(w http.ResponseWriter, r *http.Request, rawBody []byte, realModel, apiKeyID string) {
	account := h.selectCodexAccount()
	if account == nil {
		h.sendOpenAIError(w, 503, "server_error", "no ChatGPT (codex) account available")
		return
	}
	if err := h.ensureValidToken(account); err != nil {
		h.sendOpenAIError(w, 502, "server_error", fmt.Sprintf("codex token refresh failed: %v", err))
		return
	}

	// Rewrite the model field to the real (prefix-stripped) model.
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if mb, err := json.Marshal(realModel); err == nil {
		payload["model"] = mb
	}
	body, _ := json.Marshal(payload)

	// Attach the inbound request context so a client disconnect cancels the
	// upstream call instead of leaving it running.
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, codexBackendResponsesURL, bytes.NewReader(body))
	if err != nil {
		h.sendOpenAIError(w, 500, "server_error", err.Error())
		return
	}
	applyCodexHeaders(req, account, codexSessionID(account))

	resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		h.recordFailureWithDetails("responses", realModel, account.ID, apiKeyID, err)
		h.sendOpenAIError(w, 502, "server_error", fmt.Sprintf("codex upstream request failed: %v", err))
		return
	}
	defer resp.Body.Close()

	// Stream the upstream response straight back to the client.
	for k, vals := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "content-length" || lk == "connection" || lk == "transfer-encoding" {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
			if _, werr := w.Write(buf[:n]); werr != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				logger.Debugf("[Codex] stream read error: %v", readErr)
			}
			break
		}
	}
	// Attribute per-key usage on success. The Responses stream is passed through
	// verbatim, so estimate output tokens from the raw SSE payload.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		h.recordSuccessForApiKey(apiKeyID, 0, estimateApproxTokens(out.String()), 0)
	}
}

// codexResponsesInputItem is one item in the ChatGPT Responses "input" array.
type codexResponsesInputItem struct {
	Type    string                    `json:"type"`
	Role    string                    `json:"role,omitempty"`
	Content []codexResponsesContent   `json:"content,omitempty"`
}

type codexResponsesContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// chatMessagesToCodexInput converts Chat Completions messages to the Responses
// "input" array. Only text content is carried (tool calls are not translated).
func chatMessagesToCodexInput(messages []OpenAIMessage) []codexResponsesInputItem {
	items := make([]codexResponsesInputItem, 0, len(messages))
	for _, m := range messages {
		text := openAIMessageText(m.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		role := m.Role
		// Responses uses "input_text" for user/system and "output_text" for assistant.
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		items = append(items, codexResponsesInputItem{
			Type: "message",
			Role: role,
			Content: []codexResponsesContent{{Type: contentType, Text: text}},
		})
	}
	return items
}

// openAIMessageText best-effort extracts the text of a Chat Completions message
// content, which may be a plain string or an array of content parts.
func openAIMessageText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var sb strings.Builder
		for _, part := range v {
			if pm, ok := part.(map[string]interface{}); ok {
				if txt, ok := pm["text"].(string); ok {
					sb.WriteString(txt)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// handleCodexChat serves a /v1/chat/completions request from a codex account by
// converting it to the Responses format, forwarding to the ChatGPT backend, and
// translating the streamed Responses events back into Chat Completions chunks.
func (h *Handler) handleCodexChat(w http.ResponseWriter, r *http.Request, req *OpenAIRequest, realModel, apiKeyID string) {
	account := h.selectCodexAccount()
	if account == nil {
		h.sendOpenAIError(w, 503, "server_error", "no ChatGPT (codex) account available")
		return
	}
	if err := h.ensureValidToken(account); err != nil {
		h.sendOpenAIError(w, 502, "server_error", fmt.Sprintf("codex token refresh failed: %v", err))
		return
	}

	payload := map[string]interface{}{
		"model":  realModel,
		"input":  chatMessagesToCodexInput(req.Messages),
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	// Attach the inbound request context so a client disconnect cancels the
	// upstream call instead of leaving it running.
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, codexBackendResponsesURL, bytes.NewReader(body))
	if err != nil {
		h.sendOpenAIError(w, 500, "server_error", err.Error())
		return
	}
	applyCodexHeaders(upReq, account, codexSessionID(account))

	resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(upReq)
	if err != nil {
		h.recordFailureWithDetails("chat", realModel, account.ID, apiKeyID, err)
		h.sendOpenAIError(w, 502, "server_error", fmt.Sprintf("codex upstream request failed: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		h.recordFailureWithDetails("chat", realModel, account.ID, apiKeyID, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(errBody)))
		h.sendOpenAIError(w, resp.StatusCode, "server_error", fmt.Sprintf("codex upstream error: %s", string(errBody)))
		return
	}

	if req.Stream {
		h.streamCodexAsChatCompletions(w, resp.Body, realModel, apiKeyID)
	} else {
		h.collectCodexAsChatCompletion(w, resp.Body, realModel, apiKeyID)
	}
}

// codexResponsesEvent is the subset of a ChatGPT Responses SSE event we parse.
type codexResponsesEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
}

// streamCodexAsChatCompletions reads the Responses SSE stream and emits Chat
// Completions chunks. output_text.delta events become content deltas.
func (h *Handler) streamCodexAsChatCompletions(w http.ResponseWriter, upstream io.Reader, model, apiKeyID string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	chatID := "chatcmpl-" + uuid.New().String()
	sendChunk := func(delta map[string]interface{}, finish interface{}) {
		chunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"model":   model,
			"choices": []map[string]interface{}{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	sendChunk(map[string]interface{}{"role": "assistant"}, nil)

	var content strings.Builder
	scanner := bufio.NewScanner(upstream)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev codexResponsesEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if ev.Type == "response.output_text.delta" && ev.Delta != "" {
			content.WriteString(ev.Delta)
			sendChunk(map[string]interface{}{"content": ev.Delta}, nil)
		}
	}
	sendChunk(map[string]interface{}{}, "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	h.recordSuccessForApiKey(apiKeyID, 0, estimateApproxTokens(content.String()), 0)
}

// collectCodexAsChatCompletion buffers the full Responses stream and returns a
// single non-streaming Chat Completions response.
func (h *Handler) collectCodexAsChatCompletion(w http.ResponseWriter, upstream io.Reader, model, apiKeyID string) {
	var sb strings.Builder
	scanner := bufio.NewScanner(upstream)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev codexResponsesEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if ev.Type == "response.output_text.delta" {
			sb.WriteString(ev.Delta)
		}
	}
	content := sb.String()
	out := map[string]interface{}{
		"id":      "chatcmpl-" + uuid.New().String(),
		"object":  "chat.completion",
		"model":   model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       map[string]interface{}{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(out)
	h.recordSuccessForApiKey(apiKeyID, 0, estimateApproxTokens(content), 0)
}

// claudeMessagesToCodexInput converts a Claude request (system + messages) to the
// Responses "input" array. Only text content is carried.
func claudeMessagesToCodexInput(req *ClaudeRequest) []codexResponsesInputItem {
	items := make([]codexResponsesInputItem, 0, len(req.Messages)+1)
	if sys := claudeSystemText(req.System); sys != "" {
		items = append(items, codexResponsesInputItem{
			Type:    "message",
			Role:    "system",
			Content: []codexResponsesContent{{Type: "input_text", Text: sys}},
		})
	}
	for _, m := range req.Messages {
		text, _, _ := extractClaudeUserContent(m.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		contentType := "input_text"
		if m.Role == "assistant" {
			contentType = "output_text"
		}
		items = append(items, codexResponsesInputItem{
			Type:    "message",
			Role:    m.Role,
			Content: []codexResponsesContent{{Type: contentType, Text: text}},
		})
	}
	return items
}

// claudeSystemText flattens a Claude system field (string or []block) to text.
func claudeSystemText(system interface{}) string {
	switch v := system.(type) {
	case string:
		return v
	case []interface{}:
		var sb strings.Builder
		for _, b := range v {
			if bm, ok := b.(map[string]interface{}); ok {
				if txt, ok := bm["text"].(string); ok {
					sb.WriteString(txt)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// handleCodexClaude serves a /v1/messages (Anthropic) request from a codex account
// by converting to the Responses format, forwarding, and returning Anthropic output.
func (h *Handler) handleCodexClaude(w http.ResponseWriter, r *http.Request, req *ClaudeRequest, realModel, apiKeyID string) {
	account := h.selectCodexAccount()
	if account == nil {
		h.sendClaudeError(w, 503, "api_error", "no ChatGPT (codex) account available")
		return
	}
	if err := h.ensureValidToken(account); err != nil {
		h.sendClaudeError(w, 502, "api_error", fmt.Sprintf("codex token refresh failed: %v", err))
		return
	}

	payload := map[string]interface{}{
		"model":  realModel,
		"input":  claudeMessagesToCodexInput(req),
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, codexBackendResponsesURL, bytes.NewReader(body))
	if err != nil {
		h.sendClaudeError(w, 500, "api_error", err.Error())
		return
	}
	applyCodexHeaders(upReq, account, codexSessionID(account))

	resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(upReq)
	if err != nil {
		h.recordFailureWithDetails("messages", realModel, account.ID, apiKeyID, err)
		h.sendClaudeError(w, 502, "api_error", fmt.Sprintf("codex upstream request failed: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		h.recordFailureWithDetails("messages", realModel, account.ID, apiKeyID, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(errBody)))
		h.sendClaudeError(w, resp.StatusCode, "api_error", fmt.Sprintf("codex upstream error: %s", string(errBody)))
		return
	}

	// Collect the full text from the Responses stream, then emit as Anthropic.
	var content strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev codexResponsesEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if ev.Type == "response.output_text.delta" {
			content.WriteString(ev.Delta)
		}
	}
	text := content.String()

	if req.Stream {
		h.streamCodexAsClaude(w, text, realModel)
	} else {
		msgID := "msg_" + uuid.New().String()
		out := map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         realModel,
			"content":       []map[string]interface{}{{"type": "text", "text": text}},
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"usage":         map[string]interface{}{"input_tokens": 0, "output_tokens": estimateApproxTokens(text)},
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(out)
	}
	h.recordSuccessForApiKey(apiKeyID, 0, estimateApproxTokens(text), 0)
}

// streamCodexAsClaude emits the collected text as an Anthropic SSE message stream.
func (h *Handler) streamCodexAsClaude(w http.ResponseWriter, text, model string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	send := func(event string, data map[string]interface{}) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	msgID := "msg_" + uuid.New().String()
	send("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": msgID, "type": "message", "role": "assistant", "model": model,
			"content": []interface{}{}, "stop_reason": nil,
			"usage": map[string]interface{}{"input_tokens": 0, "output_tokens": 0},
		},
	})
	send("content_block_start", map[string]interface{}{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	})
	send("content_block_delta", map[string]interface{}{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]interface{}{"type": "text_delta", "text": text},
	})
	send("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]interface{}{"output_tokens": estimateApproxTokens(text)},
	})
	send("message_stop", map[string]interface{}{"type": "message_stop"})
}
