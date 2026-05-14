// Package dimensions provides shared business-dimension pickers (tenant, region,
// customer tier, plan, payment gateway) used by the traffic generator to tag
// each synthetic request, and by services to interpret those tags.
//
// Weights are deliberately skewed so one tenant (acme) dominates traffic — this
// creates visible fan-out in pillar anomaly scores when only a subset of
// tenants is affected by a chaos injection.
package dimensions

import (
	"math/rand"
	"net/http"
)

// Header names propagated across the request chain. Services read these from
// the incoming request and attach them to spans as business-dimension
// attributes.
const (
	HeaderTenantID       = "X-Tenant-ID"
	HeaderRegion         = "X-Region"
	HeaderCustomerTier   = "X-Customer-Tier"
	HeaderPlan           = "X-Plan"
	HeaderPaymentGateway = "X-Payment-Gateway"
)

// Span attribute keys. Kept distinct from the loadgen-internal X-* header
// names so InfraSage and other OTEL consumers see semantic OpenTelemetry-style
// attribute names.
const (
	AttrTenantID       = "tenant.id"
	AttrRegion         = "region"
	AttrCustomerTier   = "customer.tier"
	AttrPlan           = "plan"
	AttrPaymentGateway = "payment.gateway"
)

// Context bundles the business-dimension values for a single synthetic request.
type Context struct {
	TenantID       string
	Region         string
	CustomerTier   string
	Plan           string
	PaymentGateway string
}

// weighted is a tiny rejection-sampler-free weighted picker.
type weighted struct {
	values  []string
	weights []float64
	total   float64
}

func newWeighted(items []struct {
	v string
	w float64
}) *weighted {
	wp := &weighted{}
	for _, it := range items {
		wp.values = append(wp.values, it.v)
		wp.weights = append(wp.weights, it.w)
		wp.total += it.w
	}
	return wp
}

func (w *weighted) pick() string {
	r := rand.Float64() * w.total
	var cum float64
	for i, weight := range w.weights {
		cum += weight
		if r <= cum {
			return w.values[i]
		}
	}
	return w.values[len(w.values)-1]
}

var (
	tenants = newWeighted([]struct {
		v string
		w float64
	}{
		{"acme", 0.45},
		{"globex", 0.20},
		{"initech", 0.15},
		{"dunder-mifflin", 0.12},
		{"umbrella-corp", 0.08},
	})

	regions = newWeighted([]struct {
		v string
		w float64
	}{
		{"us-east-1", 0.55},
		{"us-west-2", 0.25},
		{"eu-west-1", 0.20},
	})

	tiers = newWeighted([]struct {
		v string
		w float64
	}{
		{"free", 0.50},
		{"pro", 0.35},
		{"enterprise", 0.15},
	})

	plans = newWeighted([]struct {
		v string
		w float64
	}{
		{"monthly", 0.55},
		{"annual", 0.30},
		{"trial", 0.15},
	})

	gateways = newWeighted([]struct {
		v string
		w float64
	}{
		{"stripe", 0.60},
		{"paypal", 0.25},
		{"adyen", 0.15},
	})
)

// Pick returns a fresh Context for one synthetic request.
func Pick() Context {
	return Context{
		TenantID:       tenants.pick(),
		Region:         regions.pick(),
		CustomerTier:   tiers.pick(),
		Plan:           plans.pick(),
		PaymentGateway: gateways.pick(),
	}
}

// ApplyHeaders writes the Context onto the outgoing HTTP request as headers,
// so downstream services can read them and tag their own spans/logs.
func (c Context) ApplyHeaders(req *http.Request) {
	if c.TenantID != "" {
		req.Header.Set(HeaderTenantID, c.TenantID)
	}
	if c.Region != "" {
		req.Header.Set(HeaderRegion, c.Region)
	}
	if c.CustomerTier != "" {
		req.Header.Set(HeaderCustomerTier, c.CustomerTier)
	}
	if c.Plan != "" {
		req.Header.Set(HeaderPlan, c.Plan)
	}
	if c.PaymentGateway != "" {
		req.Header.Set(HeaderPaymentGateway, c.PaymentGateway)
	}
}

// FromHeaders reads the Context out of an incoming HTTP request.
func FromHeaders(r *http.Request) Context {
	return Context{
		TenantID:       r.Header.Get(HeaderTenantID),
		Region:         r.Header.Get(HeaderRegion),
		CustomerTier:   r.Header.Get(HeaderCustomerTier),
		Plan:           r.Header.Get(HeaderPlan),
		PaymentGateway: r.Header.Get(HeaderPaymentGateway),
	}
}

// IsEmpty reports whether all fields are empty (i.e. the caller never set any
// business-dimension headers — e.g. a health check or internal probe).
func (c Context) IsEmpty() bool {
	return c.TenantID == "" && c.Region == "" && c.CustomerTier == "" && c.Plan == "" && c.PaymentGateway == ""
}

// GatewayFailureMultiplier returns a per-gateway failure-rate scaling factor.
// Used by payment-service to make incidents visibly heterogeneous across
// gateways — stripe degrades fastest under chaos, paypal is most resilient,
// adyen is in between. This makes the "stripe is having issues, paypal is
// fine" story land during the demo's pillar-impact section.
func GatewayFailureMultiplier(gateway string) float64 {
	switch gateway {
	case "stripe":
		return 1.0
	case "adyen":
		return 1.4
	case "paypal":
		return 0.6
	default:
		return 1.0
	}
}
