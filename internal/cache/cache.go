package cache

import (
	"bytes"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type key string

type entry struct {
	Data      []byte
	Headers   http.Header
	Status    int
	expiresAt time.Time
	cachedAt  time.Time
	size      int64
}

type Config struct {
	MaxSize int64
	TTL     time.Duration
	OnHit   func()
	OnMiss  func()
}

type Cache struct {
	sync.RWMutex
	singleflight.Group
	entries     map[string]*entry
	currentSize int64
	config      Config
}

func New(cfg Config) *Cache {
	return &Cache{
		entries: make(map[string]*entry),
		config:  cfg,
	}
}

// Method is not included in the key because this service only accepts GET request
func (c *Cache) key(r http.Request) key {
	return key(r.URL.String() + "\x00" + r.Header.Get("Accept"))
}

func shouldCache(cacheControl string) bool {
	for directive := range strings.SplitSeq(cacheControl, ",") {
		switch strings.TrimSpace(strings.ToLower(directive)) {
		case "no-store", "private", "no-cache":
			return false
		}
	}
	return true
}

func parseMaxAge(cacheControl string) time.Duration {
	for directive := range strings.SplitSeq(cacheControl, ",") {
		directive = strings.TrimSpace(directive)
		if strings.HasPrefix(strings.ToLower(directive), "max-age=") {
			val := strings.TrimPrefix(directive, "max-age=")
			if seconds, err := strconv.Atoi(val); err == nil && seconds > 0 {
				return time.Duration(seconds) * time.Second
			}
		}
	}
	return 0
}

func (c *Cache) get(k key) (*entry, bool) {
	c.Lock()
	defer c.Unlock()

	entry, ok := c.entries[string(k)]
	if !ok {
		return nil, false
	}

	if time.Now().After(entry.expiresAt) {
		delete(c.entries, string(k))
		c.currentSize -= entry.size
		return nil, false
	}

	return entry, true
}

func (c *Cache) set(k key, e *entry) {
	e.size = int64(len(e.Data))

	c.Lock()
	defer c.Unlock()

	for c.currentSize+e.size > c.config.MaxSize {
		if !c.evictOldestLocked() {
			break
		}
	}

	c.entries[string(k)] = e
	c.currentSize += e.size
}

// evictOldestLocked assumes that it's operating in a locked context
func (c *Cache) evictOldestLocked() bool {
	var oldestKey string
	var oldestTime time.Time

	for key, entry := range c.entries {
		if oldestKey == "" || entry.expiresAt.Before(oldestTime) {
			oldestKey = key
			oldestTime = entry.expiresAt
		}
	}

	if oldestKey == "" {
		return false
	}

	c.currentSize -= c.entries[oldestKey].size
	delete(c.entries, oldestKey)
	return true
}

func (c *Cache) StartCleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		slog.Info("Cleaning up cache entries", "count", len(c.entries))
		c.cleanup(time.Now())
	}
}

func (c *Cache) cleanup(now time.Time) {
	c.Lock()
	defer c.Unlock()
	for key, entry := range c.entries {
		if now.After(entry.expiresAt) {
			c.currentSize -= entry.size
			delete(c.entries, key)
		}
	}
}

func (e *entry) write(w http.ResponseWriter, age ...int) {
	maps.Copy(w.Header(), e.Headers)
	if len(age) > 0 && age[0] > 0 {
		w.Header().Set("Age", strconv.Itoa(age[0]))
	}
	w.WriteHeader(e.Status)
	w.Write(e.Data)
}

type ResponseRecorder struct {
	http.ResponseWriter
	body       bytes.Buffer
	statusCode int
	headers    http.Header
}

func (rr *ResponseRecorder) Header() http.Header {
	if rr.headers == nil {
		rr.headers = make(http.Header)
	}
	return rr.headers
}

func (rr *ResponseRecorder) WriteHeader(statusCode int) {
	rr.statusCode = statusCode
}

func (rr *ResponseRecorder) Write(b []byte) (int, error) {
	if rr.statusCode == 0 {
		rr.statusCode = http.StatusOK
	}
	return rr.body.Write(b)
}

func writeResponse(w http.ResponseWriter, data []byte, headers http.Header, status int, age int) {
	maps.Copy(w.Header(), headers)
	if age > 0 {
		w.Header().Set("Age", strconv.Itoa(age))
	}
	w.WriteHeader(status)
	w.Write(data)
}

func (c *Cache) Cache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			next.ServeHTTP(w, r)
			return
		}
		key := c.key(*r)

		if entry, ok := c.get(key); ok {
			slog.Info("Cache hit", "Key", string(key))
			if c.config.OnHit != nil {
				c.config.OnHit()
			}
			entry.write(w)
			return
		}

		val, _, _ := c.Group.Do(string(key), func() (any, error) {
			if c.config.OnMiss != nil {
				c.config.OnMiss()
			}
			rr := &ResponseRecorder{ResponseWriter: w}
			next.ServeHTTP(rr, r)

			now := time.Now()
			e := &entry{
				Data:     rr.body.Bytes(),
				Headers:  rr.Header(),
				Status:   rr.statusCode,
				cachedAt: now,
			}

			if rr.statusCode >= 200 && rr.statusCode < 300 {
				cc := rr.Header().Get("Cache-Control")
				if shouldCache(cc) {
					ttl := c.config.TTL
					if maxAge := parseMaxAge(cc); maxAge > 0 && maxAge < ttl {
						ttl = maxAge
					}
					e.expiresAt = now.Add(ttl)

					slog.Info("Caching", "Key", string(key))
					c.set(key, e)
					return e, nil
				}
			}

			return e, nil
		})

		entry := val.(*entry)

		var age int
		if !entry.expiresAt.IsZero() {
			age = int(time.Since(entry.cachedAt).Seconds())
		}

		writeResponse(w, entry.Data, entry.Headers, entry.Status, age)
	})
}
