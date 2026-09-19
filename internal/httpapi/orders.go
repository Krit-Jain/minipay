package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

type CreateOrderRequest struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type Order struct {
	ID        string    `json:"id"`
	Amount    int64     `json:"amount"`
	Currency  string    `json:"currency"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

var (
	mu     sync.Mutex
	orders = make(map[string]Order)
)

func newID() (string, error) {
	var b [16]byte

	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(b[:]), nil
}

func CreateOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(
			w,
			"method not allowed",
			http.StatusMethodNotAllowed,
		)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var req CreateOrderRequest

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&req); err != nil {
		http.Error(
			w,
			"invalid JSON",
			http.StatusBadRequest,
		)
		return
	}

	if req.Amount <= 0 {
		http.Error(
			w,
			"amount must be positive",
			http.StatusBadRequest,
		)
		return
	}

	if req.Currency != "INR" {
		http.Error(
			w,
			"unsupported currency",
			http.StatusBadRequest,
		)
		return
	}

	id, err := newID()
	if err != nil {
		http.Error(
			w,
			"could not create order",
			http.StatusInternalServerError,
		)
		return
	}

	order := Order{
		ID:        id,
		Amount:    req.Amount,
		Currency:  req.Currency,
		Status:    "CREATED",
		CreatedAt: time.Now().UTC(),
	}

	mu.Lock()
	orders[id] = order
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)

	if err := json.NewEncoder(w).Encode(order); err != nil {
		return
	}
}
