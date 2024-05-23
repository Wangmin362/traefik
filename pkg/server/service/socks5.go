package service

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/traefik/traefik/v2/pkg/log"
	"golang.org/x/net/http/httpproxy"
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
	scheme := req.URL.Scheme
	if host == "172.30.3.224:10805" && scheme == "socks5" {
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

		url, _ := url.Parse("http://172.30.3.236:8090")
		rewriteRequestURL(req, url)
	}

	f.proxy.ServeHTTP(rw, req)
	if req.RequestURI == "/rest/wrm/2.0/resources" {
		log.FromContext(req.Context()).Debugf("%v", req.RequestURI)
	}
}

func rewriteRequestURL(req *http.Request, target *url.URL) {
	targetQuery := target.RawQuery
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	req.URL.Path, req.URL.RawPath = joinURLPath(target, req.URL)
	if targetQuery == "" || req.URL.RawQuery == "" {
		req.URL.RawQuery = targetQuery + req.URL.RawQuery
	} else {
		req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
	}
}

func joinURLPath(a, b *url.URL) (path, rawpath string) {
	if a.RawPath == "" && b.RawPath == "" {
		return singleJoiningSlash(a.Path, b.Path), ""
	}
	// Same as singleJoiningSlash, but uses EscapedPath to determine
	// whether a slash should be added
	apath := a.EscapedPath()
	bpath := b.EscapedPath()

	aslash := strings.HasSuffix(apath, "/")
	bslash := strings.HasPrefix(bpath, "/")

	switch {
	case aslash && bslash:
		return a.Path + b.Path[1:], apath + bpath[1:]
	case !aslash && !bslash:
		return a.Path + "/" + b.Path, apath + "/" + bpath
	}
	return a.Path + b.Path, apath + bpath
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}
