package bot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// OrderStatus is the lifecycle state of a purchase.
type OrderStatus string

const (
	OrderPending   OrderStatus = "pending"   // invoice created, awaiting payment
	OrderPaid      OrderStatus = "paid"       // payment confirmed, key minted
	OrderFailed    OrderStatus = "failed"     // payment failed / cancelled / expired
)

// Order records one purchase made through the Telegram bot.
type Order struct {
	ID           string      `json:"id"`                   // internal order UUID (also the Cryptomus order_id)
	TelegramID   int64       `json:"telegramId"`           // buyer's Telegram user id
	TelegramName string      `json:"telegramName,omitempty"` // buyer's @username or first name
	Credits      float64     `json:"credits"`              // credits purchased
	AmountUSD    float64     `json:"amountUsd"`            // price charged in USD
	Status       OrderStatus `json:"status"`
	InvoiceURL   string      `json:"invoiceUrl,omitempty"` // Cryptomus hosted payment page
	ApiKeyID     string      `json:"apiKeyId,omitempty"`   // minted key entry id (set when paid)
	ApiKeyValue  string      `json:"apiKeyValue,omitempty"` // cleartext key (shown to buyer once)
	CreatedAt    int64       `json:"createdAt"`
	PaidAt       int64       `json:"paidAt,omitempty"`
}

// Store persists orders to a JSON file, guarded by a mutex.
type Store struct {
	path   string
	mu     sync.RWMutex
	orders []Order
}

// NewStore loads orders from path (creating an empty store if the file is absent).
func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &s.orders); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// nowUnix returns the current Unix time in seconds.
func nowUnix() int64 { return time.Now().Unix() }

// saveLocked writes the current orders to disk. Caller must hold s.mu.
func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.orders, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}

// Create records a new pending order and returns it.
func (s *Store) Create(telegramID int64, telegramName string, credits, amountUSD float64) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := Order{
		ID:           uuid.New().String(),
		TelegramID:   telegramID,
		TelegramName: telegramName,
		Credits:      credits,
		AmountUSD:    amountUSD,
		Status:       OrderPending,
		CreatedAt:    time.Now().Unix(),
	}
	s.orders = append(s.orders, o)
	if err := s.saveLocked(); err != nil {
		s.orders = s.orders[:len(s.orders)-1]
		return Order{}, err
	}
	return o, nil
}

// Get returns a copy of the order with the given id, or false if not found.
func (s *Store) Get(id string) (Order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, o := range s.orders {
		if o.ID == id {
			return o, true
		}
	}
	return Order{}, false
}

// ClaimForFulfillment atomically transitions a pending order to paid so only the
// first concurrent/duplicate webhook proceeds to mint a key. It returns claimed=true
// exactly once per order; a second call (already paid) returns claimed=false. The
// order is persisted with Status=paid + PaidAt before returning so a crash between
// claim and mint does not re-mint (the key fields are filled in by MarkFulfilled).
func (s *Store) ClaimForFulfillment(id string) (order Order, claimed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.orders {
		if s.orders[i].ID != id {
			continue
		}
		if s.orders[i].Status == OrderPaid {
			return s.orders[i], false, nil // already claimed/fulfilled
		}
		s.orders[i].Status = OrderPaid
		s.orders[i].PaidAt = nowUnix()
		if e := s.saveLocked(); e != nil {
			// Roll back the claim so a transient disk error can be retried.
			s.orders[i].Status = OrderPending
			s.orders[i].PaidAt = 0
			return Order{}, false, e
		}
		return s.orders[i], true, nil
	}
	return Order{}, false, fmt.Errorf("order %s not found", id)
}

// Update applies mut to the order with the given id and persists the change.
func (s *Store) Update(id string, mut func(*Order)) (Order, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.orders {
		if s.orders[i].ID == id {
			mut(&s.orders[i])
			if err := s.saveLocked(); err != nil {
				return Order{}, false, err
			}
			return s.orders[i], true, nil
		}
	}
	return Order{}, false, nil
}

// List returns all orders, newest first.
func (s *Store) List() []Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Order, len(s.orders))
	copy(out, s.orders)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// ListByTelegram returns a buyer's orders, newest first.
func (s *Store) ListByTelegram(telegramID int64) []Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Order
	for _, o := range s.orders {
		if o.TelegramID == telegramID {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}
