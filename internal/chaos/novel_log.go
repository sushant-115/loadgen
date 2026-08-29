// novel_log.go — the anti-logstorm. A log storm tests VOLUME detection; a
// real incident's tell is often a handful of lines whose SHAPE has never
// been seen before ("lease renewal failed for shard 7: context deadline").
// This fault emits genuinely novel templates at LOW volume so it exercises
// InfraSage's template-novelty detection specifically: if volume-based
// detection fires on this, that's a finding too (it shouldn't need to).
package chaos

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"
)

// novelTemplates are message SHAPES the baseline never emits. Each pass
// mutates identifiers (which template mining should normalize away) while
// the template itself stays stable per activation — one new shape per
// activation, seen repeatedly, is the classic "new failure mode" signal.
// Every template takes exactly three integer verbs so one args tuple fits all.
var novelTemplates = []string{
	"lease renewal failed for shard %d: context deadline exceeded after %dms (attempt %d)",
	"checksum mismatch on segment %d: expected 0x%x got 0x%x — entering read-only mode",
	"circuit half-open probe to replica-%d rejected: draining connection backlog (%d pending, %d dropped)",
	"schema cache invalidation storm: %d entries evicted in epoch %d (generation %d)",
	"watchdog: heartbeat from worker-%d stale by %ds — quarantining partition %d",
	"tls ticket rotation failed on listener %d: falling back to full handshakes (%d/s, %d clients)",
}

func startNovelLog(intensity float64, duration time.Duration) {
	// Intensity maps to lines/minute (1-30) — deliberately quiet.
	perMinute := int(intensity * 30)
	if perMinute < 1 {
		perMinute = 1
	}
	tmpl := novelTemplates[rand.IntN(len(novelTemplates))]
	if duration <= 0 {
		duration = 10 * time.Minute
	}
	go func() {
		ticker := time.NewTicker(time.Minute / time.Duration(perMinute))
		defer ticker.Stop()
		deadline := time.After(duration)
		for {
			select {
			case <-deadline:
				return
			case <-ticker.C:
				if !IsActive(NovelLog) {
					return
				}
				slog.Error(fmt.Sprintf(tmpl, rand.IntN(64), rand.IntN(9000)+500, rand.IntN(1<<24)),
					"chaos", true, "fault", "novel_log")
			}
		}
	}()
}
