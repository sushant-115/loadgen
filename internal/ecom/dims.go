// Package ecom emits bounded-cardinality synthetic ecommerce KPI metrics
// (funnel, pricing, fulfilment, returns, search, …) over the existing OTEL
// pipeline, plus a unified anomaly injector that perturbs those metrics and
// emits a matching causal log line so InfraSage can both detect the anomaly
// (per-(service_id, metric_name) modified-z-score) and root-cause it
// (log-heuristic / peer correlation).
//
// Cardinality is deliberately bounded: the dimension is encoded in the metric
// NAME (InfraSage's named-metric scorer keys on (service_id, name) and ignores
// labels, and the gateway drops high-cardinality labels), so the dimension set
// is a small fixed list, never per-arbitrary-product.
package ecom

import (
	"regexp"
	"strings"
)

// Scenario is one injectable ecommerce anomaly.
type Scenario string

const (
	ConversionDrop      Scenario = "conversion_drop"
	PricingAnomaly      Scenario = "pricing_anomaly"
	VariantMismatch     Scenario = "variant_mismatch"
	CouponAbuse         Scenario = "coupon_abuse"
	TrafficLowConv      Scenario = "traffic_low_conversion"
	FulfillmentDelay    Scenario = "fulfillment_delay"
	WarehouseSLABreach  Scenario = "warehouse_sla_breach"
	ReturnsSpike        Scenario = "returns_spike"
	SearchFailures      Scenario = "search_failures"
)

// AllScenarios is the canonical ordered list (used by /ecom/scenarios).
var AllScenarios = []Scenario{
	ConversionDrop, PricingAnomaly, VariantMismatch, CouponAbuse,
	TrafficLowConv, FulfillmentDelay, WarehouseSLABreach, ReturnsSpike,
	SearchFailures,
}

func IsScenario(s string) bool {
	for _, sc := range AllScenarios {
		if string(sc) == s {
			return true
		}
	}
	return false
}

// Bounded dimension sets. Values are the human labels; metric names use the
// sanitized form.
var (
	ProductTypes = []string{"Apparel", "Footwear", "Accessories", "Home & Garden", "Electronics", "Beauty"}
	DemoSKUs     = []string{"nike-air-zoom", "adidas-ultraboost", "levis-501", "apple-airpods", "dyson-v15", "kindle-paperwhite", "yeti-rambler", "lego-technic"}
	Warehouses   = []string{"wh-east", "wh-west", "wh-central"}
	SearchTerms  = []string{"nike shoes", "summer dress", "wireless earbuds", "yoga mat"}
)

// DefaultDimension is the dimension a scenario targets when the caller does not
// specify one.
func DefaultDimension(s Scenario) string {
	switch s {
	case ConversionDrop, VariantMismatch, ReturnsSpike:
		return "Footwear"
	case PricingAnomaly:
		return "nike-air-zoom"
	case FulfillmentDelay, WarehouseSLABreach:
		return "wh-east"
	case SearchFailures:
		return "nike shoes"
	default: // CouponAbuse, TrafficLowConv are global
		return ""
	}
}

// dimScoped reports whether a scenario targets a specific dimension value
// (true) or is global (false).
func dimScoped(s Scenario) bool {
	switch s {
	case CouponAbuse, TrafficLowConv:
		return false
	}
	return true
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// sanitize lowercases and collapses any non-alphanumeric run to a single '_'.
func sanitize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonAlnum.ReplaceAllString(s, "_")
	return strings.Trim(s, "_")
}

// metricName builds "<family>__<dim>" (dim encoded in the name, not a label).
// A blank dim yields just the family (global metric).
func metricName(family, dim string) string {
	if dim == "" {
		return family
	}
	return family + "__" + sanitize(dim)
}
