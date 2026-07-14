package cache

import (
	"net/http"
	"testing"
	"time"
)

func TestSetAndGet(t *testing.T) {
	c := New(Config{MaxSize: 1024, TTL: time.Minute})

	entry := &entry{
		Data:      []byte("hello"),
		Headers:   http.Header{"Content-Type": {"text/plain"}},
		Status:    200,
		expiresAt: time.Now().Add(time.Minute),
	}

	c.set("key1", entry)

	got, ok := c.get("key1")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if string(got.Data) != "hello" {
		t.Fatalf("expected data 'hello', got %q", got.Data)
	}
	if got.Status != 200 {
		t.Fatalf("expected status 200, got %d", got.Status)
	}
	if got.Headers.Get("Content-Type") != "text/plain" {
		t.Fatalf("expected Content-Type text/plain, got %q", got.Headers.Get("Content-Type"))
	}
}

func TestGetMiss(t *testing.T) {
	c := New(Config{MaxSize: 1024, TTL: time.Minute})

	_, ok := c.get("nonexistent")
	if ok {
		t.Fatal("expected cache miss")
	}
}

func TestGetExpired(t *testing.T) {
	c := New(Config{MaxSize: 1024, TTL: time.Millisecond})

	c.set("key1", &entry{
		Data: []byte("hello"), Status: 200,
		expiresAt: time.Now().Add(time.Millisecond),
	})

	time.Sleep(5 * time.Millisecond)

	_, ok := c.get("key1")
	if ok {
		t.Fatal("expected cache miss for expired entry")
	}
}

func TestCleanup(t *testing.T) {
	c := New(Config{MaxSize: 1024, TTL: time.Millisecond})

	c.set("key1", &entry{
		Data: []byte("hello"), Status: 200,
		expiresAt: time.Now().Add(time.Millisecond),
	})
	c.set("key2", &entry{
		Data: []byte("world"), Status: 200,
		expiresAt: time.Now().Add(time.Millisecond),
	})

	time.Sleep(5 * time.Millisecond)

	c.cleanup(time.Now())

	c.Lock()
	count := len(c.entries)
	c.Unlock()

	if count != 0 {
		t.Fatalf("expected 0 entries after cleanup, got %d", count)
	}
}

func TestKeyConsistency(t *testing.T) {
	c := New(Config{MaxSize: 1024, TTL: time.Minute})

	r1, _ := http.NewRequest("GET", "http://example.com/api?q=1", nil)
	r1.Header.Set("Accept", "application/json")
	r1.Header.Set("Authorization", "Bearer token123")

	r2, _ := http.NewRequest("GET", "http://example.com/api?q=1", nil)
	r2.Header.Set("Accept", "application/json")
	r2.Header.Set("Authorization", "Bearer token123")

	if c.key(*r1) != c.key(*r2) {
		t.Fatal("expected same key for identical requests")
	}
}

func TestKeyDiffers(t *testing.T) {
	c := New(Config{MaxSize: 1024, TTL: time.Minute})

	r1, _ := http.NewRequest("GET", "http://example.com/api", nil)
	r1.Header.Set("Accept", "application/json")

	r2, _ := http.NewRequest("GET", "http://example.com/api", nil)
	r2.Header.Set("Accept", "text/html")

	if c.key(*r1) == c.key(*r2) {
		t.Fatal("expected different keys for different Accept headers")
	}
}

func TestMaxSizeEviction(t *testing.T) {
	c := New(Config{MaxSize: 20, TTL: time.Minute})

	c.set("key1", &entry{
		Data: []byte("aaaaaaaa"), Status: 200,
		expiresAt: time.Now().Add(time.Minute),
	})
	c.set("key2", &entry{
		Data: []byte("bbbbbbbb"), Status: 200,
		expiresAt: time.Now().Add(time.Minute),
	})
	c.set("key3", &entry{
		Data: []byte("cccccccc"), Status: 200,
		expiresAt: time.Now().Add(time.Minute),
	})

	c.Lock()
	count := len(c.entries)
	size := c.currentSize
	c.Unlock()

	if count != 2 {
		t.Fatalf("expected 2 entries after exceeding MaxSize, got %d", count)
	}
	if size > 20 {
		t.Fatalf("expected currentSize <= 20, got %d", size)
	}
	if _, ok := c.get("key3"); !ok {
		t.Fatal("expected key3 (newest) to survive eviction")
	}
	if _, ok := c.get("key2"); !ok {
		t.Fatal("expected key2 to survive eviction")
	}
	if _, ok := c.get("key1"); ok {
		t.Fatal("expected key1 (oldest) to be evicted")
	}
}

func TestCacheRespectsNoStore(t *testing.T) {
	if shouldCache("no-store") {
		t.Fatal("expected shouldCache to return false for no-store")
	}
}

func TestCacheRespectsPrivate(t *testing.T) {
	if shouldCache("private") {
		t.Fatal("expected shouldCache to return false for private")
	}
}

func TestCacheRespectsNoCache(t *testing.T) {
	if shouldCache("no-cache") {
		t.Fatal("expected shouldCache to return false for no-cache")
	}
}

func TestCacheRespectsMaxAge(t *testing.T) {
	ttl := parseMaxAge("max-age=5")
	if ttl != 5*time.Second {
		t.Fatalf("expected max-age to parse to 5s, got %v", ttl)
	}
}

func TestCacheSetsAgeHeader(t *testing.T) {
	c := New(Config{MaxSize: 1024, TTL: time.Minute})

	e := &entry{
		Data:      []byte("hello"),
		Headers:   http.Header{"Content-Type": {"text/plain"}},
		Status:    200,
		expiresAt: time.Now().Add(time.Minute),
		cachedAt:  time.Now(),
	}

	c.set("key1", e)

	got, ok := c.get("key1")
	if !ok {
		t.Fatal("expected cache hit")
	}

	if got.cachedAt.IsZero() {
		t.Fatal("expected cachedAt to be set")
	}

	age := int(time.Since(got.cachedAt).Seconds())
	if age > 1 {
		t.Fatalf("expected Age to be ~0, got %d", age)
	}
}

func TestCachePreservesDateHeader(t *testing.T) {
	c := New(Config{MaxSize: 1024, TTL: time.Minute})

	originalDate := "Sat, 11 Jul 2026 20:00:00 GMT"
	entry := &entry{
		Data:      []byte("hello"),
		Headers:   http.Header{"Date": {originalDate}},
		Status:    200,
		expiresAt: time.Now().Add(time.Minute),
	}

	c.set("key1", entry)

	got, ok := c.get("key1")
	if !ok {
		t.Fatal("expected cache hit")
	}

	if got.Headers.Get("Date") != originalDate {
		t.Fatalf("expected Date header %q, got %q", originalDate, got.Headers.Get("Date"))
	}
}
