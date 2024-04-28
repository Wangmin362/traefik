package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/traefik/traefik/v2/pkg/config/dynamic"
	"github.com/traefik/traefik/v2/pkg/log"
	traefiktls "github.com/traefik/traefik/v2/pkg/tls"
	"golang.org/x/net/http2"
)

type h2cTransportWrapper struct {
	*http2.Transport
}

func (t *h2cTransportWrapper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	return t.Transport.RoundTrip(req)
}

// NewRoundTripperManager creates a new RoundTripperManager.
func NewRoundTripperManager() *RoundTripperManager {
	return &RoundTripperManager{
		roundTrippers: make(map[string]http.RoundTripper),
		configs:       make(map[string]*dynamic.ServersTransport),
	}
}

// RoundTripperManager handles roundtripper for the reverse proxy.
type RoundTripperManager struct {
	rtLock sync.RWMutex // 读写锁，用于更新map
	// 不同服务的http客户端，http.RoundTripper可以理解为http客户端，只不过更加偏向底层，好处就是更加灵活。不同的服务可以配置不同的客户端行为。
	roundTrippers map[string]http.RoundTripper
	configs       map[string]*dynamic.ServersTransport // 用于配置Traefik和后端服务之前的设置
}

// Update updates the roundtrippers configurations.
func (r *RoundTripperManager) Update(
	// 用于配置Traefik和后端服务之间的设置， key为服务名，也就是真实的后端服务。value为定制的http.Transport配置。用户可以为不同的
	// 后端服务定制不同的行为。 这里说的行为，其实就是作为一个http客户端向真实服务器发送http请求时的行为，其实就是各种参数信息。
	newConfigs map[string]*dynamic.ServersTransport,
) {
	r.rtLock.Lock()
	defer r.rtLock.Unlock()

	// 1、这个for循环主要是为了解决老配置的更新和删除
	// 遍历旧的Transport配置
	for configName, config := range r.configs {
		// 看看新Transport配置中是否存在
		newConfig, ok := newConfigs[configName]
		if !ok { // 如果新Transport配置中没有这个配置了，直接删除
			delete(r.configs, configName)
			delete(r.roundTrippers, configName)
			continue
		}

		// 如果新Transport配置和旧Transport配置一样，直接忽略，啥也不需要干
		if reflect.DeepEqual(newConfig, config) {
			continue
		}

		var err error
		// TODO 根据新Transport配置创建RoundTripper，直接覆盖老的Transport配置
		r.roundTrippers[configName], err = createRoundTripper(newConfig)
		if err != nil {
			log.WithoutContext().Errorf("Could not configure HTTP Transport %s, fallback on default transport: %v", configName, err)
			r.roundTrippers[configName] = http.DefaultTransport
		}
	}

	// 1、这个for循环主要是为了解决配置的新增
	// 遍历新的配置
	for newConfigName, newConfig := range newConfigs {
		// 如果新的配置已经存在，直接忽略，应为上面肯定已经更新了
		if _, ok := r.configs[newConfigName]; ok {
			continue
		}

		var err error
		// 直接根据新配置创建RoundTripper
		r.roundTrippers[newConfigName], err = createRoundTripper(newConfig)
		if err != nil {
			log.WithoutContext().Errorf("Could not configure HTTP Transport %s, fallback on default transport: %v", newConfigName, err)
			r.roundTrippers[newConfigName] = http.DefaultTransport
		}
	}

	// 直接使用最新的Transport配置，后面后续对比，才能知道哪些配置需要删除、哪些需要更新、哪些时新增的配置
	r.configs = newConfigs
}

// Get get a roundtripper by name.
func (r *RoundTripperManager) Get(name string) (http.RoundTripper, error) {
	if len(name) == 0 {
		name = "default@internal"
	}

	r.rtLock.RLock()
	defer r.rtLock.RUnlock()

	if rt, ok := r.roundTrippers[name]; ok {
		return rt, nil
	}

	return nil, fmt.Errorf("servers transport not found %s", name)
}

// createRoundTripper creates an http.RoundTripper configured with the Transport configuration settings.
// For the settings that can't be configured in Traefik it uses the default http.Transport settings.
// An exception to this is the MaxIdleConns setting as we only provide the option MaxIdleConnsPerHost in Traefik at this point in time.
// Setting this value to the default of 100 could lead to confusing behavior and backwards compatibility issues.
func createRoundTripper(cfg *dynamic.ServersTransport /*用于配置Traefik作为一个http客户端的行为，其实就是定制http.Transport参数*/) (http.RoundTripper, error) {
	if cfg == nil {
		return nil, errors.New("no transport configuration given")
	}

	// 作为一个http客户端，肯定是需要先和真是服务之间建立TCP连接，因此需要一个Dialer拨号器，和服务方建立TCP连接，然后再发送HTTP请求。
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	// 1、转发流量的超时时间
	// 2、对于Traefik而言，这里的超时时间就是转发流量的超时时间；而对于一个Http客户端而言，其实就是发送数据的超时时间
	if cfg.ForwardingTimeouts != nil {
		dialer.Timeout = time.Duration(cfg.ForwardingTimeouts.DialTimeout)
	}

	// 1、定义一个Transport，用于完成发送http请求，并且接收响应。
	// 2、简单来说，可以直接把http.Transport理解为http客户端。http.Client的核心就是http.Transport
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment, // 这里可以支持配置HTTP代理或者HTTPS代理，也支持SOCKS代理
		DialContext:           dialer.DialContext,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ReadBufferSize:        64 * 1024,
		WriteBufferSize:       64 * 1024,
	}

	// 流量转发超时时间
	if cfg.ForwardingTimeouts != nil {
		// 等待服务端发送响应头的超时时间
		transport.ResponseHeaderTimeout = time.Duration(cfg.ForwardingTimeouts.ResponseHeaderTimeout)
		// TODO keep-alive属性，用于设置连接的保活时间，如果超过这个时间，连接会被关闭。
		transport.IdleConnTimeout = time.Duration(cfg.ForwardingTimeouts.IdleConnTimeout)
	}

	if cfg.InsecureSkipVerify || len(cfg.RootCAs) > 0 || len(cfg.ServerName) > 0 || len(cfg.Certificates) > 0 || cfg.PeerCertURI != "" {
		transport.TLSClientConfig = &tls.Config{
			ServerName:         cfg.ServerName,
			InsecureSkipVerify: cfg.InsecureSkipVerify,             // 设置客户端是否跳过检验服务端证书
			RootCAs:            createRootCACertPool(cfg.RootCAs),  // 创建一个根证书池
			Certificates:       cfg.Certificates.GetCertificates(), // 配置客户端证书，用于mTLS，也就是双向校验
		}

		if cfg.PeerCertURI != "" {
			transport.TLSClientConfig.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				return traefiktls.VerifyPeerCertificate(cfg.PeerCertURI, transport.TLSClientConfig, rawCerts)
			}
		}
	}

	// Return directly HTTP/1.1 transport when HTTP/2 is disabled
	if cfg.DisableHTTP2 {
		return &KerberosRoundTripper{
			OriginalRoundTripper: transport,
			new: func() http.RoundTripper {
				return transport.Clone()
			},
		}, nil
	}

	// TODO 这玩意是怎么实现的？
	rt, err := newSmartRoundTripper(transport, cfg.ForwardingTimeouts)
	if err != nil {
		return nil, err
	}
	return &KerberosRoundTripper{
		OriginalRoundTripper: rt,
		new: func() http.RoundTripper {
			return rt.Clone()
		},
	}, nil
}

type KerberosRoundTripper struct {
	new                  func() http.RoundTripper
	OriginalRoundTripper http.RoundTripper
}

// TODO 这玩意拿来干嘛的？
type stickyRoundTripper struct {
	RoundTripper http.RoundTripper
}

type transportKeyType string

var transportKey transportKeyType = "transport"

func AddTransportOnContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, transportKey, &stickyRoundTripper{})
}

func (k *KerberosRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	value, ok := request.Context().Value(transportKey).(*stickyRoundTripper)
	if !ok {
		return k.OriginalRoundTripper.RoundTrip(request)
	}

	if value.RoundTripper != nil {
		return value.RoundTripper.RoundTrip(request)
	}

	resp, err := k.OriginalRoundTripper.RoundTrip(request)

	// If we found that we are authenticating with Kerberos (Negotiate) or NTLM.
	// We put a dedicated roundTripper in the ConnContext.
	// This will stick the next calls to the same connection with the backend.
	if err == nil && containsNTLMorNegotiate(resp.Header.Values("WWW-Authenticate")) {
		value.RoundTripper = k.new()
	}
	return resp, err
}

func containsNTLMorNegotiate(h []string) bool {
	return slices.ContainsFunc(h, func(s string) bool {
		return strings.HasPrefix(s, "NTLM") || strings.HasPrefix(s, "Negotiate")
	})
}

func createRootCACertPool(rootCAs []traefiktls.FileOrContent) *x509.CertPool {
	if len(rootCAs) == 0 {
		return nil
	}

	roots := x509.NewCertPool()

	for _, cert := range rootCAs {
		certContent, err := cert.Read()
		if err != nil {
			log.WithoutContext().Error("Error while read RootCAs", err)
			continue
		}
		roots.AppendCertsFromPEM(certContent)
	}

	return roots
}
