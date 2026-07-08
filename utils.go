package main

import (
	"log"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// I know there are packages that make the config syntax better when loading from the environment
// but I didn't want to pull in extra dependencis

func envFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func envStrings(key string, fallback []string) []string {
	if v := os.Getenv(key); v != "" {
		return strings.Split(v, ",")
	}
	return fallback
}

func envString(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func envCIDRs(key string, fallback []netip.Prefix) []netip.Prefix {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}

	parts := strings.Split(raw, ",")
	prefixes := make([]netip.Prefix, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(p)
		if err != nil {
			// fail loudly at startup rather than silently ignoring a bad config
			log.Fatalf("invalid CIDR %q in %s: %v", p, key, err)
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}
