package shopify

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/loadgen/internal/dimensions"
)

// ─── Shopify-shaped types ─────────────────────────────────────────────────────
// JSON field names match exactly what InfraSage's webhook receiver parses
// (internal/integrations/shopify.go). Money fields are strings, as real
// Shopify sends them.

type Variant struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Price string `json:"price"`
	SKU   string `json:"sku"`
}

type Product struct {
	ID          int64     `json:"id"`
	Title       string    `json:"title"`
	Handle      string    `json:"handle"`
	ProductType string    `json:"product_type"`
	Vendor      string    `json:"vendor"`
	Variants    []Variant `json:"variants"`
	CreatedAt   string    `json:"created_at"`
}

type Address struct {
	City        string `json:"city"`
	Province    string `json:"province"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
}

type Customer struct {
	ID             int64   `json:"id"`
	Email          string  `json:"email"`
	FirstName      string  `json:"first_name"`
	LastName       string  `json:"last_name"`
	OrdersCount    int     `json:"orders_count"`
	State          string  `json:"state"`
	DefaultAddress Address `json:"default_address"`
	CreatedAt      string  `json:"created_at"`
}

type DiscountCode struct {
	Code   string `json:"code"`
	Amount string `json:"amount"`
	Type   string `json:"type"`
}

type LineItem struct {
	ID          int64  `json:"id"`
	ProductID   int64  `json:"product_id"`
	VariantID   int64  `json:"variant_id"`
	Title       string `json:"title"`
	Quantity    int    `json:"quantity"`
	Price       string `json:"price"`
	ProductType string `json:"product_type"`
	Vendor      string `json:"vendor"`
}

type Order struct {
	ID                    int64          `json:"id"`
	Name                  string         `json:"name"`
	OrderNumber           int64          `json:"order_number"`
	TotalPrice            string         `json:"total_price"`
	SubtotalPrice         string         `json:"subtotal_price"`
	Currency              string         `json:"currency"`
	FinancialStatus       string         `json:"financial_status"`
	Gateway               string         `json:"gateway"`
	BuyerAcceptsMarketing bool           `json:"buyer_accepts_marketing"`
	DiscountCodes         []DiscountCode `json:"discount_codes"`
	LineItems             []LineItem     `json:"line_items"`
	BillingAddress        Address        `json:"billing_address"`
	ShippingAddress       Address        `json:"shipping_address"`
	Customer              Customer       `json:"customer"`
	CreatedAt             string         `json:"created_at"`
	UpdatedAt             string         `json:"updated_at"`
}

type RefundTxn struct {
	Amount  string `json:"amount"`
	Kind    string `json:"kind"`
	Gateway string `json:"gateway"`
}

type Refund struct {
	ID           int64       `json:"id"`
	OrderID      int64       `json:"order_id"`
	CreatedAt    string      `json:"created_at"`
	Transactions []RefundTxn `json:"transactions"`
}

type Transaction struct {
	ID        int64  `json:"id"`
	OrderID   int64  `json:"order_id"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Gateway   string `json:"gateway"`
	Amount    string `json:"amount"`
	CreatedAt string `json:"created_at"`
}

type Checkout struct {
	ID            int64      `json:"id"`
	Token         string     `json:"token"`
	SubtotalPrice string     `json:"subtotal_price"`
	TotalPrice    string     `json:"total_price"`
	Currency      string     `json:"currency"`
	Email         string     `json:"email"`
	LineItems     []LineItem `json:"line_items"`
	CreatedAt     string     `json:"created_at"`
}

type Fulfillment struct {
	ID             int64  `json:"id"`
	OrderID        int64  `json:"order_id"`
	Status         string `json:"status"`
	ShipmentStatus string `json:"shipment_status"`
	CreatedAt      string `json:"created_at"`
	OrderCreatedAt string `json:"order_created_at"`
}

// Event is one webhook to emit.
type Event struct {
	ShopDomain string
	Topic      string
	Payload    interface{}
}

// ─── Realistic generation data ────────────────────────────────────────────────

var (
	productTypes = []string{"Apparel", "Footwear", "Accessories", "Home & Garden", "Electronics", "Beauty"}

	titleAdjective = []string{"Classic", "Premium", "Eco", "Vintage", "Modern", "Signature", "Limited", "Essential", "Pro", "Artisan"}
	titleNoun       = map[string][]string{
		"Apparel":       {"Hoodie", "Tee", "Jacket", "Joggers", "Crewneck"},
		"Footwear":      {"Runners", "Loafers", "Boots", "Sandals", "Sneakers"},
		"Accessories":   {"Backpack", "Wallet", "Sunglasses", "Watch", "Belt"},
		"Home & Garden": {"Lamp", "Throw", "Planter", "Mug Set", "Cushion"},
		"Electronics":   {"Earbuds", "Charger", "Speaker", "Webcam", "Keyboard"},
		"Beauty":        {"Serum", "Cleanser", "Balm", "Mask", "Mist"},
	}
	priceRange = map[string][2]float64{
		"Apparel":       {24, 120},
		"Footwear":      {45, 220},
		"Accessories":   {15, 180},
		"Home & Garden": {12, 95},
		"Electronics":   {29, 340},
		"Beauty":        {9, 75},
	}

	firstNames = []string{"Ava", "Liam", "Noah", "Emma", "Olivia", "Sophia", "Mason", "Lucas", "Mia", "Ethan", "Isabella", "Aiden", "Riya", "Kenji", "Lena", "Mateo"}
	lastNames  = []string{"Patel", "Kim", "Garcia", "Nguyen", "Smith", "Müller", "Rossi", "Dubois", "Silva", "Khan", "Johnson", "Park", "Costa", "Haddad"}

	vendors = []string{"NorthLoop", "Brightside", "Kestrel", "Maison Vert", "Atlas&Co", "Pinecrest"}

	discountCodes = []string{"WELCOME10", "SUMMER20", "VIP15", "FREESHIP", "BUNDLE25"}
)

// ─── Store ────────────────────────────────────────────────────────────────────

// Store holds the in-memory catalog/customers/orders per shop and the event
// channel the emitter drains.
type Store struct {
	cfg    Config
	mu     sync.RWMutex
	rr     int // round-robin shop cursor
	idSeq  int64
	prods  map[string][]Product
	custs  map[string][]Customer
	orders map[string][]Order
	events chan Event
}

const maxOrdersRetained = 600

// NewStore seeds a stable catalog + customers + historical orders per shop.
func NewStore(cfg Config) *Store {
	s := &Store{
		cfg:    cfg,
		prods:  map[string][]Product{},
		custs:  map[string][]Customer{},
		orders: map[string][]Order{},
		events: make(chan Event, 1024),
	}
	now := time.Now().UTC()
	for _, shop := range cfg.Shops {
		for i := 0; i < 60; i++ {
			s.prods[shop.Domain] = append(s.prods[shop.Domain], s.newProduct(now))
		}
		for i := 0; i < 150; i++ {
			s.custs[shop.Domain] = append(s.custs[shop.Domain], s.newCustomer(now))
		}
		for i := 0; i < cfg.SeedOrders; i++ {
			o := s.newOrder(shop, now.Add(-time.Duration(rand.Intn(120))*time.Minute))
			s.orders[shop.Domain] = append(s.orders[shop.Domain], o)
		}
	}
	return s
}

func (s *Store) nextID() int64 {
	s.idSeq++
	return 7_000_000_000 + s.idSeq
}

func (s *Store) newProduct(now time.Time) Product {
	pt := productTypes[rand.Intn(len(productTypes))]
	nouns := titleNoun[pt]
	title := fmt.Sprintf("%s %s", titleAdjective[rand.Intn(len(titleAdjective))], nouns[rand.Intn(len(nouns))])
	pr := priceRange[pt]
	price := money(pr[0] + rand.Float64()*(pr[1]-pr[0]))
	id := s.nextID()
	return Product{
		ID:          id,
		Title:       title,
		Handle:      fmt.Sprintf("p-%d", id),
		ProductType: pt,
		Vendor:      vendors[rand.Intn(len(vendors))],
		Variants:    []Variant{{ID: s.nextID(), Title: "Default", Price: price, SKU: fmt.Sprintf("SKU-%d", id%100000)}},
		CreatedAt:   now.Add(-time.Duration(rand.Intn(240)) * time.Hour).Format(time.RFC3339),
	}
}

func (s *Store) newCustomer(now time.Time) Customer {
	fn := firstNames[rand.Intn(len(firstNames))]
	ln := lastNames[rand.Intn(len(lastNames))]
	dim := dimensions.Pick()
	country, code := countryFor(dim.Region)
	id := s.nextID()
	return Customer{
		ID:          id,
		Email:       fmt.Sprintf("%s.%s%d@example.com", fn, ln, id%10000),
		FirstName:   fn,
		LastName:    ln,
		OrdersCount: rand.Intn(12),
		State:       "enabled",
		DefaultAddress: Address{
			City:        "—",
			Province:    dim.Region,
			Country:     country,
			CountryCode: code,
		},
		CreatedAt: now.Add(-time.Duration(rand.Intn(8760)) * time.Hour).Format(time.RFC3339),
	}
}

// countryFor maps a loadgen region to a Shopify (country, countryCode).
func countryFor(region string) (country, code string) {
	switch region {
	case "eu-west-1":
		opts := [][2]string{{"United Kingdom", "GB"}, {"Germany", "DE"}, {"France", "FR"}}
		c := opts[rand.Intn(len(opts))]
		return c[0], c[1]
	default:
		return "United States", "US"
	}
}

func (s *Store) pickCustomer(shop string) Customer {
	cs := s.custs[shop]
	if len(cs) == 0 {
		return s.newCustomer(time.Now().UTC())
	}
	return cs[rand.Intn(len(cs))]
}

func (s *Store) pickProduct(shop string) Product {
	ps := s.prods[shop]
	if len(ps) == 0 {
		return s.newProduct(time.Now().UTC())
	}
	return ps[rand.Intn(len(ps))]
}

// newOrder builds a realistic order. Gateway is the shop's FIXED gateway so the
// payment_gateway pillar key stays stable for this shop.
func (s *Store) newOrder(shop Shop, ts time.Time) Order {
	cust := s.pickCustomer(shop.Domain)
	nItems := 1 + rand.Intn(3)
	var items []LineItem
	subtotal := 0.0
	for i := 0; i < nItems; i++ {
		p := s.pickProduct(shop.Domain)
		qty := 1 + rand.Intn(2)
		unit := parseMoney(p.Variants[0].Price)
		subtotal += unit * float64(qty)
		items = append(items, LineItem{
			ID: s.nextID(), ProductID: p.ID, VariantID: p.Variants[0].ID,
			Title: p.Title, Quantity: qty, Price: p.Variants[0].Price,
			ProductType: p.ProductType, Vendor: p.Vendor,
		})
	}
	var discounts []DiscountCode
	total := subtotal
	if rand.Float64() < 0.25 {
		d := subtotal * 0.10
		total = subtotal - d
		discounts = []DiscountCode{{Code: discountCodes[rand.Intn(len(discountCodes))], Amount: money(d), Type: "percentage"}}
	}
	id := s.nextID()
	addr := cust.DefaultAddress
	return Order{
		ID:                    id,
		Name:                  fmt.Sprintf("#%d", 1000+id%100000),
		OrderNumber:           1000 + id%100000,
		TotalPrice:            money(total),
		SubtotalPrice:         money(subtotal),
		Currency:              "USD",
		FinancialStatus:       "paid",
		Gateway:               shop.Gateway,
		BuyerAcceptsMarketing: rand.Float64() < 0.30,
		DiscountCodes:         discounts,
		LineItems:             items,
		BillingAddress:        addr,
		ShippingAddress:       addr,
		Customer:              cust,
		CreatedAt:             ts.Format(time.RFC3339),
		UpdatedAt:             ts.Format(time.RFC3339),
	}
}

func (s *Store) recordOrder(shop string, o Order) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lst := append(s.orders[shop], o)
	if len(lst) > maxOrdersRetained {
		lst = lst[len(lst)-maxOrdersRetained:]
	}
	s.orders[shop] = lst
}

func (s *Store) recentOrder(shop string) (Order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	lst := s.orders[shop]
	if len(lst) == 0 {
		return Order{}, false
	}
	return lst[rand.Intn(len(lst))], true
}

// ─── Topic mix ────────────────────────────────────────────────────────────────

type weightedTopic struct {
	topic string
	w     float64
}

var topicMix = []weightedTopic{
	{"orders/create", 0.22},
	{"orders/paid", 0.18},
	{"transactions/create", 0.22},
	{"checkouts/create", 0.14},
	{"checkouts/delete", 0.08},
	{"fulfillments/create", 0.08},
	{"customers/create", 0.04},
	{"refunds/create", 0.02},
	{"orders/cancelled", 0.02},
}

func pickTopic() string {
	total := 0.0
	for _, t := range topicMix {
		total += t.w
	}
	r := rand.Float64() * total
	cum := 0.0
	for _, t := range topicMix {
		cum += t.w
		if r <= cum {
			return t.topic
		}
	}
	return topicMix[0].topic
}

// RunMutator emits one weighted webhook event per tick at WebhooksPerMinute.
func (s *Store) RunMutator(ctx context.Context) {
	if s.cfg.WebhooksPerMinute < 1 {
		s.cfg.WebhooksPerMinute = 60
	}
	interval := time.Minute / time.Duration(s.cfg.WebhooksPerMinute)
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			shop := s.cfg.Shops[s.rr%len(s.cfg.Shops)]
			s.rr++
			s.mu.Unlock()
			s.emit(shop, pickTopic())
		}
	}
}

// emit builds the payload for one topic and pushes it (non-blocking).
func (s *Store) emit(shop Shop, topic string) {
	now := time.Now().UTC()
	var payload interface{}

	switch topic {
	case "orders/create":
		o := s.newOrder(shop, now)
		s.recordOrder(shop.Domain, o)
		payload = &o
	case "orders/paid":
		if o, ok := s.recentOrder(shop.Domain); ok {
			payload = &o
		} else {
			o := s.newOrder(shop, now)
			payload = &o
		}
	case "orders/cancelled":
		o, ok := s.recentOrder(shop.Domain)
		if !ok {
			o = s.newOrder(shop, now)
		}
		payload = &o
	case "transactions/create":
		o, ok := s.recentOrder(shop.Domain)
		amount := "49.00"
		var oid int64
		if ok {
			amount = o.TotalPrice
			oid = o.ID
		}
		status := "success"
		if rand.Float64() < s.cfg.BaselineDecline {
			status = "failure"
		}
		payload = &Transaction{
			ID: s.nextID(), OrderID: oid, Kind: "sale", Status: status,
			Gateway: shop.Gateway, Amount: amount, CreatedAt: now.Format(time.RFC3339),
		}
	case "checkouts/create":
		payload = s.newCheckout(shop, now)
	case "checkouts/delete":
		payload = s.newCheckout(shop, now)
	case "fulfillments/create":
		o, ok := s.recentOrder(shop.Domain)
		oc := now.Add(-time.Duration(20+rand.Intn(180)) * time.Minute)
		if ok {
			if t, err := time.Parse(time.RFC3339, o.CreatedAt); err == nil {
				oc = t
			}
		}
		payload = &Fulfillment{
			ID: s.nextID(), Status: "success", ShipmentStatus: "in_transit",
			CreatedAt: now.Format(time.RFC3339), OrderCreatedAt: oc.Format(time.RFC3339),
		}
	case "customers/create":
		c := s.newCustomer(now)
		s.mu.Lock()
		s.custs[shop.Domain] = append(s.custs[shop.Domain], c)
		s.mu.Unlock()
		payload = &c
	case "refunds/create":
		o, ok := s.recentOrder(shop.Domain)
		amount := "29.00"
		var oid int64
		if ok {
			amount = o.TotalPrice
			oid = o.ID
		}
		payload = &Refund{
			ID: s.nextID(), OrderID: oid, CreatedAt: now.Format(time.RFC3339),
			Transactions: []RefundTxn{{Amount: amount, Kind: "refund", Gateway: shop.Gateway}},
		}
	default:
		return
	}

	select {
	case s.events <- Event{ShopDomain: shop.Domain, Topic: topic, Payload: payload}:
	default: // channel full — drop rather than stall the mutator
	}
}

func (s *Store) newCheckout(shop Shop, now time.Time) *Checkout {
	p := s.pickProduct(shop.Domain)
	qty := 1 + rand.Intn(3)
	sub := parseMoney(p.Variants[0].Price) * float64(qty)
	return &Checkout{
		ID: s.nextID(), Token: fmt.Sprintf("chk_%d", s.nextID()),
		SubtotalPrice: money(sub), TotalPrice: money(sub), Currency: "USD",
		Email:     s.pickCustomer(shop.Domain).Email,
		LineItems: []LineItem{{ID: s.nextID(), ProductID: p.ID, Title: p.Title, Quantity: qty, Price: p.Variants[0].Price, ProductType: p.ProductType}},
		CreatedAt: now.Format(time.RFC3339),
	}
}

// ─── REST accessors (copies under RLock) ──────────────────────────────────────

func (s *Store) ListProducts(shop string, limit int) []Product {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clip(s.prods[shop], limit)
}

func (s *Store) ListCustomers(shop string, limit int) []Customer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clip(s.custs[shop], limit)
}

func (s *Store) ListOrders(shop string, limit int) []Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	lst := s.orders[shop]
	out := make([]Order, len(lst))
	copy(out, lst)
	// newest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return clip(out, limit)
}

func (s *Store) ShopDomains() []Shop {
	return s.cfg.Shops
}

func clip[T any](in []T, limit int) []T {
	if limit <= 0 || limit > 250 {
		limit = 50
	}
	if len(in) > limit {
		in = in[:limit]
	}
	out := make([]T, len(in))
	copy(out, in)
	return out
}

// ─── money helpers ────────────────────────────────────────────────────────────

func money(f float64) string {
	if f < 0 {
		f = 0
	}
	return fmt.Sprintf("%.2f", f)
}

func parseMoney(s string) float64 {
	var f float64
	_, _ = fmt.Sscanf(s, "%f", &f)
	return f
}
