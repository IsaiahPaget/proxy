package main

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	cacheHits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "proxy_cache_hits_total",
		Help: "Total number of cache hits",
	})
	cacheMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "proxy_cache_misses_total",
		Help: "Total number of cache misses",
	})
	rateLimitRejected = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "proxy_rate_limit_rejected_total",
		Help: "Total number of requests rejected by rate limiter",
	})
	upstreamLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "proxy_upstream_request_duration_seconds",
		Help:    "Histogram of upstream request latency in seconds",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	})
)

func init() {
	f := []prometheus.Collector{
		cacheHits,
		cacheMisses,
		rateLimitRejected,
		upstreamLatency,
	}
	prometheus.MustRegister(f...)
}

func metricsHandler() http.Handler {
	return promhttp.Handler()
}

func cacheHitCallback()        { cacheHits.Inc() }
func cacheMissCallback()       { cacheMisses.Inc() }
func rateLimitRejectCallback() { rateLimitRejected.Inc() }

type latencyTransport struct {
	base http.RoundTripper
}

func (t *latencyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	upstreamLatency.Observe(time.Since(start).Seconds())
	return resp, err
}
