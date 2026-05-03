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

	"github.com/loadgen/internal/chaos"
	"github.com/loadgen/internal/distribution"
	"github.com/loadgen/internal/sysstate"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
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

	time.Sleep(distribution.ScaledDuration(3, 0.6, sysstate.FaultHealth(sysstate.FaultDBContention)))
	applyConnectionPoolDelay(span)
	applyDBSlowChaosDelay()

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

	time.Sleep(distribution.ScaledDuration(2, 0.5, sysstate.FaultHealth(sysstate.FaultDBContention)))
	applyConnectionPoolDelay(span)
	applyDBSlowChaosDelay()

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

	time.Sleep(distribution.ScaledDuration(5, 0.7, sysstate.FaultHealth(sysstate.FaultDBContention)))
	applyConnectionPoolDelay(span)
	applyDBSlowChaosDelay()

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

	time.Sleep(distribution.ScaledDuration(3, 0.6, sysstate.FaultHealth(sysstate.FaultDBContention)))
	applyConnectionPoolDelay(span)
	applyDBSlowChaosDelay()

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

	time.Sleep(distribution.ScaledDuration(1, 0.3, sysstate.FaultHealth(sysstate.FaultMemoryPressure)))

	// cache_stampede: hot-key eviction — most cache reads miss, forcing all
	// callers to go to the DB. The counter-intuitive signal: cache latency DROPS
	// (empty cache = fast lookup) while DB latency/errors explode.
	if sysstate.IsActive(sysstate.FaultCacheStampede) {
		evictProb := sysstate.ScaledErrorRate(0, 0.90, sysstate.FaultHealth(sysstate.FaultCacheStampede))
		if rand.Float64() < evictProb {
			span.SetAttributes(
				attribute.Bool("cache.hit", false),
				attribute.Bool("cache.evicted", true),
			)
			return nil, false
		}
	}

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

	time.Sleep(distribution.ScaledDuration(1, 0.3, sysstate.FaultHealth(sysstate.FaultMemoryPressure)))

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
// TraceCarrier propagates the W3C traceparent of the publishing span so that
// consumers can create child spans linked to the producer's trace tree.
type Message struct {
	Topic        string            `json:"topic"`
	Payload      json.RawMessage   `json:"payload"`
	TraceCarrier map[string]string `json:"-"`
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
	// Inject trace context into the message so consumers can continue the trace.
	carrier := make(map[string]string)
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(carrier))
	msg := Message{Topic: topic, Payload: raw, TraceCarrier: carrier}

	applyQueueBacklogDelay()

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
				applyQueueBacklogDelay()
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

func applyDBSlowChaosDelay() {
	if !chaos.IsActive(chaos.DBSlow) {
		return
	}
	// Additional DB delay up to ~700ms at max intensity.
	intensity := chaos.GetIntensity(chaos.DBSlow)
	extra := time.Duration(float64(700*time.Millisecond) * intensity)
	if extra < 10*time.Millisecond {
		extra = 10 * time.Millisecond
	}
	time.Sleep(extra)
}

func applyQueueBacklogDelay() {
	// Fault-based queue backpressure: notification consumer lag causes publish to block.
	if sysstate.IsActive(sysstate.FaultQueueBackpressure) {
		queueHealth := sysstate.FaultHealth(sysstate.FaultQueueBackpressure)
		extra := distribution.ScaledDuration(50, 1.5, queueHealth)
		slog.Debug("queue backpressure delay",
			"extra_ms", extra.Milliseconds(),
			"queue_health", queueHealth,
		)
		time.Sleep(extra)
	}
	// Chaos-based queue backlog (manual chaos injection, unchanged).
	if !chaos.IsActive(chaos.QueueBacklog) {
		return
	}
	intensity := chaos.GetIntensity(chaos.QueueBacklog)
	extra := time.Duration(float64(900*time.Millisecond) * intensity)
	if extra < 20*time.Millisecond {
		extra = 20 * time.Millisecond
	}
	time.Sleep(extra)
}

// applyConnectionPoolDelay simulates connection pool exhaustion: a fraction of
// DB calls get stuck waiting for a free slot, producing bimodal p50/p99 latency.
// The diagnostic puzzle: p50 looks fine, only p99/p999 are extreme. Most
// monitoring alerts fire on avg latency first, so this goes undetected longer.
func applyConnectionPoolDelay(span trace.Span) {
	if !sysstate.IsActive(sysstate.FaultConnectionPool) {
		return
	}
	poolHealth := sysstate.FaultHealth(sysstate.FaultConnectionPool)
	// At peak health=0.18 roughly 45% of callers wait for a pool slot.
	waitProb := sysstate.ScaledErrorRate(0, 0.45, poolHealth)
	if rand.Float64() >= waitProb {
		return
	}
	// Wait time: 8-20 s (simulates a saturated pool queue).
	wait := 8*time.Second + time.Duration(rand.Float64()*12*1000)*time.Millisecond
	if span != nil {
		span.SetAttributes(
			attribute.Bool("db.connection_pool_wait", true),
			attribute.Int64("db.pool_wait_ms", wait.Milliseconds()),
		)
	}
	slog.Warn("connection pool exhausted: waiting for slot",
		"wait_ms", wait.Milliseconds(),
		"pool_health", poolHealth,
	)
	time.Sleep(wait)
}

// ---------------------------------------------------------------------------
// Shared singleton instances so all services in the same process can
// communicate.
// ---------------------------------------------------------------------------

var (
	SharedDB    = NewDB()
	SharedQueue = NewQueue()
)
