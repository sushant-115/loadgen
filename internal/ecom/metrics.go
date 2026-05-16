package ecom

import (
	"context"
	"log/slog"
	"math"
	"math/rand"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/loadgen/internal/telemetry"
)

// series is one bounded metric (dimension already encoded in name).
type series struct {
	name   string
	family string
	dim    string // sanitized; "" for global
	base   float64
	obs    metric.Float64Observable
}

// Emitter publishes the synthetic ecommerce KPI metrics and applies the
// injector's active perturbations on every collection.
type Emitter struct {
	inj    *Injector
	series []*series
	logged map[Scenario]time.Time
}

// product-type popularity weights (Footwear/Apparel are the big movers).
var ptWeight = map[string]float64{
	"apparel": 1.0, "footwear": 1.2, "accessories": 0.6,
	"home_garden": 0.5, "electronics": 0.9, "beauty": 0.7,
}

var skuPrice = map[string]float64{
	"nike_air_zoom": 129.99, "adidas_ultraboost": 179.99, "levis_501": 69.99,
	"apple_airpods": 199.99, "dyson_v15": 749.99, "kindle_paperwhite": 149.99,
	"yeti_rambler": 39.99, "lego_technic": 89.99,
}

func NewEmitter(inj *Injector) *Emitter {
	e := &Emitter{inj: inj, logged: map[Scenario]time.Time{}}

	add := func(family, dimHuman string, base float64) {
		e.series = append(e.series, &series{
			name:   metricName(family, dimHuman),
			family: family,
			dim:    sanitize(dimHuman),
			base:   base,
		})
	}

	for _, pt := range ProductTypes {
		w := ptWeight[sanitize(pt)]
		views := 900 * w
		add("ecom_product_views", pt, views)
		add("ecom_add_to_cart", pt, views*0.18)
		add("ecom_checkout_complete", pt, views*0.18*0.45)
		add("ecom_conversion_rate", pt, 8.1)
		add("ecom_variant_mismatch_rate", pt, 0.4)
		add("ecom_returns", pt, views*0.18*0.45*0.03)
		add("ecom_return_rate", pt, 3.0)
	}
	for _, sku := range DemoSKUs {
		add("ecom_avg_item_price", sku, skuPrice[sanitize(sku)])
	}
	for _, wh := range Warehouses {
		add("ecom_fulfillment_latency_seconds", wh, 5400)
		add("ecom_sla_breach_rate", wh, 1.0)
	}
	for _, q := range SearchTerms {
		add("ecom_search_total", q, 220)
		add("ecom_search_zero_result_rate", q, 1.5)
	}
	add("ecom_coupon_redemptions", "", 42)
	add("ecom_signups_max_per_ip", "", 2)
	add("ecom_sessions", "", 1200)
	add("ecom_orders", "", 96)
	add("ecom_conversion_rate_overall", "", 8.0)

	return e
}

// Start registers the OTEL observable callback and the causal-log loop.
func (e *Emitter) Start(ctx context.Context) error {
	m := telemetry.Meter("mock-shopify-ecom")

	observables := make([]metric.Observable, 0, len(e.series))
	for _, s := range e.series {
		g, err := m.Float64ObservableGauge(s.name,
			metric.WithDescription("synthetic ecommerce KPI: "+s.name))
		if err != nil {
			return err
		}
		s.obs = g
		observables = append(observables, g)
	}

	_, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for _, s := range e.series {
			o.ObserveFloat64(s.obs, e.value(s))
		}
		return nil
	}, observables...)
	if err != nil {
		return err
	}

	go e.causalLogLoop(ctx)
	return nil
}

// value = baseline (mild diurnal + noise) with any active injection applied.
func (e *Emitter) value(s *series) float64 {
	t := time.Now()
	dayFrac := float64(t.Hour()*3600+t.Minute()*60+t.Second()) / 86400.0
	diurnal := 0.12 * math.Sin(2*math.Pi*dayFrac)
	noise := (rand.Float64()*2 - 1) * 0.04
	v := s.base * (1 + diurnal + noise)
	return e.apply(s.family, s.dim, v)
}

// apply perturbs one metric if a scenario targeting it is active. For
// dimension-scoped scenarios the metric's own dim must match the target.
func (e *Emitter) apply(fam, dim string, v float64) float64 {
	hit := func(s Scenario) (bool, float64) {
		on, I, sd := e.inj.active(s)
		if !on {
			return false, 0
		}
		if dimScoped(s) && sanitize(sd) != dim {
			return false, 0
		}
		return true, I
	}

	if on, I := hit(ConversionDrop); on {
		switch fam {
		case "ecom_add_to_cart":
			v *= 1 - 0.70*I
		case "ecom_checkout_complete":
			v *= 1 - 0.82*I
		case "ecom_conversion_rate":
			v *= 1 - 0.78*I
		}
	}
	if on, I := hit(PricingAnomaly); on && fam == "ecom_avg_item_price" {
		v *= 1 - 0.55*I // sharp markdown (mispriced)
	}
	if on, I := hit(VariantMismatch); on && fam == "ecom_variant_mismatch_rate" {
		v += 45 * I // 0.4% -> up to ~45%
	}
	if on, I := hit(CouponAbuse); on {
		switch fam {
		case "ecom_coupon_redemptions":
			v *= 1 + 6*I
		case "ecom_signups_max_per_ip":
			v += 60 * I
		}
	}
	if on, I := hit(TrafficLowConv); on {
		switch fam {
		case "ecom_sessions":
			v *= 1 + 3.5*I
		case "ecom_conversion_rate_overall":
			v *= 1 - 0.72*I
		}
	}
	if on, I := hit(FulfillmentDelay); on && fam == "ecom_fulfillment_latency_seconds" {
		v *= 1 + 7*I
	}
	if on, I := hit(WarehouseSLABreach); on {
		switch fam {
		case "ecom_sla_breach_rate":
			v += 55 * I
		case "ecom_fulfillment_latency_seconds":
			v *= 1 + 4*I
		}
	}
	if on, I := hit(ReturnsSpike); on {
		switch fam {
		case "ecom_returns":
			v *= 1 + 5*I
		case "ecom_return_rate":
			v *= 1 + 5*I
		}
	}
	if on, I := hit(SearchFailures); on && fam == "ecom_search_zero_result_rate" {
		v += 80 * I
	}
	return v
}

// causalLogLoop emits a recognizable root-cause log line for each active
// scenario every ~15s so InfraSage's log-heuristic RCA has a sustained signal.
func (e *Emitter) causalLogLoop(ctx context.Context) {
	tk := time.NewTicker(15 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			for _, s := range AllScenarios {
				on, I, dim := e.inj.active(s)
				if !on {
					continue
				}
				slog.Warn("ecom anomaly active", "scenario", string(s),
					"dimension", dim, "intensity", I, "cause", causalMessage(s, dim))
			}
		}
	}
}

func causalMessage(s Scenario, dim string) string {
	switch s {
	case ConversionDrop:
		return "add_to_cart failing: cart-service returned HTTP 503 for product_type=" + dim + " — checkout funnel degraded"
	case PricingAnomaly:
		return "pricing rule mismatch sku=" + dim + ": catalog price != checkout price (pricing-service rule eval error)"
	case VariantMismatch:
		return "variant mapping error: color/size variant mismatch for product_type=" + dim + " (variant-service mapping table stale)"
	case CouponAbuse:
		return "coupon abuse: ip=203.0.113.45 created many accounts redeeming code WELCOME50 (fraud filter bypass)"
	case TrafficLowConv:
		return "traffic surge with flat conversion: sessions up sharply, orders flat (bot traffic / campaign mistargeting)"
	case FulfillmentDelay:
		return "fulfillment delayed: warehouse=" + dim + " queue backlog growing (WMS worker pool saturated)"
	case WarehouseSLABreach:
		return "warehouse SLA breach: warehouse=" + dim + " p95 fulfilment exceeds 24h SLA"
	case ReturnsSpike:
		return "returns surge: product_type=" + dim + " return_rate elevated (defective batch / sizing issue)"
	case SearchFailures:
		return "search degraded: query='" + dim + "' zero-result rate high (search index shard unavailable)"
	}
	return string(s)
}
