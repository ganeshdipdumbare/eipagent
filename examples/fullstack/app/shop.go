package app

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Shop is the demo's business data: orders, help-center articles and an
// outbox. A real app would call its own services here.
type Shop struct {
	mu      sync.Mutex
	orders  map[string]*Order
	outbox  []Email
	refunds int
}

type Order struct {
	ID        string  `json:"id"`
	Item      string  `json:"item"`
	Total     float64 `json:"total"`
	Status    string  `json:"status"`
	Detail    string  `json:"detail"`
	Customer  string  `json:"customer"`
	Email     string  `json:"email"`
	Card      string  `json:"card"`
	Refunded  float64 `json:"refunded"`
	RefundRef string  `json:"refund_ref,omitempty"`
}

type Email struct {
	To      string    `json:"to"`
	Subject string    `json:"subject"`
	Body    string    `json:"body"`
	At      time.Time `json:"at"`
}

type Article struct {
	Title, Body string
	Keywords    []string
}

var articles = []Article{
	{"Returns and refunds", "You can return any item within 30 days of delivery for a full refund. Returns are free. Refunds reach the original card within 5 to 7 business days.", []string{"return", "refund", "policy", "money back"}},
	{"Shipping times", "Orders ship within 1 business day. Standard delivery takes 3 to 5 business days. If a parcel shows no tracking update for 5 days, we treat it as lost and refund or resend it.", []string{"ship", "delivery", "late", "lost", "tracking"}},
	{"Damaged items", "If an item arrives damaged, send a photo within 14 days and we refund it right away. You don't need to send it back.", []string{"broken", "damaged", "cracked"}},
}

func NewShop() *Shop {
	return &Shop{orders: map[string]*Order{
		"1042": {ID: "1042", Item: "Ceramic pour-over mug", Total: 24.00, Status: "delivered", Detail: "Delivered on Oct 24", Customer: "Maya Chen", Email: "maya.chen@example.com", Card: "4242"},
		"1077": {ID: "1077", Item: "Noise-cancelling headphones", Total: 149.00, Status: "in transit", Detail: "Shipped Oct 18 with DHL; no tracking update since Oct 21", Customer: "Jonas Weber", Email: "jonas.weber@example.com", Card: "1881"},
		"1103": {ID: "1103", Item: "Linen throw blanket", Total: 68.50, Status: "processing", Detail: "Packed, ships tomorrow", Customer: "Priya Nair", Email: "priya.nair@example.com", Card: "0005"},
		"1120": {ID: "1120", Item: "Burr espresso grinder", Total: 219.00, Status: "delivered", Detail: "Delivered on Oct 27", Customer: "Leo Martins", Email: "leo.martins@example.com", Card: "7310"},
	}}
}

func (s *Shop) Order(id string) (Order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[strings.TrimPrefix(id, "#")]
	if !ok {
		return Order{}, false
	}
	return *o, true
}

func (s *Shop) Orders() []Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Order
	for _, id := range []string{"1042", "1077", "1103", "1120"} {
		out = append(out, *s.orders[id])
	}
	return out
}

func (s *Shop) Refund(id string, amount float64) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[strings.TrimPrefix(id, "#")]
	switch {
	case !ok:
		return Order{}, fmt.Errorf("no order %s", id)
	case amount <= 0:
		return Order{}, fmt.Errorf("refund amount must be positive")
	case o.Refunded+amount > o.Total+0.001:
		return Order{}, fmt.Errorf("order %s was $%.2f and $%.2f is already refunded", id, o.Total, o.Refunded)
	}
	s.refunds++
	o.Refunded += amount
	o.RefundRef = fmt.Sprintf("RF-%d%03d", time.Now().Year()%100, 400+s.refunds)
	if o.Refunded >= o.Total-0.001 {
		o.Status = "refunded"
	} else {
		o.Status = "partially refunded"
	}
	return *o, nil
}

func (s *Shop) Send(e Email) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.At = time.Now()
	s.outbox = append(s.outbox, e)
}

func (s *Shop) Outbox() []Email {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Email{}, s.outbox...)
}

func Search(query string) (Article, bool) {
	q := strings.ToLower(query)
	for _, a := range articles {
		for _, k := range a.Keywords {
			if strings.Contains(q, k) {
				return a, true
			}
		}
	}
	return Article{}, false
}
