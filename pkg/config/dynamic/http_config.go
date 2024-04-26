package dynamic

import (
	"reflect"
	"time"

	ptypes "github.com/traefik/paerser/types"
	traefiktls "github.com/traefik/traefik/v2/pkg/tls"
	"github.com/traefik/traefik/v2/pkg/types"
)

// +k8s:deepcopy-gen=true

// HTTPConfiguration contains all the HTTP configuration parameters.
type HTTPConfiguration struct {
	Routers     map[string]*Router     `json:"routers,omitempty" toml:"routers,omitempty" yaml:"routers,omitempty" export:"true"`
	Services    map[string]*Service    `json:"services,omitempty" toml:"services,omitempty" yaml:"services,omitempty" export:"true"`
	Middlewares map[string]*Middleware `json:"middlewares,omitempty" toml:"middlewares,omitempty" yaml:"middlewares,omitempty" export:"true"`
	// TODO 这玩意有啥用？
	Models map[string]*Model `json:"models,omitempty" toml:"models,omitempty" yaml:"models,omitempty" export:"true"`
	// 1、用于定制Traefik作为一个http客户端时向真实服务器发送请求的行为。
	// 2、起核心目的就是为了定制化http.Transport，http.Transport 抽象了一个http客户端，用于向服务端发起一个http请求。这里暴露的配置
	// 就是为了方便用户定制Traefik的行为
	// 3、TODO 这里为什么需要把服务的Transport单独拎出来？  完全可以放在Service当中
	ServersTransports map[string]*ServersTransport `json:"serversTransports,omitempty" toml:"serversTransports,omitempty" yaml:"serversTransports,omitempty" label:"-" export:"true"`
}

// +k8s:deepcopy-gen=true

// Model is a set of default router's values.
type Model struct {
	Middlewares []string         `json:"middlewares,omitempty" toml:"middlewares,omitempty" yaml:"middlewares,omitempty" export:"true"`
	TLS         *RouterTLSConfig `json:"tls,omitempty" toml:"tls,omitempty" yaml:"tls,omitempty" label:"allowEmpty" file:"allowEmpty" kv:"allowEmpty" export:"true"`
}

// +k8s:deepcopy-gen=true

// Service holds a service configuration (can only be of one type at the same time).
type Service struct {
	LoadBalancer *ServersLoadBalancer `json:"loadBalancer,omitempty" toml:"loadBalancer,omitempty" yaml:"loadBalancer,omitempty" export:"true"`
	Weighted     *WeightedRoundRobin  `json:"weighted,omitempty" toml:"weighted,omitempty" yaml:"weighted,omitempty" label:"-" export:"true"`
	Mirroring    *Mirroring           `json:"mirroring,omitempty" toml:"mirroring,omitempty" yaml:"mirroring,omitempty" label:"-" export:"true"`
	Failover     *Failover            `json:"failover,omitempty" toml:"failover,omitempty" yaml:"failover,omitempty" label:"-" export:"true"`
}

// +k8s:deepcopy-gen=true

// Router holds the router configuration.
type Router struct {
	// 声明当前路由是针对哪个入口点的路由，如果没有声明，那么当前路由将会匹配所有的入口点
	EntryPoints []string `json:"entryPoints,omitempty" toml:"entryPoints,omitempty" yaml:"entryPoints,omitempty" export:"true"`
	// 声明当前路由需要使用的中间件
	Middlewares []string `json:"middlewares,omitempty" toml:"middlewares,omitempty" yaml:"middlewares,omitempty" export:"true"`
	// 当前路由的后端服务
	Service string `json:"service,omitempty" toml:"service,omitempty" yaml:"service,omitempty" export:"true"`
	// 当前路由的匹配规则
	Rule string `json:"rule,omitempty" toml:"rule,omitempty" yaml:"rule,omitempty"`
	// 如果有多个路由同时匹配，那么可以指定优先级，优先级高的路由优先匹配
	Priority int `json:"priority,omitempty" toml:"priority,omitempty,omitzero" yaml:"priority,omitempty" export:"true"`
	// TODO 这里的TLS配置是干嘛的？
	TLS *RouterTLSConfig `json:"tls,omitempty" toml:"tls,omitempty" yaml:"tls,omitempty" label:"allowEmpty" file:"allowEmpty" kv:"allowEmpty" export:"true"`
	// TODO 默认规则是什么？用户可以配置么？
	DefaultRule bool `json:"-" toml:"-" yaml:"-" label:"-" file:"-"`
}

// +k8s:deepcopy-gen=true

// RouterTLSConfig holds the TLS configuration for a router.
type RouterTLSConfig struct {
	// TODO 这玩意应该怎么配置？
	Options      string         `json:"options,omitempty" toml:"options,omitempty" yaml:"options,omitempty" export:"true"`
	CertResolver string         `json:"certResolver,omitempty" toml:"certResolver,omitempty" yaml:"certResolver,omitempty" export:"true"`
	Domains      []types.Domain `json:"domains,omitempty" toml:"domains,omitempty" yaml:"domains,omitempty" export:"true"`
}

// +k8s:deepcopy-gen=true

// Mirroring holds the Mirroring configuration.
type Mirroring struct {
	Service     string          `json:"service,omitempty" toml:"service,omitempty" yaml:"service,omitempty" export:"true"`
	MaxBodySize *int64          `json:"maxBodySize,omitempty" toml:"maxBodySize,omitempty" yaml:"maxBodySize,omitempty" export:"true"`
	Mirrors     []MirrorService `json:"mirrors,omitempty" toml:"mirrors,omitempty" yaml:"mirrors,omitempty" export:"true"`
	HealthCheck *HealthCheck    `json:"healthCheck,omitempty" toml:"healthCheck,omitempty" yaml:"healthCheck,omitempty" label:"allowEmpty" file:"allowEmpty" kv:"allowEmpty" export:"true"`
}

// SetDefaults Default values for a WRRService.
func (m *Mirroring) SetDefaults() {
	var defaultMaxBodySize int64 = -1
	m.MaxBodySize = &defaultMaxBodySize
}

// +k8s:deepcopy-gen=true

// Failover holds the Failover configuration.
type Failover struct {
	Service     string       `json:"service,omitempty" toml:"service,omitempty" yaml:"service,omitempty" export:"true"`
	Fallback    string       `json:"fallback,omitempty" toml:"fallback,omitempty" yaml:"fallback,omitempty" export:"true"`
	HealthCheck *HealthCheck `json:"healthCheck,omitempty" toml:"healthCheck,omitempty" yaml:"healthCheck,omitempty" label:"allowEmpty" file:"allowEmpty" export:"true"`
}

// +k8s:deepcopy-gen=true

// MirrorService holds the MirrorService configuration.
type MirrorService struct {
	Name    string `json:"name,omitempty" toml:"name,omitempty" yaml:"name,omitempty" export:"true"`
	Percent int    `json:"percent,omitempty" toml:"percent,omitempty" yaml:"percent,omitempty" export:"true"`
}

// +k8s:deepcopy-gen=true

// WeightedRoundRobin is a weighted round robin load-balancer of services.
type WeightedRoundRobin struct {
	Services []WRRService `json:"services,omitempty" toml:"services,omitempty" yaml:"services,omitempty" export:"true"`
	Sticky   *Sticky      `json:"sticky,omitempty" toml:"sticky,omitempty" yaml:"sticky,omitempty" export:"true"`
	// HealthCheck enables automatic self-healthcheck for this service, i.e.
	// whenever one of its children is reported as down, this service becomes aware of it,
	// and takes it into account (i.e. it ignores the down child) when running the
	// load-balancing algorithm. In addition, if the parent of this service also has
	// HealthCheck enabled, this service reports to its parent any status change.
	HealthCheck *HealthCheck `json:"healthCheck,omitempty" toml:"healthCheck,omitempty" yaml:"healthCheck,omitempty" label:"allowEmpty" file:"allowEmpty" kv:"allowEmpty" export:"true"`
}

// +k8s:deepcopy-gen=true

// WRRService is a reference to a service load-balanced with weighted round-robin.
type WRRService struct {
	Name   string `json:"name,omitempty" toml:"name,omitempty" yaml:"name,omitempty" export:"true"`
	Weight *int   `json:"weight,omitempty" toml:"weight,omitempty" yaml:"weight,omitempty" export:"true"`
}

// SetDefaults Default values for a WRRService.
func (w *WRRService) SetDefaults() {
	defaultWeight := 1
	w.Weight = &defaultWeight
}

// +k8s:deepcopy-gen=true

// Sticky holds the sticky configuration.
type Sticky struct {
	// Cookie defines the sticky cookie configuration.
	Cookie *Cookie `json:"cookie,omitempty" toml:"cookie,omitempty" yaml:"cookie,omitempty" label:"allowEmpty" file:"allowEmpty" kv:"allowEmpty" export:"true"`
}

// +k8s:deepcopy-gen=true

// Cookie holds the sticky configuration based on cookie.
type Cookie struct {
	// Name defines the Cookie name.
	Name string `json:"name,omitempty" toml:"name,omitempty" yaml:"name,omitempty" export:"true"`
	// Secure defines whether the cookie can only be transmitted over an encrypted connection (i.e. HTTPS).
	Secure bool `json:"secure,omitempty" toml:"secure,omitempty" yaml:"secure,omitempty" export:"true"`
	// HTTPOnly defines whether the cookie can be accessed by client-side APIs, such as JavaScript.
	HTTPOnly bool `json:"httpOnly,omitempty" toml:"httpOnly,omitempty" yaml:"httpOnly,omitempty" export:"true"`
	// SameSite defines the same site policy.
	// More info: https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Set-Cookie/SameSite
	SameSite string `json:"sameSite,omitempty" toml:"sameSite,omitempty" yaml:"sameSite,omitempty" export:"true"`
}

// +k8s:deepcopy-gen=true

// ServersLoadBalancer holds the ServersLoadBalancer configuration.
type ServersLoadBalancer struct {
	// TODO Sticky是用来干嘛的？
	Sticky  *Sticky  `json:"sticky,omitempty" toml:"sticky,omitempty" yaml:"sticky,omitempty" label:"allowEmpty" file:"allowEmpty" kv:"allowEmpty" export:"true"`
	Servers []Server `json:"servers,omitempty" toml:"servers,omitempty" yaml:"servers,omitempty" label-slice-as-struct:"server" export:"true"`
	// HealthCheck enables regular active checks of the responsiveness of the
	// children servers of this load-balancer. To propagate status changes (e.g. all
	// servers of this service are down) upwards, HealthCheck must also be enabled on
	// the parent(s) of this service.
	// 后端服务的健康检测配置
	HealthCheck        *ServerHealthCheck  `json:"healthCheck,omitempty" toml:"healthCheck,omitempty" yaml:"healthCheck,omitempty" export:"true"`
	PassHostHeader     *bool               `json:"passHostHeader" toml:"passHostHeader" yaml:"passHostHeader" export:"true"`
	ResponseForwarding *ResponseForwarding `json:"responseForwarding,omitempty" toml:"responseForwarding,omitempty" yaml:"responseForwarding,omitempty" export:"true"`
	// TODO 这玩意用来干嘛的？
	ServersTransport string `json:"serversTransport,omitempty" toml:"serversTransport,omitempty" yaml:"serversTransport,omitempty" export:"true"`
}

// Mergeable tells if the given service is mergeable.
func (l *ServersLoadBalancer) Mergeable(loadBalancer *ServersLoadBalancer) bool {
	savedServers := l.Servers
	defer func() {
		l.Servers = savedServers
	}()
	l.Servers = nil

	savedServersLB := loadBalancer.Servers
	defer func() {
		loadBalancer.Servers = savedServersLB
	}()
	loadBalancer.Servers = nil

	return reflect.DeepEqual(l, loadBalancer)
}

// SetDefaults Default values for a ServersLoadBalancer.
func (l *ServersLoadBalancer) SetDefaults() {
	defaultPassHostHeader := true
	l.PassHostHeader = &defaultPassHostHeader
}

// +k8s:deepcopy-gen=true

// ResponseForwarding holds the response forwarding configuration.
type ResponseForwarding struct {
	// FlushInterval defines the interval, in milliseconds, in between flushes to the client while copying the response body.
	// A negative value means to flush immediately after each write to the client.
	// This configuration is ignored when ReverseProxy recognizes a response as a streaming response;
	// for such responses, writes are flushed to the client immediately.
	// Default: 100ms
	FlushInterval string `json:"flushInterval,omitempty" toml:"flushInterval,omitempty" yaml:"flushInterval,omitempty" export:"true"`
}

// +k8s:deepcopy-gen=true

// Server holds the server configuration.
type Server struct {
	URL string `json:"url,omitempty" toml:"url,omitempty" yaml:"url,omitempty" label:"-"`
	// TODO 这种全是中划线的字段是什么意思？ 用户可不可以设置这些字段？
	Scheme string `json:"-" toml:"-" yaml:"-" file:"-"`
	Port   string `json:"-" toml:"-" yaml:"-" file:"-"`
}

// SetDefaults Default values for a Server.
func (s *Server) SetDefaults() {
	s.Scheme = "http"
}

// +k8s:deepcopy-gen=true

// ServerHealthCheck holds the HealthCheck configuration.
type ServerHealthCheck struct {
	Scheme string `json:"scheme,omitempty" toml:"scheme,omitempty" yaml:"scheme,omitempty" export:"true"`
	Path   string `json:"path,omitempty" toml:"path,omitempty" yaml:"path,omitempty" export:"true"`
	Method string `json:"method,omitempty" toml:"method,omitempty" yaml:"method,omitempty" export:"true"`
	Port   int    `json:"port,omitempty" toml:"port,omitempty,omitzero" yaml:"port,omitempty" export:"true"`
	// TODO change string to ptypes.Duration
	Interval string `json:"interval,omitempty" toml:"interval,omitempty" yaml:"interval,omitempty" export:"true"`
	// TODO change string to ptypes.Duration
	Timeout         string            `json:"timeout,omitempty" toml:"timeout,omitempty" yaml:"timeout,omitempty" export:"true"`
	Hostname        string            `json:"hostname,omitempty" toml:"hostname,omitempty" yaml:"hostname,omitempty"`
	FollowRedirects *bool             `json:"followRedirects" toml:"followRedirects" yaml:"followRedirects" export:"true"`
	Headers         map[string]string `json:"headers,omitempty" toml:"headers,omitempty" yaml:"headers,omitempty" export:"true"`
}

// SetDefaults Default values for a HealthCheck.
func (h *ServerHealthCheck) SetDefaults() {
	fr := true
	h.FollowRedirects = &fr
}

// +k8s:deepcopy-gen=true

// HealthCheck controls healthcheck awareness and propagation at the services level.
type HealthCheck struct{}

// +k8s:deepcopy-gen=true

// ServersTransport options to configure communication between Traefik and the servers.
// 1、之所以称为ServersTransport，其实这里是根据Traefik作为七层代理角色命名的。Traefik并不生产请求，仅仅是http请求的搬运工。Traefik收到
// 请求之后就是玩了命的找到真正的服务端，然后和这个服务端建立TCP连接，发出HTTP请求。在这个过程中，Traefik扮演的是http客户端的工作。在golang
// 中，http客户端的核心抽象就是http.RoundTripper，用于完成一次http往返请求，在HTTP规范当中，这次http往返请求被称之为事务。而http.RoundTripper
// 的默认实现就是http.Transport，这里的ServersTransport其实就是想把一些必要的配置开放给用户，让用户可以定制Traefik作为一个代理向真实
// 服务端发送Http请求的行为。
type ServersTransport struct {
	// 服务名，用于抽象一个后端服务
	ServerName string `description:"ServerName used to contact the server." json:"serverName,omitempty" toml:"serverName,omitempty" yaml:"serverName,omitempty"`
	// 设置Traefik不校验服务的证书
	InsecureSkipVerify bool `description:"Disable SSL certificate verification." json:"insecureSkipVerify,omitempty" toml:"insecureSkipVerify,omitempty" yaml:"insecureSkipVerify,omitempty" export:"true"`
	// 后端服务极有可能是自签证书，因此需要把自签证书的根证书放进来
	RootCAs []traefiktls.FileOrContent `description:"Add cert file for self-signed certificate." json:"rootCAs,omitempty" toml:"rootCAs,omitempty" yaml:"rootCAs,omitempty"`
	// 有可能后端服务需要校验客户端证书，也就是说当前服务启用了双向认证，此时就需要给Traefik配置客户端证书
	Certificates traefiktls.Certificates `description:"Certificates for mTLS." json:"certificates,omitempty" toml:"certificates,omitempty" yaml:"certificates,omitempty" export:"true"`
	// TODO 每个域名可以保留的最大空闲连接数？
	MaxIdleConnsPerHost int `description:"If non-zero, controls the maximum idle (keep-alive) to keep per-host. If zero, DefaultMaxIdleConnsPerHost is used" json:"maxIdleConnsPerHost,omitempty" toml:"maxIdleConnsPerHost,omitempty" yaml:"maxIdleConnsPerHost,omitempty" export:"true"`
	// 流量转发超时时间
	ForwardingTimeouts *ForwardingTimeouts `description:"Timeouts for requests forwarded to the backend servers." json:"forwardingTimeouts,omitempty" toml:"forwardingTimeouts,omitempty" yaml:"forwardingTimeouts,omitempty" export:"true"`
	// 是否禁用HTTP2
	DisableHTTP2 bool `description:"Disable HTTP/2 for connections with backend servers." json:"disableHTTP2,omitempty" toml:"disableHTTP2,omitempty" yaml:"disableHTTP2,omitempty" export:"true"`
	// TODO 这玩意干嘛的？
	PeerCertURI string `description:"URI used to match against SAN URI during the peer certificate verification." json:"peerCertURI,omitempty" toml:"peerCertURI,omitempty" yaml:"peerCertURI,omitempty" export:"true"`
}

// +k8s:deepcopy-gen=true

// ForwardingTimeouts contains timeout configurations for forwarding requests to the backend servers.
type ForwardingTimeouts struct {
	DialTimeout           ptypes.Duration `description:"The amount of time to wait until a connection to a backend server can be established. If zero, no timeout exists." json:"dialTimeout,omitempty" toml:"dialTimeout,omitempty" yaml:"dialTimeout,omitempty" export:"true"`
	ResponseHeaderTimeout ptypes.Duration `description:"The amount of time to wait for a server's response headers after fully writing the request (including its body, if any). If zero, no timeout exists." json:"responseHeaderTimeout,omitempty" toml:"responseHeaderTimeout,omitempty" yaml:"responseHeaderTimeout,omitempty" export:"true"`
	IdleConnTimeout       ptypes.Duration `description:"The maximum period for which an idle HTTP keep-alive connection will remain open before closing itself." json:"idleConnTimeout,omitempty" toml:"idleConnTimeout,omitempty" yaml:"idleConnTimeout,omitempty" export:"true"`
	ReadIdleTimeout       ptypes.Duration `description:"The timeout after which a health check using ping frame will be carried out if no frame is received on the HTTP/2 connection. If zero, no health check is performed." json:"readIdleTimeout,omitempty" toml:"readIdleTimeout,omitempty" yaml:"readIdleTimeout,omitempty" export:"true"`
	PingTimeout           ptypes.Duration `description:"The timeout after which the HTTP/2 connection will be closed if a response to ping is not received." json:"pingTimeout,omitempty" toml:"pingTimeout,omitempty" yaml:"pingTimeout,omitempty" export:"true"`
}

// SetDefaults sets the default values.
func (f *ForwardingTimeouts) SetDefaults() {
	f.DialTimeout = ptypes.Duration(30 * time.Second)
	f.IdleConnTimeout = ptypes.Duration(90 * time.Second)
	f.PingTimeout = ptypes.Duration(15 * time.Second)
}
