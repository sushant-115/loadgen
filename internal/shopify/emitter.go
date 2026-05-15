package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"sync/atomic"
	"time"
)

// Emitter drains the store's event channel, applies the active incident
// transform, and POSTs Shopify-shaped webhooks to InfraSage.
type Emitter struct {
	cfg      Config
	store    *Store
	client   *http.Client
	incident *IncidentState

	emitted  atomic.Int64
	declines atomic.Int64
	errors   atomic.Int64
}

// NewEmitter creates an Emitter with a 5s HTTP timeout.
func NewEmitter(cfg Config, store *Store) *Emitter {
	return &Emitter{
		cfg:      cfg,
		store:    store,
		client:   &http.Client{Timeout: 5 * time.Second},
		incident: &IncidentState{},
	}
}

// Run consumes events until ctx is cancelled.
func (e *Emitter) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-e.store.events:
			for _, out := range e.applyIncident(ev) {
				e.post(ctx, out)
			}
		}
	}
}

// applyIncident mutates / fans out an event when an incident targets its shop.
func (e *Emitter) applyIncident(ev Event) []Event {
	active, shop, decline, cancel, refund := e.incident.Active()
	if !active || ev.ShopDomain != shop {
		return []Event{ev}
	}

	out := []Event{ev}
	switch ev.Topic {
	case "transactions/create":
		if t, ok := ev.Payload.(*Transaction); ok && rand.Float64() < decline {
			t.Status = "failure"
			e.declines.Add(1)
		}
	case "orders/create", "orders/paid":
		o, ok := ev.Payload.(*Order)
		if !ok {
			break
		}
		if rand.Float64() < cancel {
			cp := *o
			out = append(out, Event{ShopDomain: ev.ShopDomain, Topic: "orders/cancelled", Payload: &cp})
		}
		if rand.Float64() < refund {
			out = append(out, Event{ShopDomain: ev.ShopDomain, Topic: "refunds/create", Payload: &Refund{
				ID: e.store.nextID(), OrderID: o.ID, CreatedAt: time.Now().UTC().Format(time.RFC3339),
				Transactions: []RefundTxn{{Amount: o.TotalPrice, Kind: "refund", Gateway: o.Gateway}},
			}})
		}
	}
	return out
}

// post sends one webhook. Topic keeps its literal '/' so InfraSage's
// {topic...} wildcard yields e.g. "orders/create" (do NOT %2F-encode).
func (e *Emitter) post(ctx context.Context, ev Event) {
	body, err := json.Marshal(ev.Payload)
	if err != nil {
		e.errors.Add(1)
		return
	}
	url := e.cfg.InfraSageBaseURL + "/api/v1/webhooks/shopify/" + ev.ShopDomain + "/" + ev.Topic
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		e.errors.Add(1)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Shop-Domain", ev.ShopDomain)
	req.Header.Set("X-Shopify-Topic", ev.Topic)
	req.Header.Set("X-Shopify-API-Version", "2024-01")

	resp, err := e.client.Do(req)
	e.emitted.Add(1)
	if err != nil {
		e.errors.Add(1)
		slog.Warn("shopify webhook post failed", "shop", ev.ShopDomain, "topic", ev.Topic, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e.errors.Add(1)
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		slog.Warn("shopify webhook non-2xx", "shop", ev.ShopDomain, "topic", ev.Topic,
			"status", resp.StatusCode, "body", string(snippet))
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
}
