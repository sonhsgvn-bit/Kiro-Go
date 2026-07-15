package bot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"sync"
	"time"
)

// KeyMinter mints a paid API key. Implemented by the proxy layer so the bot
// package does not import proxy (avoids an import cycle). It returns the
// cleartext key value and the key entry id.
type KeyMinter func(name string, credits float64) (keyValue, keyID string, err error)

// Bot is the Telegram sales bot. It long-polls for updates and drives the
// purchase flow, minting keys through the injected KeyMinter after Cryptomus
// confirms payment via the webhook.
type Bot struct {
	store  *Store
	minter KeyMinter

	mu      sync.RWMutex
	token   string
	crypto  *CryptomusClient
	baseURL string // public https base for the callback

	http    *http.Client
	stopCh  chan struct{}
	running bool
}

// singleton lets the proxy webhook reach the running bot without threading it
// through every call site. Set by Start, cleared by Stop.
var (
	singleton   *Bot
	singletonMu sync.RWMutex
)

// Current returns the running bot, or nil if the bot is not started.
func Current() *Bot {
	singletonMu.RLock()
	defer singletonMu.RUnlock()
	return singleton
}

// New creates a bot backed by store, minting keys through minter.
func New(store *Store, minter KeyMinter) *Bot {
	return &Bot{
		store:  store,
		minter: minter,
		http:   &http.Client{Timeout: 65 * time.Second}, // > long-poll timeout
	}
}

// Start (re)loads the bot config and, if enabled and configured, launches the
// polling loop. Safe to call repeatedly (e.g. after a config change): it stops
// any existing loop first. A disabled or unconfigured bot is a no-op.
func (b *Bot) Start() {
	b.Stop()

	bc := config.GetBotConfig()
	if !bc.Enabled || bc.TelegramToken == "" {
		logger.Infof("[Bot] disabled or missing Telegram token; not starting")
		return
	}

	b.mu.Lock()
	b.token = bc.TelegramToken
	b.baseURL = bc.PublicBaseURL
	if bc.CryptomusMerchantID != "" && bc.CryptomusAPIKey != "" {
		b.crypto = NewCryptomusClient(bc.CryptomusMerchantID, bc.CryptomusAPIKey)
	} else {
		b.crypto = nil
	}
	b.stopCh = make(chan struct{})
	b.running = true
	stopCh := b.stopCh
	b.mu.Unlock()

	singletonMu.Lock()
	singleton = b
	singletonMu.Unlock()

	go b.pollLoop(stopCh)
	logger.Infof("[Bot] started Telegram polling loop")
}

// Stop halts the polling loop if running.
func (b *Bot) Stop() {
	b.mu.Lock()
	if b.running && b.stopCh != nil {
		close(b.stopCh)
		b.running = false
	}
	b.mu.Unlock()

	// Clear the singleton if it still points at this bot, so the public Cryptomus
	// webhook (via Current()) cannot mint keys through a stopped/disabled bot.
	singletonMu.Lock()
	if singleton == b {
		singleton = nil
	}
	singletonMu.Unlock()
}

// pollLoop repeatedly calls getUpdates with long polling until stopCh closes.
func (b *Bot) pollLoop(stopCh chan struct{}) {
	var offset int64
	for {
		select {
		case <-stopCh:
			return
		default:
		}
		updates, err := b.getUpdates(offset)
		if err != nil {
			// Back off briefly on transient errors so a broken token or network
			// blip doesn't spin the CPU.
			select {
			case <-stopCh:
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			b.handleUpdate(u)
		}
	}
}

// --- Telegram API types + calls ---------------------------------------------

type tgUpdate struct {
	UpdateID      int64           `json:"update_id"`
	Message       *tgMessage      `json:"message"`
	CallbackQuery *tgCallbackQuery `json:"callback_query"`
}

type tgMessage struct {
	MessageID int64   `json:"message_id"`
	From      *tgUser `json:"from"`
	Chat      *tgChat `json:"chat"`
	Text      string  `json:"text"`
}

type tgCallbackQuery struct {
	ID      string     `json:"id"`
	From    *tgUser    `json:"from"`
	Message *tgMessage `json:"message"`
	Data    string     `json:"data"`
}

type tgUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

type tgChat struct {
	ID int64 `json:"id"`
}

// getUpdates long-polls Telegram for new updates.
func (b *Bot) getUpdates(offset int64) ([]tgUpdate, error) {
	b.mu.RLock()
	token := b.token
	b.mu.RUnlock()

	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?timeout=50&offset=%d", token, offset)
	resp, err := b.http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("getUpdates status %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		OK     bool       `json:"ok"`
		Result []tgUpdate `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.Result, nil
}

// SendMessage sends a text message (Markdown) to a chat. Exported so the
// Cryptomus webhook can notify a buyer once their key is minted.
func (b *Bot) SendMessage(chatID int64, text string) error {
	return b.sendMessage(chatID, text, nil)
}

// sendMessage sends text with an optional inline keyboard.
func (b *Bot) sendMessage(chatID int64, text string, replyMarkup interface{}) error {
	b.mu.RLock()
	token := b.token
	b.mu.RUnlock()
	if token == "" {
		return fmt.Errorf("bot token not configured")
	}

	payload := map[string]interface{}{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "Markdown",
	}
	if replyMarkup != nil {
		payload["reply_markup"] = replyMarkup
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	resp, err := b.http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sendMessage status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// answerCallback acknowledges a callback query so the client stops its spinner.
func (b *Bot) answerCallback(id string) {
	b.mu.RLock()
	token := b.token
	b.mu.RUnlock()
	payload := map[string]interface{}{"callback_query_id": id}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", token)
	resp, err := b.http.Post(url, "application/json", bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
	}
}
