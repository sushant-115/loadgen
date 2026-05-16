// Mock Shopify – a fake Shopify store that serves a browsable Admin REST API
// and continuously fires realistic Shopify webhooks at InfraSage, with an
// injectable payment-gateway-meltdown incident knob for demos.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/loadgen/internal/chaos"
	"github.com/loadgen/internal/ecom"
	"github.com/loadgen/internal/middleware"
	"github.com/loadgen/internal/shopify"
	"github.com/loadgen/internal/sysstate"
	"github.com/loadgen/internal/telemetry"
)

const serviceName = "mock-shopify"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	// Telemetry is best-effort: the mock's value is the webhooks it pushes to
	// InfraSage, not its own OTEL. Don't exit if no collector is reachable.
	shutdown, err := telemetry.Init(serviceName)
	if err != nil {
		slog.Warn("telemetry init failed (continuing without it)", "error", err)
		shutdown = func() {}
	}
	defer shutdown()

	cfg := shopify.LoadConfig()
	store := shopify.NewStore(cfg)
	emitter := shopify.NewEmitter(cfg, store)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go store.RunMutator(ctx)
	go emitter.Run(ctx)

	// Synthetic bounded-cardinality ecommerce KPI metrics + unified anomaly
	// injector (conversion/pricing/fulfilment/returns/search/…).
	ecomInjector := ecom.NewInjector()
	if err := ecom.NewEmitter(ecomInjector).Start(ctx); err != nil {
		slog.Warn("ecom metrics start failed (continuing)", "error", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/api/2024-01/products.json", shopify.HandleProducts(store))
	mux.HandleFunc("GET /admin/api/2024-01/orders.json", shopify.HandleOrders(store))
	mux.HandleFunc("GET /admin/api/2024-01/customers.json", shopify.HandleCustomers(store))
	mux.HandleFunc("GET /admin/api/2024-01/shop.json", shopify.HandleShop(store))
	shopify.RegisterIncidentEndpoints(mux, emitter)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.Handle("GET /metrics", telemetry.PrometheusHandler())
	chaos.RegisterChaosEndpoints(mux)
	sysstate.RegisterEndpoints(mux)
	ecom.RegisterEndpoints(mux, ecomInjector)

	handler := middleware.Chain(serviceName, slog.Default(), mux)
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: handler}

	go func() {
		slog.Info("mock-shopify starting", "addr", srv.Addr,
			"shops", len(cfg.Shops), "webhooks_per_minute", cfg.WebhooksPerMinute,
			"infrasage", cfg.InfraSageBaseURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down mock-shopify")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}
