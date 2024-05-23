package service

import (
	"golang.org/x/net/http/httpproxy"
	"net/http"
	"net/http/httputil"
	"net/url"
)

type Socks5Handler struct {
	proxy *httputil.ReverseProxy
}

// NewSocks5Handler creates a socks5 handler.
func NewSocks5Handler(proxy *httputil.ReverseProxy) http.Handler {
	return &Socks5Handler{proxy: proxy}
}

func (f *Socks5Handler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	host := req.URL.Host
	if host == "socks5://172.30.3.224:10805" {
		// 设置SOCKS代理
		tp := f.proxy.Transport.(*KerberosRoundTripper)
		smtp := tp.OriginalRoundTripper.(*smartRoundTripper)
		hpc := httpproxy.Config{
			HTTPProxy:  "socks5://172.30.3.224:10805",
			HTTPSProxy: "socks5://172.30.3.224:10805",
		}
		smtp.http.Proxy = func(r *http.Request) (*url.URL, error) {
			return hpc.ProxyFunc()(r.URL)
		}
		smtp.http2.Proxy = func(r *http.Request) (*url.URL, error) {
			return hpc.ProxyFunc()(r.URL)
		}

		req.URL.Scheme = "http"
		req.URL.Host = "172.30.3.236:8090"
	}
}
