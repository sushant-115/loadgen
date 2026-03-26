package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"
)

type ChaosConfig struct {
	Type            string `json:"type"`
	TargetService   string `json:"target_service"`
	Intensity       int    `json:"intensity"`
	DurationSeconds int    `json:"duration_seconds"`
}

type ChaosState struct {
	mu              sync.Mutex
	active          bool
	chaosType       string
	intensity       int
	startTime       time.Time
	durationSeconds int
}

var (
	chaosState  = &ChaosState{}
	serviceName = getEnvVar("SERVICE_NAME", "chaos-service")
)

func getEnvVar(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}

func isChaosActive() bool {
	chaosState.mu.Lock()
	defer chaosState.mu.Unlock()

	if !chaosState.active {
		return false
	}

	elapsed := time.Since(chaosState.startTime)
	duration := time.Duration(chaosState.durationSeconds) * time.Second
	if elapsed > duration {
		chaosState.active = false
		log.Printf("Chaos expired for service %s", serviceName)
		return false
	}
	return true
}

func getChaosType() (string, int) {
	chaosState.mu.Lock()
	defer chaosState.mu.Unlock()
	return chaosState.chaosType, chaosState.intensity
}

func injectChaos(w http.ResponseWriter, r *http.Request) {
	chaosType, intensity := getChaosType()

	switch chaosType {
	case "latency":
		// Inject artificial latency (0-100ms based on intensity)
		delayMs := (intensity * 100) / 100
		time.Sleep(time.Duration(delayMs) * time.Millisecond)

	case "errors":
		// Randomly return errors based on intensity (0-100%)
		if rand.Intn(100) < intensity {
			http.Error(w, `{"error":"Service unavailable (chaos injected)"}`, http.StatusServiceUnavailable)
			return
		}

	case "cpu":
		// Simulate CPU spike with goroutines
		go func() {
			stopTime := time.Now().Add(time.Second)
			for time.Now().Before(stopTime) {
				_ = fib(30)
			}
		}()

	case "memory":
		// Simulate memory spike
		memAlloc := make([]byte, (intensity*10*1024*1024)/100)
		go func() {
			time.Sleep(time.Duration(5) * time.Second)
			_ = memAlloc // Reference to prevent GC
		}()
	}
}

func fib(n int) int {
	if n <= 1 {
		return n
	}
	return fib(n-1) + fib(n-2)
}

func handleDefault(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Check if chaos is active and inject it
	if isChaosActive() {
		injectChaos(w, r)
	}

	duration := float64(time.Since(start).Milliseconds())

	// Send response
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"service":        serviceName,
		"status":         "ok",
		"time":           time.Now().Unix(),
		"response_time":  duration,
		"chaos_active":   isChaosActive(),
	})
}

func handleChaos(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var cfg ChaosConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"Invalid request: %v"}`, err), http.StatusBadRequest)
		return
	}

	// Check if this target matches our service name
	if cfg.TargetService != serviceName && cfg.TargetService != "all" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "chaos not targeting this service",
			"service": serviceName,
		})
		return
	}

	// Activate chaos
	chaosState.mu.Lock()
	chaosState.active = true
	chaosState.chaosType = cfg.Type
	chaosState.intensity = cfg.Intensity
	chaosState.startTime = time.Now()
	chaosState.durationSeconds = cfg.DurationSeconds
	chaosState.mu.Unlock()

	log.Printf("Chaos activated: type=%s, intensity=%d, duration=%ds for service=%s",
		cfg.Type, cfg.Intensity, cfg.DurationSeconds, serviceName)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "activated",
		"type":     cfg.Type,
		"service":  serviceName,
		"intensity": cfg.Intensity,
		"duration": cfg.DurationSeconds,
	})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	active := isChaosActive()
	chaosState.mu.Lock()
	chaosType := chaosState.chaosType
	intensity := chaosState.intensity
	elapsed := time.Since(chaosState.startTime).Seconds()
	chaosState.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"service":   serviceName,
		"active":    active,
		"type":      chaosType,
		"intensity": intensity,
		"elapsed":   int(elapsed),
	})
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "healthy",
		"service": serviceName,
	})
}

func main() {
	log.SetPrefix("[chaos-service] ")
	log.Printf("Starting chaos service for %s", serviceName)

	http.HandleFunc("/chaos", handleChaos)
	http.HandleFunc("/chaos/status", handleStatus)
	http.HandleFunc("/healthz", handleHealthz)
	http.HandleFunc("/", handleDefault)

	log.Println("Listening on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
