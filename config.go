package main

import (
	"bufio"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	ListenAddr     string
	FlushInterval  time.Duration
	StickDeadzone  float64
	TriggerMax     float64
}

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
	cfg.FlushInterval = envDuration("FLUSH_INTERVAL_MS", cfg.FlushInterval)
	cfg.StickDeadzone = envFloat("STICK_DEADZONE", cfg.StickDeadzone)
	cfg.TriggerMax = envFloat("TRIGGER_THRESHOLD", cfg.TriggerMax)

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
