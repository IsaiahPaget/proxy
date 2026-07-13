# ProxyCache

A proxy the browser can call to cache responses from upstream APIs that are expensive to call, while also solving the CORS problem.

## Build & Run

**Local (no Docker):**

> Note: there's no `.env` file reader, so running locally uses the hard-coded fallback values.

```bash
go run .
```

**Docker:**

```bash
docker build -t proxycache .
docker run -p 8080:8080 proxycache
```

**Docker Compose** (for dev):

    Note: this also spins up Prometheus
```bash
docker-compose -f docker-compose.dev.yml up --build
```

The server listens on `:8080` by default.

## Testing

```bash

# Fuzz tests (run with -fuzz to do continuous fuzzing)
go test -run FuzzProxy_CacheConsistency -fuzz=. -fuzztime=30s .
go test -run FuzzProxy_UpstreamAllowlist -fuzz=. -fuzztime=30s .
go test -run FuzzProxy_URLParam -fuzz=. -fuzztime=30s .
```

## API

### `GET /proxy?url=<url-encoded target URL>`

**Request flow**

```
Client -> Service -> OnlyTrustedCommunication -> Rate limiter middleware -> limited     -> Client
                                                                          -> not limited -> Caching middleware -> cache hit  -> Client
                                                                                                                -> cache miss -> ReverseProxy -> Client
````

**Example:**

```bash
curl "http://localhost:8080/proxy?url=http%3A%2F%2Fhttpbin.org%2Fget"
```

### `GET /health`

Returns `200 OK` with body `all good`. Used for liveness probes.

### `GET /metrics`

Returns Prometheus metrics. Not rate-limited.

| Metric | Type | Description |
|---|---|---|
| `proxy_cache_hits_total` | Counter | Total cache hits |
| `proxy_cache_misses_total` | Counter | Total cache misses |
| `proxy_upstream_request_duration_seconds` | Histogram | Upstream request latency |
| `proxy_rate_limit_rejected_total` | Counter | Total 429 responses |

## Configuration

All configuration is via environment variables. Defaults are shown in parentheses.

| Variable | Type | Default | Description |
|---|---|---|---|
| `ADDR` | string | `:8080` | Address to listen on |
| `RATE` | float | `10` | Token-bucket refill rate (requests/sec) per client IP |
| `BURST` | float | `20` | Maximum burst size per client IP |
| `CACHE_MAX_SIZE` | int | `2048` | Maximum cache size in bytes |
| `CACHE_TTL_SECONDS` | int | `60` | Time-to-live for cached entries, in seconds |
| `ALLOWED_UPSTREAMS` | string | `https://httpbin.org/get` | Comma-separated list of allowed upstream hostnames. Requests to unknown hosts are rejected with `403 Forbidden` |
| `TRUSTED_PROXIES` | string | (empty) | Comma-separated CIDR list of trusted proxy IPs (e.g. `10.0.0.0/8,172.16.0.0/12`). When the connecting IP matches, `X-Forwarded-For` is trusted for client IP extraction |
| `ENV` | string | `prod` | When `dev`, always trusts `X-Forwarded-For`, regardless of `TRUSTED_PROXIES`. Intended for local development and tests only |
| `READ_TIMEOUT_SECONDS` | int | `10` | Maximum duration for reading the entire request, including the body |
| `WRITE_TIMEOUT_SECONDS` | int | `30` | Maximum duration before timing out the response write (should exceed upstream latency) |
| `IDLE_TIMEOUT_SECONDS` | int | `120` | Maximum time to wait for the next request when keep-alives are enabled |

## Client IP Extraction

The service is designed to sit behind a load balancer / ingress controller. The real client IP is determined as follows:

The rate limiter middleware will look at the X-Forwarded-For header and take the last entry. Which in itself is not exactly secure. So to mitigate the harm I have another middleware infront of that called OnlyTrustedCommunication which will commpare the RemoteAddr and the url param to a whitelist of valid connections which are specified in the env vars. So by the time we get to the rate limiter we know we can trust the X-Forwarded-For header.

This prevents clients from spoofing their IP via a fake `X-Forwarded-For` header. In production, set `TRUSTED_PROXIES` to your load balancer's CIDR range (e.g. `10.0.0.0/8` for a typical cloud VPC).

## Design Decisions & Trade-offs

**httputil.ReverseProxy**
Used the stdlib `ReverseProxy` rather than hand-rolling one, since it handles far more of the HTTP spec correctly — including forwarding headers like `Authorization` without extra work. The trade-off: it forwards almost everything it's given, including redirects, without validating them — making this proxy vulnerable to SSRF (see Known Limitations).

**Rate limiting**
Uses `golang.org/x/time/rate`'s token-bucket implementation, both to avoid non-official dependencies and because it's the standard approach for this kind of limiter.

**Caching**
The caching middleware doesn't implement the full HTTP proxy spec. Since the cache exists to protect upstream APIs from excessive requests, it only respects `Cache-Control` headers on the *upstream response*, not the client request, and never caches requests that include an `Authorization` header. Supported upstream directives:
- `no-store`, `private`, `no-cache` → response is not cached
- `max-age=N` → used as TTL (overrides `CACHE_TTL_SECONDS` if smaller)

**Cache stampede prevention**
Uses `golang.org/x/sync/singleflight` for the same reason as the rate limiter — request deduplication has enough edge cases that implementing it from scratch felt out of scope for a take-home.

**Rate limiting applies to `/proxy` only**
`/health` and `/metrics` are exempt so liveness probes and Prometheus scrapes are never throttled.

**Allowlisting for security**
A middleware enforces a whitelist of trusted proxy CIDRs (`TRUSTED_PROXIES`) to safely extract the client IP: `X-Forwarded-For` is only trusted when the connecting IP matches an entry in that list; otherwise `RemoteAddr` is used. Similarly, since redirects can be abused, `ALLOWED_UPSTREAMS` restricts which hosts the service is allowed to talk to in the first place.

**Configuration**
There's no configuration validation — malformed env vars fall back to defaults where possible, but the behavior for malformed values is well \_('')_/

## Known Limitations

- **No redirect validation**: `OnlyTrustedCommunication` validates the initial URL against `ALLOWED_UPSTREAMS`, but redirect responses from upstreams are followed without checking whether the redirect target is also allowlisted. A malicious or compromised upstream could redirect to a disallowed host (SSRF). Fixing this would require a custom `http.RoundTripper` that inspects `Location` headers on 3xx responses.

## With More Time

- Reshape the API around `./server -upstream <url-encoded target URL>`, so each instance is spun up with a non-user-provided URL — safer, and able to take full advantage of the built-in `SingleHostReverseProxy`.
- Solve the redirect/SSRF problem via custom types injected into `ReverseProxy`.
- Implement a better cache eviction strategy.
- Implement graceful shutdown.
- Review the HTTP proxy spec more closely and implement more of it.
- Expand test coverage (current tests are AI-generated, with supervision).
- Deepen my own understanding of the reverse-proxy internals — the area of the code I feel least confident about.
- Consider rewriting with a framework like Chi or Gin for nicer middleware ergonomics.
- Add unit tests for the rate limiter and proxy (though there's not much to test beyond I/O).
- Improve logging.
- Harden the metrics implementation, which was largely AI-generated.
