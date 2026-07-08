package proxy

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
)

type ctxKey int

const targetKey ctxKey = 0

type Proxy struct {
	httputil.ReverseProxy
}

func New() *Proxy {
	return &Proxy{
		ReverseProxy: httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				target := pr.In.Context().Value(targetKey).(*url.URL)
				// can't use setUrl because it appends /proxy to the start so I set it manually
				pr.Out.Host = target.Host
				pr.Out.URL = target
				pr.SetXForwarded()
			},
			Transport: http.DefaultTransport,
			ModifyResponse: func(resp *http.Response) error {
				resp.Header.Set("Access-Control-Allow-Origin", "*")
				return nil
			},
		},
	}
}

func (proxy *Proxy) ProxyHandlerFunc(w http.ResponseWriter, r *http.Request) {

	target, err := url.Parse(r.URL.Query().Get("url"))
	if err != nil {
		http.Error(w, "Invalid URL param", http.StatusBadRequest)
		return
	}

	ctx := context.WithValue(r.Context(), targetKey, target)
	proxy.ServeHTTP(w, r.WithContext(ctx))
}
