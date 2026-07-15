package bot

import (
	"bytes"
	"crypto/md5"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// cryptomusAPIBase is the Cryptomus merchant API root.
const cryptomusAPIBase = "https://api.cryptomus.com/v1"

// CryptomusClient talks to the Cryptomus merchant API. The payment API key
// both authenticates requests and signs/verifies webhook payloads.
type CryptomusClient struct {
	merchantID string
	apiKey     string
	http       *http.Client
}

// NewCryptomusClient builds a client with a 30s HTTP timeout.
func NewCryptomusClient(merchantID, apiKey string) *CryptomusClient {
	return &CryptomusClient{
		merchantID: merchantID,
		apiKey:     apiKey,
		http:       &http.Client{Timeout: 30 * time.Second},
	}
}

// sign computes the Cryptomus request/response signature:
// md5( base64(body) + apiKey ), hex-encoded.
func (c *CryptomusClient) sign(body []byte) string {
	payload := base64.StdEncoding.EncodeToString(body) + c.apiKey
	sum := md5.Sum([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// invoiceResult is the subset of the create-payment response we use.
type invoiceResult struct {
	URL    string `json:"url"`     // hosted payment page
	UUID   string `json:"uuid"`    // Cryptomus payment uuid
	OrderID string `json:"order_id"`
}

// CreateInvoice creates a payment and returns its hosted URL. amountUSD is the
// fiat price; Cryptomus lets the payer pick the crypto. orderID must be unique
// (we pass the internal order UUID) and callbackURL receives the webhook.
func (c *CryptomusClient) CreateInvoice(amountUSD float64, orderID, callbackURL string) (invoiceResult, error) {
	reqBody := map[string]interface{}{
		"amount":       fmt.Sprintf("%.2f", amountUSD),
		"currency":     "USD",
		"order_id":     orderID,
		"url_callback": callbackURL,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return invoiceResult{}, err
	}

	req, err := http.NewRequest(http.MethodPost, cryptomusAPIBase+"/payment", bytes.NewReader(body))
	if err != nil {
		return invoiceResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("merchant", c.merchantID)
	req.Header.Set("sign", c.sign(body))

	resp, err := c.http.Do(req)
	if err != nil {
		return invoiceResult{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return invoiceResult{}, fmt.Errorf("cryptomus create payment failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var out struct {
		Result invoiceResult `json:"result"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return invoiceResult{}, err
	}
	if out.Result.URL == "" {
		return invoiceResult{}, fmt.Errorf("cryptomus response missing payment url: %s", string(respBody))
	}
	return out.Result, nil
}

// WebhookPayload is the Cryptomus payment webhook body.
type WebhookPayload struct {
	Type          string `json:"type"`
	UUID          string `json:"uuid"`
	OrderID       string `json:"order_id"`
	Amount        string `json:"amount"`
	PaymentStatus string `json:"payment_status"`
	Sign          string `json:"sign"`
}

// VerifyWebhook recomputes the signature over the raw webhook body (with the
// "sign" field removed) and compares it to the provided signature. Returns the
// parsed payload on success.
func (c *CryptomusClient) VerifyWebhook(rawBody []byte) (WebhookPayload, error) {
	var generic map[string]interface{}
	if err := json.Unmarshal(rawBody, &generic); err != nil {
		return WebhookPayload{}, err
	}
	providedSign, _ := generic["sign"].(string)
	if providedSign == "" {
		return WebhookPayload{}, fmt.Errorf("webhook missing sign")
	}
	delete(generic, "sign")
	// Cryptomus signs md5(base64(json_without_sign) + apiKey), where the JSON is
	// produced by PHP json_encode. PHP does NOT escape <, >, & (Go's json.Marshal
	// does by default → < etc.), so disable HTML escaping or valid webhooks
	// would be rejected. Trailing newline from Encode is trimmed.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil {
		return WebhookPayload{}, err
	}
	signedBody := bytes.TrimRight(buf.Bytes(), "\n")
	// PHP json_encode escapes forward slashes by default; match that before hashing.
	signedBody = []byte(strings.ReplaceAll(string(signedBody), "/", "\\/"))

	expected := c.sign(signedBody)
	// Constant-time compare to avoid leaking the signature via timing.
	if subtle.ConstantTimeCompare([]byte(expected), []byte(providedSign)) != 1 {
		return WebhookPayload{}, fmt.Errorf("webhook signature mismatch")
	}

	var payload WebhookPayload
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return WebhookPayload{}, err
	}
	return payload, nil
}

// IsPaid reports whether a webhook status means the payment is settled.
func (p WebhookPayload) IsPaid() bool {
	return p.PaymentStatus == "paid" || p.PaymentStatus == "paid_over"
}
