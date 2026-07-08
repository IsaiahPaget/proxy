package main

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"time"

	"github.com/isaiahpaget/proxy/internal/cache"
	"github.com/isaiahpaget/proxy/internal/proxy"
	"github.com/isaiahpaget/proxy/internal/ratelimiter"
)

type Application struct {
	config      Config
	p           *proxy.Proxy
	ratelimiter *ratelimiter.RateLimiter
	cache       *cache.Cache
}

type Env string

type Config struct {
	env          string
	addr         string
	writeTimeout time.Duration
	readTimeout  time.Duration
	idleTimeout  time.Duration
	AllowedUpstreams []string
	TrustedProxies   []netip.Prefix
}

func (app *Application) isTrustedProxy(addr netip.Addr) bool {
	for _, prefix := range app.config.TrustedProxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (app *Application) run(h http.Handler) error {
	srv := &http.Server{
		Addr:         app.config.addr,
		Handler:      h,
		WriteTimeout: app.config.writeTimeout,
		ReadTimeout:  app.config.readTimeout,
		IdleTimeout:  app.config.idleTimeout,
	}

	slog.Info("Server has started at", "address", srv.Addr)

	return srv.ListenAndServe()
}

func isAllowedUpstream(target url.URL, allowedUpstreams []string) bool {
	return slices.ContainsFunc(allowedUpstreams, func(u string) bool {
		_url, error := url.Parse(u)
		if error != nil {
			return false
		}
		return target.Host == _url.Host
	})
}

func (app *Application) OnlyTrustedCommunication(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, "Malformed request in remote address", http.StatusBadRequest)
			return
		}
		// NOTE: if we are in dev mode I just want to blindly trust X-forwarded-For
		if app.config.env == PROD {
			// using netip because it allows specifying a range
			addr, err := netip.ParseAddr(remoteIP)
			if err != nil {
				http.Error(w, "Malformed ip", http.StatusBadRequest)
			}
			if !app.isTrustedProxy(addr) {
				http.Error(w, "Forbidden", http.StatusForbidden)
			}
		}

		target, err := url.Parse(r.URL.Query().Get("url"))
		if err != nil {
			http.Error(w, "Invalid URL param", http.StatusBadRequest)
			return
		}

		if ok := isAllowedUpstream(*target, app.config.AllowedUpstreams); !ok {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (app *Application) healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("all good"))
}

func (app *Application) mount() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", app.healthCheckHandler)
	mux.Handle("GET /metrics", metricsHandler())
	mux.Handle("GET /proxy",
		app.OnlyTrustedCommunication(
			app.ratelimiter.RateLimit(
				app.cache.Cache(http.HandlerFunc(app.p.ProxyHandlerFunc)),
			),
		))
	return mux
}
