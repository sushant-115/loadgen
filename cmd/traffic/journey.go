// journey.go — coherent user sessions (Travel Light for loadgen, part 1).
//
// The legacy mode fires independent weighted actions: statistically shaped,
// behaviorally incoherent (an order created by nobody, a login that leads
// nowhere). Journey mode runs SESSIONS: a virtual user logs in, browses,
// maybe orders, maybe upgrades — consecutive steps share actor state, so
// traces read like a person and downstream services see realistic call
// ratios (payments only follow orders, notifications only follow payments).
//
// Session launch rate follows the loadshape curve (diurnal + weekly +
// organic noise), scaled down during simulated incidents — real users
// leave when the site is slow; they do not helpfully 5× their traffic.

package main

import (
	"context"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/loadgen/internal/loadshape"
	"github.com/loadgen/internal/sysstate"
)

// journeyStep: one action plus how a human behaves around it.
type journeyStep struct {
	action      string
	thinkMeanMs float64 // lognormal mean think-time BEFORE this step
	abandonProb float64 // chance the session ends before this step
}

type journeyDef struct {
	name   string
	weight float64
	steps  []journeyStep
}

// The journey mix. Weights approximate a commerce-ish product: most
// sessions browse, a fraction buys, a sliver converts to paid plans.
var journeys = []journeyDef{
	{
		name: "browser", weight: 0.46,
		steps: []journeyStep{
			{action: "auth_login", thinkMeanMs: 300},
			{action: "users_get", thinkMeanMs: 1200},
			{action: "orders_list", thinkMeanMs: 2500, abandonProb: 0.25},
			{action: "orders_get", thinkMeanMs: 1800, abandonProb: 0.30},
		},
	},
	{
		name: "buyer", weight: 0.27,
		steps: []journeyStep{
			{action: "auth_login", thinkMeanMs: 300},
			{action: "users_get", thinkMeanMs: 1000},
			{action: "orders_list", thinkMeanMs: 2200},
			{action: "orders_create", thinkMeanMs: 3500, abandonProb: 0.18}, // cart hesitation
			{action: "orders_get", thinkMeanMs: 1200},
		},
	},
	{
		name: "returning-quick-check", weight: 0.17,
		steps: []journeyStep{
			{action: "auth_verify", thinkMeanMs: 100},
			{action: "orders_list", thinkMeanMs: 900},
		},
	},
	{
		name: "signup", weight: 0.07,
		steps: []journeyStep{
			{action: "users_list", thinkMeanMs: 800},
			{action: "users_create", thinkMeanMs: 4000, abandonProb: 0.35}, // form friction
			{action: "auth_login", thinkMeanMs: 600},
			{action: "users_get", thinkMeanMs: 1000},
		},
	},
	{
		name: "converter", weight: 0.03,
		steps: []journeyStep{
			{action: "auth_login", thinkMeanMs: 300},
			{action: "users_get", thinkMeanMs: 1500},
			{action: "user_upgrade", thinkMeanMs: 5000, abandonProb: 0.40}, // pricing-page dwell
		},
	},
}

func pickJourney() journeyDef {
	total := 0.0
	for _, j := range journeys {
		total += j.weight
	}
	r := rand.Float64() * total
	for _, j := range journeys {
		r -= j.weight
		if r <= 0 {
			return j
		}
	}
	return journeys[0]
}

// avgStepsPerJourney converts target request-RPS into session launch rate.
func avgStepsPerJourney() float64 {
	total, weight := 0.0, 0.0
	for _, j := range journeys {
		total += float64(len(j.steps)) * j.weight
		weight += j.weight
	}
	return total / weight
}

// thinkTime draws a lognormal-ish delay around the mean; time-compressed
// runs shrink it so compressed days still finish their sessions.
func thinkTime(meanMs, compression float64) time.Duration {
	if meanMs <= 0 {
		return 0
	}
	// lognormal via exp of a normal centered to preserve the mean roughly.
	v := meanMs * math.Exp(rand.NormFloat64()*0.5-0.125)
	if compression > 1 {
		v /= compression
	}
	return time.Duration(v) * time.Millisecond
}

// runSession executes one journey to completion or abandonment.
func runSession(ctx context.Context, client *http.Client, baseURL string, compression float64) {
	j := pickJourney()
	for _, step := range j.steps {
		select {
		case <-ctx.Done():
			return
		case <-time.After(thinkTime(step.thinkMeanMs, compression)):
		}
		if step.abandonProb > 0 && rand.Float64() < step.abandonProb {
			return // the human wandered off
		}
		sendPlanned(ctx, client, baseURL, step.action)
	}
}

// runJourneyMode is the journey-mode main loop: a fractional session
// budget accumulates at the loadshape-derived rate; whole sessions launch
// as the budget crosses integers.
func runJourneyMode(ctx context.Context, client *http.Client, cfg config, shape *loadshape.Shape) {
	stepsPerJourney := avgStepsPerJourney()
	tick := 250 * time.Millisecond
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	budget := 0.0
	logEvery := time.NewTicker(60 * time.Second)
	defer logEvery.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("traffic-generator (journeys) stopping")
			globalStats.summarize()
			return
		case <-logEvery.C:
			slog.Info("load shape",
				"target_rps", shape.RPSAt(time.Now()),
				"health_factor", healthFactor(),
				"mode", "journeys")
		case <-ticker.C:
			targetRPS := shape.RPSAt(time.Now()) * healthFactor()
			sessionsPerSec := targetRPS / stepsPerJourney
			budget += sessionsPerSec * tick.Seconds()
			for budget >= 1 {
				budget--
				go runSession(ctx, client, cfg.TargetURL, shapeCompression(shape))
			}
		}
	}
}

// healthFactor shrinks traffic during simulated incidents — users bounce
// off a degraded site. 1.0 healthy → 0.55 at health 0.
func healthFactor() float64 {
	h := sysstate.HealthScore()
	return 0.55 + 0.45*h
}

// shapeCompression exposes the shape's time compression for think-time
// scaling without exporting the config struct field everywhere.
func shapeCompression(s *loadshape.Shape) float64 { return s.Compression() }

// shapeConfigFromEnv builds the loadshape config from LOADSHAPE_* env vars
// so the deployment can tune realism without a rebuild.
func shapeConfigFromEnv(baseRPS float64) loadshape.Config {
	cfg := loadshape.DefaultConfig(baseRPS)
	cfg.DiurnalAmplitude = envFloatOrDefault("LOADSHAPE_DIURNAL_AMPLITUDE", cfg.DiurnalAmplitude)
	cfg.WeekendFactor = envFloatOrDefault("LOADSHAPE_WEEKEND_FACTOR", cfg.WeekendFactor)
	cfg.LunchDip = envFloatOrDefault("LOADSHAPE_LUNCH_DIP", cfg.LunchDip)
	cfg.NoiseSigma = envFloatOrDefault("LOADSHAPE_NOISE_SIGMA", cfg.NoiseSigma)
	cfg.TimeCompression = envFloatOrDefault("LOADSHAPE_TIME_COMPRESSION", cfg.TimeCompression)
	cfg.MinRPS = envFloatOrDefault("LOADSHAPE_MIN_RPS", cfg.MinRPS)
	return cfg
}

func envFloatOrDefault(key string, def float64) float64 {
	if v := envOrDefault(key, ""); v != "" {
		var f float64
		if f2, err := strconv.ParseFloat(v, 64); err == nil {
			return f2
		}
		_ = f
	}
	return def
}
