package main

import (
	"log/slog"
	"net/netip"
	"os"
	"time"
	"github.com/isaiahpaget/proxy/internal/cache"
	"github.com/isaiahpaget/proxy/internal/proxy"
	"github.com/isaiahpaget/proxy/internal/ratelimiter"
	"golang.org/x/time/rate"
)

const (
	PROD = "prod"
	DEV  = "dev"
)

func main() {
	cfg := Config{
		env:              envString("ENV", PROD),
		addr:             envString("ADDR", ":8080"),
		AllowedUpstreams: envStrings("ALLOWED_UPSTREAMS", []string{"https://httpbin.org/get"}),
		TrustedProxies:   envCIDRs("TRUSTED_PROXIES", []netip.Prefix{}),
		readTimeout:      time.Duration(envInt("READ_TIMEOUT_SECONDS", 10)) * time.Second,
		writeTimeout:     time.Duration(envInt("WRITE_TIMEOUT_SECONDS", 30)) * time.Second,
		idleTimeout:      time.Duration(envInt("IDLE_TIMEOUT_SECONDS", 120)) * time.Second,
	}

	rl := ratelimiter.New(ratelimiter.Config{
		Rate:      rate.Limit(envFloat("RATE", 10)),
		Burst:     int(envFloat("BURST", 20)),
		ClientTTL: 3 * time.Minute,
		OnReject:  rateLimitRejectCallback,
	})

	go rl.StartCleanup()

	p := proxy.New()

	// For metrics
	p.Transport = &latencyTransport{base: p.Transport}

	c := cache.New(cache.Config{
		MaxSize: int64(envInt("CACHE_MAX_SIZE", 2048)),
		TTL:     time.Duration(envInt("CACHE_TTL_SECONDS", 60)) * time.Second,
		OnHit:   cacheHitCallback,
		OnMiss:  cacheMissCallback,
	})
	go c.StartCleanup()

	app := Application{
		config:      cfg,
		ratelimiter: rl,
		p:           p,
		cache:       c,
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := app.run(app.mount()); err != nil {
		slog.Error("Server has failed to start", "error", err)
		os.Exit(1)
	}
}
