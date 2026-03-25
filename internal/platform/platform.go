// Package platform provides lightweight in-memory simulations of a database
// and a message queue so that services can be wired together without real
// infrastructure.
package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// ---------------------------------------------------------------------------
// In-memory DB simulation
// ---------------------------------------------------------------------------

// DB simulates a data store backed by an in-memory map.
type DB struct {
	mu   sync.RWMutex
	data map[string]map[string]json.RawMessage // table -> id -> record
}

// NewDB creates a new in-memory DB.
func NewDB() *DB {
	return &DB{data: make(map[string]map[string]json.RawMessage)}
}

// Insert stores a record. It simulates 1-5ms latency and creates a child span.
func (db *DB) Insert(ctx context.Context, table, id string, record any) error {
	_, span := otel.Tracer("platform.db").Start(ctx, "db.insert",
		trace.WithAttributes(
			attribute.String("db.table", table),
			attribute.String("db.record_id", id),
		),
	)
	defer span.End()

	time.Sleep(time.Duration(1+rand.Intn(5)) * time.Millisecond)

	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	if db.data[table] == nil {
		db.data[table] = make(map[string]json.RawMessage)
	}
	db.data[table][id] = raw
	return nil
}

// Get retrieves a record by table and id.
func (db *DB) Get(ctx context.Context, table, id string, dest any) error {
	_, span := otel.Tracer("platform.db").Start(ctx, "db.get",
		trace.WithAttributes(
			attribute.String("db.table", table),
			attribute.String("db.record_id", id),
		),
	)
	defer span.End()

	time.Sleep(time.Duration(1+rand.Intn(3)) * time.Millisecond)

	db.mu.RLock()
	defer db.mu.RUnlock()
	tbl, ok := db.data[table]
	if !ok {
		return fmt.Errorf("table %q not found", table)
	}
	raw, ok := tbl[id]
	if !ok {
		return fmt.Errorf("record %q not found in %q", id, table)
	}
	return json.Unmarshal(raw, dest)
}

// List returns all records in a table as raw JSON messages.
func (db *DB) List(ctx context.Context, table string) ([]json.RawMessage, error) {
	_, span := otel.Tracer("platform.db").Start(ctx, "db.list",
		trace.WithAttributes(attribute.String("db.table", table)),
	)
	defer span.End()

	time.Sleep(time.Duration(2+rand.Intn(5)) * time.Millisecond)

	db.mu.RLock()
	defer db.mu.RUnlock()
	tbl := db.data[table]
	result := make([]json.RawMessage, 0, len(tbl))
	for _, raw := range tbl {
		result = append(result, raw)
	}
	return result, nil
}

// Update replaces an existing record in the given table.
func (db *DB) Update(ctx context.Context, table, id string, record any) error {
	_, span := otel.Tracer("platform.db").Start(ctx, "db.update",
		trace.WithAttributes(
			attribute.String("db.table", table),
			attribute.String("db.record_id", id),
		),
	)
	defer span.End()

	time.Sleep(time.Duration(1+rand.Intn(5)) * time.Millisecond)

	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tbl, ok := db.data[table]
	if !ok {
		return fmt.Errorf("record %q not found in %q", id, table)
	}
	if _, ok := tbl[id]; !ok {
		return fmt.Errorf("record %q not found in %q", id, table)
	}
	tbl[id] = raw
	return nil
}

// ---------------------------------------------------------------------------
// In-memory cache simulation
// ---------------------------------------------------------------------------

// Cache simulates an in-memory cache (e.g. Redis) for read-through / cache-aside
// patterns.
type Cache struct {
	mu     sync.RWMutex
	store  map[string][]byte
}

// NewCache creates a new in-memory cache.
func NewCache() *Cache {
	return &Cache{store: make(map[string][]byte)}
}

// Get retrieves a value from the cache. Returns nil, false on a miss.
func (c *Cache) Get(ctx context.Context, key string) ([]byte, bool) {
	_, span := otel.Tracer("platform.cache").Start(ctx, "cache.get",
		trace.WithAttributes(attribute.String("cache.key", key)),
	)
	defer span.End()

	time.Sleep(time.Duration(1+rand.Intn(2)) * time.Millisecond)

	c.mu.RLock()
	defer c.mu.RUnlock()
	data, ok := c.store[key]
	span.SetAttributes(attribute.Bool("cache.hit", ok))
	return data, ok
}

// Set stores a value in the cache.
func (c *Cache) Set(ctx context.Context, key string, value []byte) {
	_, span := otel.Tracer("platform.cache").Start(ctx, "cache.set",
		trace.WithAttributes(attribute.String("cache.key", key)),
	)
	defer span.End()

	time.Sleep(time.Duration(1+rand.Intn(2)) * time.Millisecond)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.store[key] = value
}

// Delete removes a value from the cache.
func (c *Cache) Delete(ctx context.Context, key string) {
	_, span := otel.Tracer("platform.cache").Start(ctx, "cache.delete",
		trace.WithAttributes(attribute.String("cache.key", key)),
	)
	defer span.End()

	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.store, key)
}

// ---------------------------------------------------------------------------
// In-memory message queue simulation
// ---------------------------------------------------------------------------

// Message represents a queue message.
type Message struct {
	Topic   string          `json:"topic"`
	Payload json.RawMessage `json:"payload"`
}

// Handler processes a queue message.
type Handler func(ctx context.Context, msg Message) error

// Queue is an in-memory pub/sub message queue.
type Queue struct {
	mu          sync.RWMutex
	subscribers map[string][]chan Message
}

// NewQueue creates a new Queue.
func NewQueue() *Queue {
	return &Queue{subscribers: make(map[string][]chan Message)}
}

// Publish sends a message to all subscribers on the given topic.
func (q *Queue) Publish(ctx context.Context, topic string, payload any) error {
	_, span := otel.Tracer("platform.queue").Start(ctx, "queue.publish",
		trace.WithAttributes(attribute.String("queue.topic", topic)),
	)
	defer span.End()

	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	msg := Message{Topic: topic, Payload: raw}

	q.mu.RLock()
	defer q.mu.RUnlock()
	for _, ch := range q.subscribers[topic] {
		select {
		case ch <- msg:
		default:
			slog.Warn("queue subscriber channel full, dropping message", "topic", topic)
		}
	}
	return nil
}

// Subscribe registers a handler for the given topic and starts consuming in
// a goroutine. It returns a cancel function to stop consuming.
func (q *Queue) Subscribe(ctx context.Context, topic string, h Handler) func() {
	ch := make(chan Message, 256)

	q.mu.Lock()
	q.subscribers[topic] = append(q.subscribers[topic], ch)
	q.mu.Unlock()

	done := make(chan struct{})

	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				if err := h(ctx, msg); err != nil {
					slog.ErrorContext(ctx, "queue handler error",
						"topic", topic,
						"error", err,
					)
				}
			}
		}
	}()

	return func() {
		close(ch)
		<-done
	}
}

// ---------------------------------------------------------------------------
// Shared singleton instances so all services in the same process can
// communicate.
// ---------------------------------------------------------------------------

var (
	SharedDB    = NewDB()
	SharedQueue = NewQueue()
)
