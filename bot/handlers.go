package bot

import (
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"strconv"
	"strings"
)

// handleUpdate routes a Telegram update to the right handler.
func (b *Bot) handleUpdate(u tgUpdate) {
	if u.CallbackQuery != nil {
		b.handleCallback(u.CallbackQuery)
		return
	}
	if u.Message != nil && u.Message.Text != "" {
		b.handleMessage(u.Message)
	}
}

// handleMessage handles text commands.
func (b *Bot) handleMessage(m *tgMessage) {
	if m.Chat == nil || m.From == nil {
		return
	}
	text := strings.TrimSpace(m.Text)
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return
	}
	cmd := strings.ToLower(fields[0])
	// Strip a @botname suffix Telegram adds in groups.
	if i := strings.Index(cmd, "@"); i >= 0 {
		cmd = cmd[:i]
	}

	switch cmd {
	case "/start", "/help":
		b.cmdStart(m.Chat.ID)
	case "/buy":
		if len(fields) >= 2 {
			b.buyCustom(m.Chat.ID, m.From, fields[1])
		} else {
			b.cmdBuy(m.Chat.ID)
		}
	case "/mykeys":
		b.cmdMyKeys(m.Chat.ID, m.From.ID)
	default:
		b.sendMessage(m.Chat.ID, msgUnknown, nil)
	}
}

// handleCallback handles inline-button presses (package selection).
func (b *Bot) handleCallback(cb *tgCallbackQuery) {
	b.answerCallback(cb.ID)
	if cb.Message == nil || cb.Message.Chat == nil || cb.From == nil {
		return
	}
	data := cb.Data
	if strings.HasPrefix(data, "pkg:") {
		b.buyPackage(cb.Message.Chat.ID, cb.From, strings.TrimPrefix(data, "pkg:"))
	}
}

const (
	msgUnknown = "Unknown command. Use /buy to purchase credits or /mykeys to view your keys."
)

// cmdStart shows the welcome + command list.
func (b *Bot) cmdStart(chatID int64) {
	bc := config.GetBotConfig()
	var sb strings.Builder
	sb.WriteString("*Welcome!* Buy API credits below.\n\n")
	sb.WriteString("• /buy — choose a package or enter a custom amount\n")
	if bc.PricePerCredit > 0 && bc.MinCredits > 0 {
		sb.WriteString(fmt.Sprintf("   custom: `/buy <credits>` (min %g, $%.4f per credit)\n", bc.MinCredits, bc.PricePerCredit))
	}
	sb.WriteString("• /mykeys — view your purchased keys\n")
	b.sendMessage(chatID, sb.String(), nil)
}

// cmdBuy shows the fixed packages as inline buttons.
func (b *Bot) cmdBuy(chatID int64) {
	bc := config.GetBotConfig()
	if len(bc.Packages) == 0 && (bc.PricePerCredit <= 0 || bc.MinCredits <= 0) {
		b.sendMessage(chatID, "Purchasing is not configured yet. Please contact the admin.", nil)
		return
	}

	var rows [][]map[string]string
	for _, p := range bc.Packages {
		label := fmt.Sprintf("%s — %g credits ($%.2f)", p.Label, p.Credits, p.Price)
		rows = append(rows, []map[string]string{{"text": label, "callback_data": "pkg:" + p.ID}})
	}
	markup := map[string]interface{}{"inline_keyboard": rows}

	msg := "*Choose a package:*"
	if bc.PricePerCredit > 0 && bc.MinCredits > 0 {
		msg += fmt.Sprintf("\n\nOr buy a custom amount: `/buy <credits>` (min %g).", bc.MinCredits)
	}
	if len(rows) == 0 {
		b.sendMessage(chatID, msg, nil)
		return
	}
	b.sendMessage(chatID, msg, markup)
}

// buyPackage starts checkout for a fixed package id.
func (b *Bot) buyPackage(chatID int64, from *tgUser, pkgID string) {
	bc := config.GetBotConfig()
	for _, p := range bc.Packages {
		if p.ID == pkgID {
			b.startCheckout(chatID, from, p.Credits, p.Price)
			return
		}
	}
	b.sendMessage(chatID, "That package no longer exists. Use /buy to see current options.", nil)
}

// buyCustom starts checkout for a user-entered credit amount.
func (b *Bot) buyCustom(chatID int64, from *tgUser, arg string) {
	bc := config.GetBotConfig()
	if bc.PricePerCredit <= 0 || bc.MinCredits <= 0 {
		b.sendMessage(chatID, "Custom amounts are not available. Use /buy to see packages.", nil)
		return
	}
	credits, err := strconv.ParseFloat(arg, 64)
	if err != nil || credits <= 0 {
		b.sendMessage(chatID, "Please enter a valid number, e.g. `/buy 100`.", nil)
		return
	}
	if credits < bc.MinCredits {
		b.sendMessage(chatID, fmt.Sprintf("Minimum purchase is %g credits.", bc.MinCredits), nil)
		return
	}
	price := credits * bc.PricePerCredit
	b.startCheckout(chatID, from, credits, price)
}

// startCheckout creates an order + Cryptomus invoice and sends the pay link.
func (b *Bot) startCheckout(chatID int64, from *tgUser, credits, price float64) {
	b.mu.RLock()
	crypto := b.crypto
	baseURL := strings.TrimRight(b.baseURL, "/")
	b.mu.RUnlock()

	if crypto == nil {
		b.sendMessage(chatID, "Payments are not configured yet. Please contact the admin.", nil)
		return
	}
	if baseURL == "" {
		b.sendMessage(chatID, "Payment callback is not configured. Please contact the admin.", nil)
		return
	}

	name := from.Username
	if name == "" {
		name = from.FirstName
	}
	order, err := b.store.Create(from.ID, name, credits, price)
	if err != nil {
		logger.Warnf("[Bot] create order failed: %v", err)
		b.sendMessage(chatID, "Could not create your order. Please try again.", nil)
		return
	}

	callbackURL := baseURL + "/webhook/cryptomus"
	inv, err := crypto.CreateInvoice(price, order.ID, callbackURL)
	if err != nil {
		logger.Warnf("[Bot] create invoice failed: %v", err)
		b.store.Update(order.ID, func(o *Order) { o.Status = OrderFailed })
		b.sendMessage(chatID, "Could not create the payment invoice. Please try again later.", nil)
		return
	}
	b.store.Update(order.ID, func(o *Order) { o.InvoiceURL = inv.URL })

	msg := fmt.Sprintf(
		"*Order created*\n\nCredits: *%g*\nPrice: *$%.2f*\n\n[Pay now](%s)\n\nYour API key will be delivered here automatically once payment is confirmed.",
		credits, price, inv.URL,
	)
	b.sendMessage(chatID, msg, nil)
}

// cmdMyKeys lists a buyer's paid orders and their keys.
func (b *Bot) cmdMyKeys(chatID, telegramID int64) {
	orders := b.store.ListByTelegram(telegramID)
	if len(orders) == 0 {
		b.sendMessage(chatID, "You have no orders yet. Use /buy to purchase credits.", nil)
		return
	}
	var sb strings.Builder
	sb.WriteString("*Your orders:*\n\n")
	for _, o := range orders {
		switch o.Status {
		case OrderPaid:
			sb.WriteString(fmt.Sprintf("✅ %g credits — `%s`\n", o.Credits, o.ApiKeyValue))
		case OrderPending:
			sb.WriteString(fmt.Sprintf("⏳ %g credits — awaiting payment\n", o.Credits))
		default:
			sb.WriteString(fmt.Sprintf("❌ %g credits — failed\n", o.Credits))
		}
	}
	b.sendMessage(chatID, sb.String(), nil)
}

// FulfillOrder is called by the Cryptomus webhook when a payment settles. It
// mints the key, records it on the order, and notifies the buyer. Idempotent:
// a duplicate webhook for an already-paid order is ignored.
func (b *Bot) FulfillOrder(orderID, paidAmount string) error {
	// Defensive amount check: the webhook is signature-verified, but confirm the
	// settled amount covers the order price before minting, so a malformed/mismatched
	// paid callback can't grant credits for less than charged. Empty/unparseable
	// amount is tolerated (Cryptomus only reports paid/paid_over for settled invoices).
	if pa, err := strconv.ParseFloat(strings.TrimSpace(paidAmount), 64); err == nil && pa > 0 {
		if o, ok := b.store.Get(orderID); ok && o.AmountUSD > 0 && pa+0.01 < o.AmountUSD {
			return fmt.Errorf("paid amount %.2f is less than order amount %.2f", pa, o.AmountUSD)
		}
	}

	// Atomically claim the order (pending → paid) so a duplicate/retried Cryptomus
	// webhook can never mint a second key for the same payment. Only the first
	// caller gets claimed=true; the rest short-circuit here.
	order, claimed, err := b.store.ClaimForFulfillment(orderID)
	if err != nil {
		return fmt.Errorf("claim order: %w", err)
	}
	if !claimed {
		return nil // already fulfilled by an earlier webhook
	}

	keyValue, keyID, err := b.minter(fmt.Sprintf("tg:%d order:%s", order.TelegramID, order.ID), order.Credits)
	if err != nil {
		// Roll the claim back to pending so a legitimate retry can mint later,
		// rather than leaving a paid order with no key.
		b.store.Update(orderID, func(o *Order) {
			o.Status = OrderPending
			o.PaidAt = 0
		})
		return fmt.Errorf("mint key: %w", err)
	}

	updated, _, err := b.store.Update(orderID, func(o *Order) {
		o.ApiKeyID = keyID
		o.ApiKeyValue = keyValue
	})
	if err != nil {
		return fmt.Errorf("update order: %w", err)
	}

	msg := fmt.Sprintf(
		"*Payment confirmed!* 🎉\n\nCredits: *%g*\n\nYour API key:\n`%s`\n\nKeep it safe — it grants access to your purchased credits.",
		updated.Credits, keyValue,
	)
	if err := b.SendMessage(updated.TelegramID, msg); err != nil {
		logger.Warnf("[Bot] failed to deliver key to %d: %v", updated.TelegramID, err)
	}
	return nil
}
