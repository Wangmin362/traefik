package service

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"net/url"
	"reflect"
	"time"

	"github.com/containous/alice"
	"github.com/traefik/traefik/v2/pkg/config/dynamic"
	"github.com/traefik/traefik/v2/pkg/config/runtime"
	"github.com/traefik/traefik/v2/pkg/healthcheck"
	"github.com/traefik/traefik/v2/pkg/log"
	"github.com/traefik/traefik/v2/pkg/metrics"
	"github.com/traefik/traefik/v2/pkg/middlewares/accesslog"
	"github.com/traefik/traefik/v2/pkg/middlewares/emptybackendhandler"
	metricsMiddle "github.com/traefik/traefik/v2/pkg/middlewares/metrics"
	"github.com/traefik/traefik/v2/pkg/middlewares/pipelining"
	"github.com/traefik/traefik/v2/pkg/safe"
	"github.com/traefik/traefik/v2/pkg/server/cookie"
	"github.com/traefik/traefik/v2/pkg/server/provider"
	"github.com/traefik/traefik/v2/pkg/server/service/loadbalancer/failover"
	"github.com/traefik/traefik/v2/pkg/server/service/loadbalancer/mirror"
	"github.com/traefik/traefik/v2/pkg/server/service/loadbalancer/wrr"
	"github.com/vulcand/oxy/v2/roundrobin"
	"github.com/vulcand/oxy/v2/roundrobin/stickycookie"
)

const (
	defaultHealthCheckInterval = 30 * time.Second
	defaultHealthCheckTimeout  = 5 * time.Second
)

const defaultMaxBodySize int64 = -1

// RoundTripperGetter is a roundtripper getter interface.
// 1、Q：为什么这里的响应是http.RoundTripper而不是http.Handler接口？ A：因为Traefik的主要作用是作为一个代理。并不是真实请求的服务端。
// 所以当Traefik接收到一个http请求之后，需要想办法把这个请求发送到真正的服务端；而在这个过程中，对于真实的服务端而言，Traefik是彻头彻尾的
// web客户端。所以这里使用的是http.RoundTripper而不是http.Handler接口。
type RoundTripperGetter interface {
	// Get 1、用于获取后端服务的http客户端。
	// 2、name为Traefik动态配置的后端服务名。http.RoundTripper可以理解为http客户端
	Get(name string) (http.RoundTripper, error)
}

// NewManager creates a new Manager.
func NewManager(
	configs map[string]*runtime.ServiceInfo, // 所有后端服务的配置信息
	metricsRegistry metrics.Registry,
	routinePool *safe.Pool,
	roundTripperManager RoundTripperGetter,
) *Manager {
	return &Manager{
		routinePool:         routinePool,
		metricsRegistry:     metricsRegistry,
		bufferPool:          newBufferPool(),
		roundTripperManager: roundTripperManager,
		balancers:           make(map[string]healthcheck.Balancers),
		configs:             configs,
		rand:                rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Manager The service manager.
// 1、后端服务的管理器
type Manager struct {
	routinePool         *safe.Pool       // 协程池
	metricsRegistry     metrics.Registry // 普罗米修斯指标注册中心
	bufferPool          httputil.BufferPool
	roundTripperManager RoundTripperGetter // 用于http客户但发送HTTP请求，并获取响应保温。可以简单理解为http客户端
	// balancers is the map of all Balancers, keyed by service name.
	// There is one Balancer per service handler, and there is one service handler per reference to a service
	// (e.g. if 2 routers refer to the same service name, 2 service handlers are created),
	// which is why there is not just one Balancer per service name.
	// 包装了负载均衡的后端服务, key为后端服务名字
	balancers map[string]healthcheck.Balancers
	// 后端服务配置， key为后端服务名字
	configs map[string]*runtime.ServiceInfo
	// 后端服务负载均衡随机数，不然每次都是从第一个服务轮询
	rand *rand.Rand // For the initial shuffling of load-balancers.
}

// BuildHTTP Creates a http.Handler for a service configuration.
func (m *Manager) BuildHTTP(rootCtx context.Context,
	serviceName string, // 这里的服务名其实就是路由中的Service字段，用于设置当前路由如果匹配成功之后把流量导入到哪里
) (http.Handler, error) {
	ctx := log.With(rootCtx, log.Str(log.ServiceName, serviceName))

	// 获取后端服务名，格式为<serviceName>@<ProviderName>
	serviceName = provider.GetQualifiedName(ctx, serviceName)
	// 增加一些上下文信息
	ctx = provider.AddInContext(ctx, serviceName)

	// 根据服务名获取到当前指定服务的配置
	conf, ok := m.configs[serviceName]
	if !ok {
		return nil, fmt.Errorf("the service %q does not exist", serviceName)
	}

	// 一个后端服务只能设置为LoadBalancer、Weighted、Mirroring、Failover中的一种, 不能同时设置多种
	value := reflect.ValueOf(*conf.Service)
	var count int
	for i := range value.NumField() {
		if !value.Field(i).IsNil() {
			count++
		}
	}
	if count > 1 {
		err := errors.New("cannot create service: multi-types service not supported, consider declaring two different pieces of service instead")
		conf.AddError(err, true)
		return nil, err
	}

	var lb http.Handler

	switch {
	case conf.LoadBalancer != nil:
		var err error
		lb, err = m.getLoadBalancerServiceHandler(ctx, serviceName, conf.LoadBalancer)
		if err != nil {
			conf.AddError(err, true)
			return nil, err
		}
	case conf.Weighted != nil:
		var err error
		lb, err = m.getWRRServiceHandler(ctx, serviceName, conf.Weighted)
		if err != nil {
			conf.AddError(err, true)
			return nil, err
		}
	case conf.Mirroring != nil:
		var err error
		lb, err = m.getMirrorServiceHandler(ctx, conf.Mirroring)
		if err != nil {
			conf.AddError(err, true)
			return nil, err
		}
	case conf.Failover != nil:
		var err error
		lb, err = m.getFailoverServiceHandler(ctx, serviceName, conf.Failover)
		if err != nil {
			conf.AddError(err, true)
			return nil, err
		}
	default:
		sErr := fmt.Errorf("the service %q does not have any type defined", serviceName)
		conf.AddError(sErr, true)
		return nil, sErr
	}

	return lb, nil
}

func (m *Manager) getFailoverServiceHandler(ctx context.Context, serviceName string, config *dynamic.Failover) (http.Handler, error) {
	f := failover.New(config.HealthCheck)

	serviceHandler, err := m.BuildHTTP(ctx, config.Service)
	if err != nil {
		return nil, err
	}

	f.SetHandler(serviceHandler)

	updater, ok := serviceHandler.(healthcheck.StatusUpdater)
	if !ok {
		return nil, fmt.Errorf("child service %v of %v not a healthcheck.StatusUpdater (%T)", config.Service, serviceName, serviceHandler)
	}

	if err := updater.RegisterStatusUpdater(func(up bool) {
		f.SetHandlerStatus(ctx, up)
	}); err != nil {
		return nil, fmt.Errorf("cannot register %v as updater for %v: %w", config.Service, serviceName, err)
	}

	fallbackHandler, err := m.BuildHTTP(ctx, config.Fallback)
	if err != nil {
		return nil, err
	}

	f.SetFallbackHandler(fallbackHandler)

	// Do not report the health of the fallback handler.
	if config.HealthCheck == nil {
		return f, nil
	}

	fallbackUpdater, ok := fallbackHandler.(healthcheck.StatusUpdater)
	if !ok {
		return nil, fmt.Errorf("child service %v of %v not a healthcheck.StatusUpdater (%T)", config.Fallback, serviceName, fallbackHandler)
	}

	if err := fallbackUpdater.RegisterStatusUpdater(func(up bool) {
		f.SetFallbackHandlerStatus(ctx, up)
	}); err != nil {
		return nil, fmt.Errorf("cannot register %v as updater for %v: %w", config.Fallback, serviceName, err)
	}

	return f, nil
}

func (m *Manager) getMirrorServiceHandler(ctx context.Context, config *dynamic.Mirroring) (http.Handler, error) {
	serviceHandler, err := m.BuildHTTP(ctx, config.Service)
	if err != nil {
		return nil, err
	}

	maxBodySize := defaultMaxBodySize
	if config.MaxBodySize != nil {
		maxBodySize = *config.MaxBodySize
	}
	handler := mirror.New(serviceHandler, m.routinePool, maxBodySize, config.HealthCheck)
	for _, mirrorConfig := range config.Mirrors {
		mirrorHandler, err := m.BuildHTTP(ctx, mirrorConfig.Name)
		if err != nil {
			return nil, err
		}

		err = handler.AddMirror(mirrorHandler, mirrorConfig.Percent)
		if err != nil {
			return nil, err
		}
	}
	return handler, nil
}

func (m *Manager) getWRRServiceHandler(ctx context.Context, serviceName string, config *dynamic.WeightedRoundRobin) (http.Handler, error) {
	// TODO Handle accesslog and metrics with multiple service name
	if config.Sticky != nil && config.Sticky.Cookie != nil {
		config.Sticky.Cookie.Name = cookie.GetName(config.Sticky.Cookie.Name, serviceName)
	}

	balancer := wrr.New(config.Sticky, config.HealthCheck)
	for _, service := range shuffle(config.Services, m.rand) {
		serviceHandler, err := m.BuildHTTP(ctx, service.Name)
		if err != nil {
			return nil, err
		}

		balancer.AddService(service.Name, serviceHandler, service.Weight)

		if config.HealthCheck == nil {
			continue
		}

		childName := service.Name
		updater, ok := serviceHandler.(healthcheck.StatusUpdater)
		if !ok {
			return nil, fmt.Errorf("child service %v of %v not a healthcheck.StatusUpdater (%T)", childName, serviceName, serviceHandler)
		}

		if err := updater.RegisterStatusUpdater(func(up bool) {
			balancer.SetStatus(ctx, childName, up)
		}); err != nil {
			return nil, fmt.Errorf("cannot register %v as updater for %v: %w", childName, serviceName, err)
		}

		log.FromContext(ctx).Debugf("Child service %v will update parent %v on status change", childName, serviceName)
	}

	return balancer, nil
}

func (m *Manager) getLoadBalancerServiceHandler(ctx context.Context,
	serviceName string, // 路由配置中设置的后端服务名
	service *dynamic.ServersLoadBalancer, // 后端服务配置, 包含了后端服务的配置信息
) (http.Handler, error) {
	// 如果没有设置，那么默认开启PassHostHeader
	if service.PassHostHeader == nil {
		defaultPassHostHeader := true
		service.PassHostHeader = &defaultPassHostHeader
	}

	if len(service.ServersTransport) > 0 {
		service.ServersTransport = provider.GetQualifiedName(ctx, service.ServersTransport)
	}

	// 根据服务名字，获取为该服务构建的RoundTripper
	roundTripper, err := m.roundTripperManager.Get(service.ServersTransport)
	if err != nil {
		return nil, err
	}

	// TODO 构建反向代理，处理请求转发
	// TODO 这里是怎么把流量转发出去，然后把流量io拷贝到之前的连接中的？
	fwd, err := buildProxy(service.PassHostHeader, service.ResponseForwarding, roundTripper, m.bufferPool)
	if err != nil {
		return nil, err
	}

	alHandler := func(next http.Handler) (http.Handler, error) {
		return accesslog.NewFieldHandler(next, accesslog.ServiceName, serviceName, accesslog.AddServiceFields), nil
	}
	chain := alice.New()
	if m.metricsRegistry != nil && m.metricsRegistry.IsSvcEnabled() {
		chain = chain.Append(metricsMiddle.WrapServiceHandler(ctx, m.metricsRegistry, serviceName))
	}

	handler, err := chain.Append(alHandler).Then(pipelining.New(ctx, fwd, "pipelining"))
	if err != nil {
		return nil, err
	}

	balancer, err := m.getLoadBalancer(ctx, serviceName, service, handler)
	if err != nil {
		return nil, err
	}

	// TODO rename and checks
	m.balancers[serviceName] = append(m.balancers[serviceName], balancer)

	// Empty (backend with no servers)
	return emptybackendhandler.New(balancer), nil
}

// LaunchHealthCheck launches the health checks.
func (m *Manager) LaunchHealthCheck() {
	backendConfigs := make(map[string]*healthcheck.BackendConfig)

	for serviceName, balancers := range m.balancers {
		ctx := log.With(context.Background(), log.Str(log.ServiceName, serviceName))

		service := m.configs[serviceName].LoadBalancer

		// Health Check
		hcOpts := buildHealthCheckOptions(ctx, balancers, serviceName, service.HealthCheck)
		if hcOpts == nil {
			continue
		}
		// 获取Transport
		hcOpts.Transport, _ = m.roundTripperManager.Get(service.ServersTransport)
		log.FromContext(ctx).Debugf("Setting up healthcheck for service %s with %s", serviceName, *hcOpts)

		backendConfigs[serviceName] = healthcheck.NewBackendConfig(*hcOpts, serviceName)
	}

	healthcheck.GetHealthCheck(m.metricsRegistry).SetBackendsConfiguration(context.Background(), backendConfigs)
}

func buildHealthCheckOptions(ctx context.Context, lb healthcheck.Balancer, backend string, hc *dynamic.ServerHealthCheck) *healthcheck.Options {
	if hc == nil {
		return nil
	}

	logger := log.FromContext(ctx)

	if hc.Path == "" {
		logger.Errorf("Ignoring heath check configuration for '%s': no path provided", backend)
		return nil
	}

	interval := defaultHealthCheckInterval
	if hc.Interval != "" {
		intervalOverride, err := time.ParseDuration(hc.Interval)
		switch {
		case err != nil:
			logger.Errorf("Illegal health check interval for '%s': %s", backend, err)
		case intervalOverride <= 0:
			logger.Errorf("Health check interval smaller than zero for service '%s'", backend)
		default:
			interval = intervalOverride
		}
	}

	timeout := defaultHealthCheckTimeout
	if hc.Timeout != "" {
		timeoutOverride, err := time.ParseDuration(hc.Timeout)
		switch {
		case err != nil:
			logger.Errorf("Illegal health check timeout for backend '%s': %s", backend, err)
		case timeoutOverride <= 0:
			logger.Errorf("Health check timeout smaller than zero for backend '%s', backend", backend)
		default:
			timeout = timeoutOverride
		}
	}

	followRedirects := true
	if hc.FollowRedirects != nil {
		followRedirects = *hc.FollowRedirects
	}

	return &healthcheck.Options{
		Scheme:          hc.Scheme,
		Path:            hc.Path,
		Method:          hc.Method,
		Port:            hc.Port,
		Interval:        interval,
		Timeout:         timeout,
		LB:              lb,
		Hostname:        hc.Hostname,
		Headers:         hc.Headers,
		FollowRedirects: followRedirects,
	}
}

func (m *Manager) getLoadBalancer(ctx context.Context, serviceName string, service *dynamic.ServersLoadBalancer, fwd http.Handler) (healthcheck.BalancerStatusHandler, error) {
	logger := log.FromContext(ctx)
	logger.Debug("Creating load-balancer")

	var options []roundrobin.LBOption

	var cookieName string
	if service.Sticky != nil && service.Sticky.Cookie != nil {
		cookieName = cookie.GetName(service.Sticky.Cookie.Name, serviceName)

		opts := roundrobin.CookieOptions{
			HTTPOnly: service.Sticky.Cookie.HTTPOnly,
			Secure:   service.Sticky.Cookie.Secure,
			SameSite: convertSameSite(service.Sticky.Cookie.SameSite),
		}

		// Sticky Cookie Value
		cv, err := stickycookie.NewFallbackValue(&stickycookie.RawValue{}, &stickycookie.HashValue{})
		if err != nil {
			return nil, err
		}

		options = append(options, roundrobin.EnableStickySession(roundrobin.NewStickySessionWithOptions(cookieName, opts).SetCookieValue(cv)))

		logger.Debugf("Sticky session cookie name: %v", cookieName)
	}

	lb, err := roundrobin.New(fwd, options...)
	if err != nil {
		return nil, err
	}

	lbsu := healthcheck.NewLBStatusUpdater(lb, m.configs[serviceName], service.HealthCheck)
	if err := m.upsertServers(ctx, lbsu, service.Servers); err != nil {
		return nil, fmt.Errorf("error configuring load balancer for service %s: %w", serviceName, err)
	}

	return lbsu, nil
}

func (m *Manager) upsertServers(ctx context.Context, lb healthcheck.BalancerHandler, servers []dynamic.Server) error {
	logger := log.FromContext(ctx)

	for name, srv := range shuffle(servers, m.rand) {
		u, err := url.Parse(srv.URL)
		if err != nil {
			return fmt.Errorf("error parsing server URL %s: %w", srv.URL, err)
		}

		logger.WithField(log.ServerName, name).Debugf("Creating server %d %s", name, u)

		if err := lb.UpsertServer(u, roundrobin.Weight(1)); err != nil {
			return fmt.Errorf("error adding server %s to load balancer: %w", srv.URL, err)
		}

		// TODO Handle Metrics
	}
	return nil
}

func convertSameSite(sameSite string) http.SameSite {
	switch sameSite {
	case "none":
		return http.SameSiteNoneMode
	case "lax":
		return http.SameSiteLaxMode
	case "strict":
		return http.SameSiteStrictMode
	default:
		return 0
	}
}

func shuffle[T any](values []T, r *rand.Rand) []T {
	shuffled := make([]T, len(values))
	copy(shuffled, values)
	r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	return shuffled
}
