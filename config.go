package main

import (
	"bufio"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	ListenAddr    string
	FlushInterval time.Duration
	StickDeadzone float64
	TriggerMax    float64
	AuthToken     string
}

// Allowed ranges. Out-of-range values are clamped rather than rejected so a
// hand-edited .env still starts the server.
const (
	minFlushMs    = 1
	maxFlushMs    = 32
	maxDeadzone   = 0.5
	minTriggerMax = 0.1
)

func clampFlush(d time.Duration) time.Duration {
	ms := d.Milliseconds()
	if ms < minFlushMs {
		return minFlushMs * time.Millisecond
	}
	if ms > maxFlushMs {
		return maxFlushMs * time.Millisecond
	}
	return d
}

func clampDeadzone(f float64) float64 { return math.Max(0, math.Min(maxDeadzone, f)) }

func clampTrigger(f float64) float64 { return math.Max(minTriggerMax, math.Min(1, f)) }

func loadConfig() config {
	cfg := config{
		ListenAddr:    ":9449",
		FlushInterval: 8 * time.Millisecond,
		StickDeadzone: 0.08,
		TriggerMax:    1.0,
	}

	// Try to load .env from the working directory.
	loadDotenv(".env")

	cfg.ListenAddr = envStr("LISTEN", cfg.ListenAddr)
	cfg.FlushInterval = clampFlush(envDuration("FLUSH_INTERVAL_MS", cfg.FlushInterval))
	cfg.StickDeadzone = clampDeadzone(envFloat("STICK_DEADZONE", cfg.StickDeadzone))
	cfg.TriggerMax = clampTrigger(envFloat("TRIGGER_THRESHOLD", cfg.TriggerMax))
	cfg.AuthToken = envStr("AUTH_TOKEN", "")

	return cfg
}

// loadDotenv reads a simple KEY=VALUE file (no expansion, no quoting).
func loadDotenv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // silent — file just doesn't exist
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" {
			continue
		}
		// Only set if not already in environment (env var wins).
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	s := os.Getenv(key)
	if s == "" {
		return fallback
	}
	ms, err := strconv.Atoi(s)
	if err != nil {
		log.Printf("config: bad %q=%q, using default %v", key, s, fallback)
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}

func envFloat(key string, fallback float64) float64 {
	s := os.Getenv(key)
	if s == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		log.Printf("config: bad %q=%q, using default %v", key, s, fallback)
		return fallback
	}
	return v
}
