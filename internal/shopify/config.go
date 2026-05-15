// Package shopify implements a mock Shopify store: an in-memory catalog of
// products/customers/orders, a Shopify-shaped Admin REST API, a webhook
// emitter that POSTs realistic Shopify webhooks at InfraSage, and an
// injectable payment-gateway-meltdown incident knob for demos.
package shopify

import (
	"os"
	"strconv"
	"strings"
)

// Shop is one mock storefront. Gateway is FIXED per shop so InfraSage's
// service_pillar_map (one row per service_id, last-write-wins) keeps a stable
// payment_gateway key and the payment_gateway pillar fans out cleanly across
// shops instead of flapping.
type Shop struct {
	Domain  string
	Gateway string
}

// Config is the mock's runtime configuration, loaded from the environment.
type Config struct {
	InfraSageBaseURL  string
	Shops             []Shop
	WebhooksPerMinute int
	BaselineDecline   float64
	SeedOrders        int
	Port              string
}

// LoadConfig reads configuration from the environment with demo-safe defaults.
func LoadConfig() Config {
	return Config{
		InfraSageBaseURL:  getenv("INFRASAGE_BASE_URL", "https://api.infrasage.dev"),
		Shops:             parseShops(getenv("SHOPIFY_SHOPS", "acme.myshopify.com:stripe,globex.myshopify.com:paypal,initech.myshopify.com:adyen")),
		WebhooksPerMinute: getenvInt("SHOPIFY_WEBHOOKS_PER_MINUTE", 90),
		BaselineDecline:   getenvFloat("SHOPIFY_BASELINE_DECLINE_RATE", 0.04),
		SeedOrders:        getenvInt("SHOPIFY_SEED_ORDERS", 200),
		Port:              getenv("SERVICE_PORT", "8086"),
	}
}

// parseShops parses a "domain:gateway,domain:gateway" CSV. Falls back to a
// single stripe shop if nothing parses.
func parseShops(csv string) []Shop {
	var shops []Shop
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		domain, gateway, ok := strings.Cut(part, ":")
		domain = strings.TrimSpace(domain)
		gateway = strings.TrimSpace(gateway)
		if !ok || domain == "" {
			continue
		}
		if gateway == "" {
			gateway = "stripe"
		}
		shops = append(shops, Shop{Domain: domain, Gateway: gateway})
	}
	if len(shops) == 0 {
		shops = []Shop{{Domain: "acme.myshopify.com", Gateway: "stripe"}}
	}
	return shops
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func getenvFloat(key string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return def
}
