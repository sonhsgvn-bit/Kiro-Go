package auth

// codex.go implements the OpenAI (ChatGPT / Codex) OAuth2 PKCE flow used to
// sign in with a ChatGPT account — the same flow the Codex CLI uses. The
// resulting access token is the runtime bearer for the ChatGPT backend
// (chatgpt.com/backend-api/codex) and is refreshed against the OpenAI token
// endpoint. See codex_login.go for the loopback listener that captures the code.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	codexAuthURL      = "https://auth.openai.com/oauth/authorize"
	codexTokenURL     = "https://auth.openai.com/oauth/token"
	codexClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexRedirectURI  = "http://localhost:1455/auth/callback"
	codexRedirectPort = "1455"
	codexCallbackPath = "/auth/callback"
	codexScope        = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	codexOriginator   = "codex_cli_rs"
)

// CodexTokenData is the resolved credential from an OAuth exchange or refresh.
type CodexTokenData struct {
	IDToken      string
	AccessToken  string
	RefreshToken string
	AccountID    string
	Email        string
	PlanType     string
	ExpiresAt    int64 // Unix seconds
}

// codexGenerateAuthURL builds the authorization-code+PKCE URL to open in the browser.
func codexGenerateAuthURL(state, challenge string) string {
	params := [][2]string{
		{"response_type", "code"},
		{"client_id", codexClientID},
		{"redirect_uri", codexRedirectURI},
		{"scope", codexScope},
		{"code_challenge", challenge},
		{"code_challenge_method", "S256"},
		{"id_token_add_organizations", "true"},
		{"codex_cli_simplified_flow", "true"},
		{"state", state},
		{"originator", codexOriginator},
	}
	query := make([]string, 0, len(params))
	for _, param := range params {
		value := strings.ReplaceAll(url.QueryEscape(param[1]), "+", "%20")
		query = append(query, param[0]+"="+value)
	}
	return codexAuthURL + "?" + strings.Join(query, "&")
}

// exchangeCodexCode swaps an authorization code (+PKCE verifier) for tokens.
func exchangeCodexCode(ctx context.Context, client *http.Client, code, verifier string) (*CodexTokenData, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {codexClientID},
		"code":          {code},
		"redirect_uri":  {codexRedirectURI},
		"code_verifier": {verifier},
	}
	return codexTokenRequest(ctx, client, data)
}

// refreshCodexTokenRequest refreshes an access token using the refresh_token grant.
func refreshCodexTokenRequest(ctx context.Context, client *http.Client, refreshToken string) (*CodexTokenData, error) {
	data := url.Values{
		"client_id":     {codexClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {"openid profile email"},
	}
	return codexTokenRequest(ctx, client, data)
}

// codexTokenRequest performs a token endpoint POST and parses the response.
func codexTokenRequest(ctx context.Context, client *http.Client, data url.Values) (*CodexTokenData, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build codex token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex token request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("codex token exchange failed (status %d): %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parse codex token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("codex token response missing access_token")
	}

	accountID, email, planType := parseCodexIDToken(tokenResp.IDToken)
	accessAccountID, accessEmail, accessPlanType := parseCodexIDToken(tokenResp.AccessToken)
	if accountID == "" {
		accountID = accessAccountID
	}
	if email == "" {
		email = accessEmail
	}
	if planType == "" {
		planType = accessPlanType
	}
	expiresIn := tokenResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return &CodexTokenData{
		IDToken:      tokenResp.IDToken,
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		AccountID:    accountID,
		Email:        email,
		PlanType:     planType,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second).Unix(),
	}, nil
}

// RefreshCodexTokenData refreshes a Codex token and preserves the account
// metadata carried by the returned ID/access token claims.
func RefreshCodexTokenData(refreshToken string, client *http.Client) (*CodexTokenData, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("codex refresh requires a refresh token")
	}
	return refreshCodexTokenRequest(context.Background(), client, refreshToken)
}

// RefreshCodexToken refreshes a Codex access token. Returns access token,
// refresh token (may be empty if unchanged), and expiry (Unix seconds).
func RefreshCodexToken(refreshToken string, client *http.Client) (string, string, int64, error) {
	td, err := RefreshCodexTokenData(refreshToken, client)
	if err != nil {
		return "", "", 0, err
	}
	return td.AccessToken, td.RefreshToken, td.ExpiresAt, nil
}

// parseCodexIDToken extracts the ChatGPT account id, email, and plan from the JWT
// id_token payload (no signature verification: used only to label the account).
func parseCodexIDToken(idToken string) (accountID, email, planType string) {
	parts := strings.Split(strings.TrimSpace(idToken), ".")
	if len(parts) < 2 {
		return "", "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if payload, err = base64.StdEncoding.DecodeString(parts[1]); err != nil {
			return "", "", ""
		}
	}
	var claims struct {
		Email     string `json:"email"`
		AccountID string `json:"account_id"`
		PlanType  string `json:"plan_type"`
		Auth      struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
			ChatGPTPlanType  string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", ""
	}
	accountID = strings.TrimSpace(claims.Auth.ChatGPTAccountID)
	if accountID == "" {
		accountID = strings.TrimSpace(claims.AccountID)
	}
	planType = strings.TrimSpace(claims.Auth.ChatGPTPlanType)
	if planType == "" {
		planType = strings.TrimSpace(claims.PlanType)
	}
	return accountID, strings.TrimSpace(claims.Email), planType
}
