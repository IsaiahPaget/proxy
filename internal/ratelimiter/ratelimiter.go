package ratelimiter

import (
	"fmt"
	"log/slog"

	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type Config struct {
	Rate      rate.Limit
	Burst     int
	ClientTTL time.Duration
	OnReject  func()
}

type RateLimiter struct {
	sync.RWMutex
	clients map[string]*client
	config  Config
}

type client struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func New(cfg Config) *RateLimiter {
	return &RateLimiter{
		clients: make(map[string]*client),
		config:  cfg,
	}
}

func (rl *RateLimiter) allow(ip string) (*client, time.Duration) {
	c := rl.getClient(ip)
	if !c.limiter.Allow() {
		slog.Info("Rate limiting", "ip", ip)
		delay := c.limiter.Reserve().DelayFrom(time.Now())
		return nil, delay
	}
	return c, 0
}

func (rl *RateLimiter) getClient(ip string) *client {
	rl.Lock()
	defer rl.Unlock()

	c, exists := rl.clients[ip]
	if !exists {
		c = &client{
			limiter: rate.NewLimiter(rl.config.Rate, rl.config.Burst),
		}
		rl.clients[ip] = c
	}
	c.lastSeen = time.Now()
	return c
}

func (rl *RateLimiter) StartCleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		slog.Info("Cleaning up clients", "count", len(rl.clients))
		rl.cleanupClients()
	}
}

func (rl *RateLimiter) cleanupClients() {
	rl.Lock()
	defer rl.Unlock()

	for ip, v := range rl.clients {
		if time.Since(v.lastSeen) > rl.config.ClientTTL {
			delete(rl.clients, ip)
		}
	}
}

func (rl *RateLimiter) RateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// NOTE: This assumes the clients and proxies in between have
		// appended the X-Forwarded-For in the right order
		fwd := r.Header.Get("X-Forwarded-For")
		parts := strings.Split(fwd, ",")
		ip := strings.TrimSpace(parts[len(parts)-1])
		key := ip
		if key == "" {
			remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				http.Error(w, "Malformed request in remote address", http.StatusBadRequest)
				return
			}
			key = remoteIP
		}

		if _, delay := rl.allow(ip); delay > 0 {
			w.Header().Set("Retry-After", fmt.Sprintf("%.0f", delay.Seconds()))

			if rl.config.OnReject != nil {
				rl.config.OnReject()
			}
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
