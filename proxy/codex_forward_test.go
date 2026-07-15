package proxy

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParseCodexModelIDsIncludesChatGPTOnlyModels(t *testing.T) {
	body := []byte(`{"models":[{"slug":"gpt-5.6-sol","supported_in_api":false},{"id":"gpt-5.4","supported_in_api":true},{"slug":"gpt-5.6-sol"}]}`)
	got, err := parseCodexModelIDs(body)
	if err != nil {
		t.Fatalf("parseCodexModelIDs returned error: %v", err)
	}
	want := []string{"gpt-5.6-sol", "gpt-5.4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("model IDs = %v, want %v", got, want)
	}
}

func TestPrepareCodexResponsesPayloadForcesRequiredFields(t *testing.T) {
	body, err := prepareCodexResponsesPayload([]byte(`{"model":"gpt/gpt-5.6-sol","input":[],"store":true,"stream":false,"previous_response_id":"resp_old"}`), "gpt-5.6-sol")
	if err != nil {
		t.Fatalf("prepareCodexResponsesPayload returned error: %v", err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("parse prepared payload: %v", err)
	}
	if payload["model"] != "gpt-5.6-sol" {
		t.Fatalf("model = %v", payload["model"])
	}
	if payload["store"] != false {
		t.Fatalf("store = %v, want false", payload["store"])
	}
	if payload["stream"] != true {
		t.Fatalf("stream = %v, want true", payload["stream"])
	}
	if _, exists := payload["previous_response_id"]; exists {
		t.Fatalf("previous_response_id was not removed")
	}
}

func TestNormalizeCodexTestModel(t *testing.T) {
	for input, want := range map[string]string{
		"gpt-5.6-sol":     "gpt-5.6-sol",
		"gpt/gpt-5.6-sol": "gpt-5.6-sol",
		" cx/gpt-5.4 ":    "gpt-5.4",
	} {
		if got := normalizeCodexTestModel(input); got != want {
			t.Fatalf("normalizeCodexTestModel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCodexSubscription(t *testing.T) {
	subscriptionType, title := codexSubscription("plus")
	if subscriptionType != "PLUS" || title != "ChatGPT Plus" {
		t.Fatalf("subscription = (%q, %q)", subscriptionType, title)
	}
}
