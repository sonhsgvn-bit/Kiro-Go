package auth

import (
	"encoding/base64"
	"testing"
)

func TestParseCodexIDTokenIncludesPlanType(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"plus@example.com","https://api.openai.com/auth":{"chatgpt_account_id":"acct_123","chatgpt_plan_type":"plus"}}`))
	accountID, email, planType := parseCodexIDToken("header." + payload + ".signature")
	if accountID != "acct_123" {
		t.Fatalf("account ID = %q", accountID)
	}
	if email != "plus@example.com" {
		t.Fatalf("email = %q", email)
	}
	if planType != "plus" {
		t.Fatalf("plan type = %q", planType)
	}
}

func TestParseCodexIDTokenFallsBackToTopLevelClaims(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"pro@example.com","account_id":"acct_top","plan_type":"pro"}`))
	accountID, email, planType := parseCodexIDToken("header." + payload + ".signature")
	if accountID != "acct_top" || email != "pro@example.com" || planType != "pro" {
		t.Fatalf("claims = (%q, %q, %q)", accountID, email, planType)
	}
}
