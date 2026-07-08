package main

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/isaiahpaget/proxy/internal/cache"
	"github.com/isaiahpaget/proxy/internal/proxy"
	"github.com/isaiahpaget/proxy/internal/ratelimiter"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"golang.org/x/time/rate"
)

// randomIP returns a pseudo-random address in the 10.0.0.0/8 private range.
func randomIP() string {
	return fmt.Sprintf("10.%d.%d.%d", rand.Intn(256), rand.Intn(256), rand.Intn(256))
}

// startLocalServer builds an Application against a local listener backed by
// the supplied backend, returning the proxy base URL. The caller should
// defer srv.Close() and listener.Close().
func startLocalServer(t testing.TB, backend *httptest.Server, opts ...func(*Config)) (string, func()) {
	t.Helper()

	cfg := Config{
		addr:             ":0",
		AllowedUpstreams: []string{backend.URL},
	}
	for _, fn := range opts {
		fn(&cfg)
	}

	c := cache.New(cache.Config{MaxSize: 2048, TTL: time.Minute})
	rl := ratelimiter.New(ratelimiter.Config{Rate: rate.Limit(10000), Burst: 10000})
	p := proxy.New()

	app := Application{
		config:      cfg,
		p:           p,
		ratelimiter: rl,
		cache:       c,
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	srv := &http.Server{
		Handler:           app.mount(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go srv.Serve(listener)

	return "http://" + listener.Addr().String(), func() {
		srv.Close()
		listener.Close()
	}
}

// TestCacheStampede now actually fails when coalescing doesn't happen
// (previously both branches only called t.Logf, so this test could never
// fail regardless of behavior). It also replaces the fixed 10ms sleep
// before releasing the barrier with an explicit "all goroutines are ready"
// signal, since a fixed sleep gives no real guarantee that all 100
// goroutines reached the barrier before it's closed — under load that
// race could mask a real stampede by not actually sending concurrent
// requests.
func TestCacheStampede(t *testing.T) {
	var backendHits atomic.Int32

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHits.Add(1)
		w.Header().Set("X-Backend", "true")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("backend-response"))
	}))
	defer backend.Close()

	cacheTTL := 100 * time.Millisecond

	c := cache.New(cache.Config{
		MaxSize: 2048,
		TTL:     cacheTTL,
	})
	go c.StartCleanup()

	rl := ratelimiter.New(ratelimiter.Config{
		Rate:  rate.Limit(10000),
		Burst: 10000,
	})

	p := proxy.New()

	app := Application{
		config: Config{
			addr:             ":0",
			AllowedUpstreams: []string{backend.URL},
		},
		p:           p,
		ratelimiter: rl,
		cache:       c,
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	srv := &http.Server{
		Handler:           app.mount(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go srv.Serve(listener)
	defer srv.Close()

	proxyBase := "http://" + listener.Addr().String()
	proxyURL := proxyBase + "/proxy?url=" + url.QueryEscape(backend.URL+"/data")

	req, err := http.NewRequest("GET", proxyURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", "10.0.0.1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if hits := backendHits.Load(); hits != 1 {
		t.Fatalf("expected 1 backend hit after cache population, got %d", hits)
	}

	backendHits.Store(0)
	time.Sleep(cacheTTL + 50*time.Millisecond)

	concurrency := 100
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	ready.Add(concurrency)
	barrier := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			req, err := http.NewRequest("GET", proxyURL, nil)
			if err != nil {
				t.Errorf("goroutine %d: %v", id, err)
				ready.Done()
				return
			}
			req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.0.%d.%d", id/256, id%256))

			ready.Done()
			<-barrier

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("goroutine %d: %v", id, err)
				return
			}
			resp.Body.Close()
		}(i)
	}

	ready.Wait() // all goroutines have built their request and are parked at the barrier
	close(barrier)
	wg.Wait()

	hits := backendHits.Load()
	if hits > 1 {
		t.Errorf("CACHE STAMPEDE DETECTED: backend received %d requests (expected 1) from %d concurrent clients", hits, concurrency)
	} else {
		t.Logf("No stampede: backend received %d request from %d concurrent clients", hits, concurrency)
	}
}

// FuzzProxy_CacheConsistency exercises cache-key/consistency behavior by
// proxying to a local httptest server that mirrors request headers back.
func FuzzProxy_CacheConsistency(f *testing.F) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path":%q,"accept":%q}`, r.URL.Path, r.Header.Get("Accept"))
	}))
	f.Cleanup(backend.Close)

	proxyBase, cleanup := startLocalServer(f, backend)
	f.Cleanup(cleanup)

	f.Add(backend.URL + "/get")
	f.Add(backend.URL + "/anything")
	f.Add(backend.URL + "/headers")

	f.Fuzz(func(t *testing.T, urlParam string) {
		makeRequest := func() (int, []byte, error) {
			req, err := http.NewRequest("GET", proxyBase+"/proxy?url="+url.QueryEscape(urlParam), nil)
			if err != nil {
				return 0, nil, err
			}
			req.Header.Set("X-Forwarded-For", randomIP())
			req.Header.Set("Accept", "application/json")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return 0, nil, err
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			return resp.StatusCode, body, err
		}

		status1, body1, err1 := makeRequest()
		if err1 != nil {
			return
		}

		status2, body2, err2 := makeRequest()
		if err2 != nil {
			t.Fatalf("second request failed: %v", err2)
		}

		if status1 != status2 {
			t.Fatalf("status mismatch: %d vs %d for %q", status1, status2, urlParam)
		}
		if string(body1) != string(body2) {
			t.Fatalf("body mismatch for %q\nFirst:\n%s\nSecond:\n%s", urlParam, body1, body2)
		}
	})
}

// FuzzProxy_UpstreamAllowlist exercises the allowlist check by proxying
// to a local httptest server.
func FuzzProxy_UpstreamAllowlist(f *testing.F) {
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	f.Cleanup(allowed.Close)

	allowedHost := strings.TrimPrefix(strings.TrimPrefix(allowed.URL, "http://"), "https://")

	proxyBase, cleanup := startLocalServer(f, allowed)
	f.Cleanup(cleanup)

	f.Add(allowed.URL + "/get")
	f.Add(allowed.URL + "/anything")
	f.Add(allowed.URL + "/headers")
	f.Add("http://evil.com/steal")
	f.Add("http://notallowed.com")
	f.Add("http://malicious.xyz/data")
	f.Add("not-a-url")
	f.Add("://bad")
	f.Add("")
	f.Add("'; DROP TABLE users; --")
	f.Add("../../../etc/passwd")
	f.Add("javascript:alert(1)")
	f.Add("file:///etc/passwd")

	f.Fuzz(func(t *testing.T, urlParam string) {
		req, err := http.NewRequest("GET", proxyBase+"/proxy?url="+url.QueryEscape(urlParam), nil)
		if err != nil {
			t.Skip(err)
		}
		req.Header.Set("X-Forwarded-For", randomIP())

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		resp.Body.Close()

		parsed, parseErr := url.Parse(urlParam)
		isAllowed := parseErr == nil && slices.ContainsFunc([]string{allowedHost}, func(host string) bool {
			return parsed.Host == host
		})

		switch {
		case parseErr != nil:
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("unparsable URL %q: got %d, want 400", urlParam, resp.StatusCode)
			}
		case !isAllowed:
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("disallowed upstream %q: got %d, want 403", urlParam, resp.StatusCode)
			}
		default:
			if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusForbidden {
				t.Errorf("allowed upstream %q: got %d, want 2xx", urlParam, resp.StatusCode)
			}
		}
	})
}

func startContainer(t testing.TB, envs ...string) (context.Context, string) {
	t.Helper()

	ctx := context.Background()

	env := make(map[string]string)
	for _, e := range envs {
		k, v, _ := strings.Cut(e, "=")
		env[k] = v
	}

	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:    "./",
			Dockerfile: "Dockerfile",
		},
		ExposedPorts: []string{"8080/tcp"},
		WaitingFor:   wait.ForHTTP("/health"),
		Env:          env,
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { container.Terminate(ctx) })

	endpoint, err := container.Endpoint(ctx, "http")
	if err != nil {
		t.Fatal(err)
	}

	return ctx, endpoint
}

func FuzzProxy_URLParam(f *testing.F) {
	_, endpoint := startContainer(f, "ENV=dev")

	f.Add("")
	f.Add(strings.Repeat(" ", 10000))
	f.Add("http://example.com")
	f.Add("not-a-url")
	f.Add("://bad")
	f.Add("http://localhost:9999")
	f.Add("'; DROP TABLE users; --")
	f.Add("../../../etc/passwd")
	f.Add("http://" + strings.Repeat("A", 10000))
	f.Add("http://user:pass@evil.com")
	f.Add("javascript:alert(1)")
	f.Add("file:///etc/passwd")

	f.Fuzz(func(t *testing.T, urlParam string) {
		req, err := http.NewRequest("GET", endpoint+"/proxy?url="+url.QueryEscape(urlParam), nil)
		if err != nil {
			t.Skip(err)
		}
		req.Header.Set("X-Forwarded-For", randomIP())

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		resp.Body.Close()

		healthResp, err := http.Get(endpoint + "/health")
		if err != nil {
			t.Fatalf("health check failed after proxying %q: %v", urlParam, err)
		}
		healthBody, err := io.ReadAll(healthResp.Body)
		healthResp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if healthResp.StatusCode != http.StatusOK || string(healthBody) != "all good" {
			t.Fatalf("health check returned %d %q after proxying %q", healthResp.StatusCode, healthBody, urlParam)
		}
	})
}
