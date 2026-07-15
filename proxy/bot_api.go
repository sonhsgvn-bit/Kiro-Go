package proxy

import (
	"encoding/json"
	"io"
	"kiro-go/bot"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
)

// mintPaidApiKey creates an enabled API key with the given credit quota and
// returns its cleartext value and id. Used by the bot to fulfill paid orders.
func mintPaidApiKey(name string, credits float64) (string, string, error) {
	entry, err := config.AddApiKey(config.ApiKeyEntry{
		Name:        name,
		Key:         config.GenerateApiKeyValue(),
		Enabled:     true,
		CreditLimit: credits,
	})
	if err != nil {
		return "", "", err
	}
	return entry.Key, entry.ID, nil
}

// handleCryptomusWebhook is the PUBLIC payment callback. It verifies the
// Cryptomus signature, and on a settled payment fulfills the matching order
// (mints a key + notifies the buyer). No admin auth: authenticity comes from
// the signature check against the configured API key.
func (h *Handler) handleCryptomusWebhook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	b := bot.Current()
	if b == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "bot not running"})
		return
	}

	bc := config.GetBotConfig()
	if bc.CryptomusAPIKey == "" {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "payments not configured"})
		return
	}

	rawBody, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "read body failed"})
		return
	}

	crypto := bot.NewCryptomusClient(bc.CryptomusMerchantID, bc.CryptomusAPIKey)
	payload, err := crypto.VerifyWebhook(rawBody)
	if err != nil {
		logger.Warnf("[Bot] webhook verification failed: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid signature"})
		return
	}

	if payload.IsPaid() {
		if err := b.FulfillOrder(payload.OrderID, payload.Amount); err != nil {
			logger.Warnf("[Bot] fulfill order %s failed: %v", payload.OrderID, err)
			// Return 500 so Cryptomus retries the callback.
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "fulfillment failed"})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// --- Bot admin API (guarded by the admin panel session) --------------------

// botConfigView masks secrets for display: it reports whether each secret is
// set without ever returning the stored value.
type botConfigView struct {
	Enabled             bool                `json:"enabled"`
	TelegramTokenSet    bool                `json:"telegramTokenSet"`
	CryptomusMerchantID string              `json:"cryptomusMerchantId"`
	CryptomusAPIKeySet  bool                `json:"cryptomusApiKeySet"`
	PublicBaseURL       string              `json:"publicBaseUrl"`
	PricePerCredit      float64             `json:"pricePerCredit"`
	MinCredits          float64             `json:"minCredits"`
	Packages            []config.BotPackage `json:"packages"`
}

func (h *Handler) apiGetBotConfig(w http.ResponseWriter, r *http.Request) {
	bc := config.GetBotConfig()
	json.NewEncoder(w).Encode(botConfigView{
		Enabled:             bc.Enabled,
		TelegramTokenSet:    bc.TelegramToken != "",
		CryptomusMerchantID: bc.CryptomusMerchantID,
		CryptomusAPIKeySet:  bc.CryptomusAPIKey != "",
		PublicBaseURL:       bc.PublicBaseURL,
		PricePerCredit:      bc.PricePerCredit,
		MinCredits:          bc.MinCredits,
		Packages:            bc.Packages,
	})
}

// apiUpdateBotConfig saves the bot config and restarts the bot. Secret fields
// (telegramToken, cryptomusApiKey) are only overwritten when a non-empty value
// is supplied, so the UI can omit them to keep the stored secret unchanged.
func (h *Handler) apiUpdateBotConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled             bool                `json:"enabled"`
		TelegramToken       *string             `json:"telegramToken"`
		CryptomusMerchantID string              `json:"cryptomusMerchantId"`
		CryptomusAPIKey     *string             `json:"cryptomusApiKey"`
		PublicBaseURL       string              `json:"publicBaseUrl"`
		PricePerCredit      float64             `json:"pricePerCredit"`
		MinCredits          float64             `json:"minCredits"`
		Packages            []config.BotPackage `json:"packages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// Start from the existing config so omitted secrets are preserved.
	bc := config.GetBotConfig()
	bc.Enabled = req.Enabled
	bc.CryptomusMerchantID = strings.TrimSpace(req.CryptomusMerchantID)
	bc.PublicBaseURL = strings.TrimSpace(req.PublicBaseURL)
	bc.PricePerCredit = req.PricePerCredit
	bc.MinCredits = req.MinCredits
	bc.Packages = req.Packages
	if req.TelegramToken != nil && strings.TrimSpace(*req.TelegramToken) != "" {
		bc.TelegramToken = strings.TrimSpace(*req.TelegramToken)
	}
	if req.CryptomusAPIKey != nil && strings.TrimSpace(*req.CryptomusAPIKey) != "" {
		bc.CryptomusAPIKey = strings.TrimSpace(*req.CryptomusAPIKey)
	}

	if err := config.UpdateBotConfig(bc); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Apply the change immediately: (re)start or stop the bot.
	if b := bot.Current(); b != nil {
		b.Start() // Start reloads config; a disabled config makes it a no-op after stopping.
	} else if h.bot != nil {
		h.bot.Start()
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiListBotOrders returns all orders (newest first) with keys masked.
func (h *Handler) apiListBotOrders(w http.ResponseWriter, r *http.Request) {
	if h.bot == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"orders": []interface{}{}})
		return
	}
	orders := h.botStore.List()
	type orderView struct {
		ID           string  `json:"id"`
		TelegramID   int64   `json:"telegramId"`
		TelegramName string  `json:"telegramName"`
		Credits      float64 `json:"credits"`
		AmountUSD    float64 `json:"amountUsd"`
		Status       string  `json:"status"`
		ApiKeyMasked string  `json:"apiKeyMasked,omitempty"`
		CreatedAt    int64   `json:"createdAt"`
		PaidAt       int64   `json:"paidAt,omitempty"`
	}
	out := make([]orderView, 0, len(orders))
	for _, o := range orders {
		out = append(out, orderView{
			ID:           o.ID,
			TelegramID:   o.TelegramID,
			TelegramName: o.TelegramName,
			Credits:      o.Credits,
			AmountUSD:    o.AmountUSD,
			Status:       string(o.Status),
			ApiKeyMasked: config.MaskApiKey(o.ApiKeyValue),
			CreatedAt:    o.CreatedAt,
			PaidAt:       o.PaidAt,
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"orders": out})
}

// apiBotStats aggregates revenue and customer stats from paid orders.
func (h *Handler) apiBotStats(w http.ResponseWriter, r *http.Request) {
	if h.bot == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"totalOrders": 0, "paidOrders": 0, "revenueUsd": 0.0,
			"creditsSold": 0.0, "customers": 0,
		})
		return
	}
	orders := h.botStore.List()
	var paid, total int
	var revenue, creditsSold float64
	customers := map[int64]bool{}
	for _, o := range orders {
		total++
		if o.Status == "paid" {
			paid++
			revenue += o.AmountUSD
			creditsSold += o.Credits
			customers[o.TelegramID] = true
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"totalOrders": total,
		"paidOrders":  paid,
		"revenueUsd":  revenue,
		"creditsSold": creditsSold,
		"customers":   len(customers),
	})
}
