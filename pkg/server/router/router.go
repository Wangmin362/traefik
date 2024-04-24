package router

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/containous/alice"
	"github.com/traefik/traefik/v2/pkg/config/runtime"
	"github.com/traefik/traefik/v2/pkg/log"
	"github.com/traefik/traefik/v2/pkg/metrics"
	"github.com/traefik/traefik/v2/pkg/middlewares/accesslog"
	"github.com/traefik/traefik/v2/pkg/middlewares/denyrouterrecursion"
	metricsMiddle "github.com/traefik/traefik/v2/pkg/middlewares/metrics"
	"github.com/traefik/traefik/v2/pkg/middlewares/recovery"
	"github.com/traefik/traefik/v2/pkg/middlewares/tracing"
	httpmuxer "github.com/traefik/traefik/v2/pkg/muxer/http"
	"github.com/traefik/traefik/v2/pkg/server/middleware"
	"github.com/traefik/traefik/v2/pkg/server/provider"
	"github.com/traefik/traefik/v2/pkg/tls"
)

const maxUserPriority = math.MaxInt - 1000

// 用于构造中间件调用链
type middlewareBuilder interface {
	BuildChain(ctx context.Context, names []string) *alice.Chain
}

type serviceManager interface {
	BuildHTTP(rootCtx context.Context, serviceName string) (http.Handler, error)
	LaunchHealthCheck()
}

// Manager A route/router manager.
type Manager struct {
	// 为指定路由构建的http.Handler请求处理链，key为路由名字，value为http.Handler
	routerHandlers map[string]http.Handler
	// serviceManager 用于管理Traefik中各种API，主要是Traefik内部API、Dashboard API、普罗米修斯指标 API、Ping API
	serviceManager  serviceManager
	metricsRegistry metrics.Registry
	// HTTP中间件Builder，用于根据指定的中间件名字，构建中间件链，其实就是一个http.Handler
	middlewaresBuilder middlewareBuilder
	// 插件构造器，用于构建指定的插件
	chainBuilder *middleware.ChainBuilder
	// 动态配置文件，只不过被转换为了运行时的动态配置文件
	conf       *runtime.Configuration
	tlsManager *tls.Manager
}

// NewManager creates a new Manager.
func NewManager(
	conf *runtime.Configuration, // 动态配置文件，只不过被转换为了运行时的动态配置文件
	serviceManager serviceManager, // serviceManager 用于管理Traefik中各种API，主要是Traefik内部API、Dashboard API、普罗米修斯指标 API、Ping API
	middlewaresBuilder middlewareBuilder, // HTTP中间件Builder，用于根据指定的中间件名字，构建中间件链，其实就是一个http.Handler
	chainBuilder *middleware.ChainBuilder, // 插件构造器，用于构建指定的插件
	metricsRegistry metrics.Registry, tlsManager *tls.Manager) *Manager {
	return &Manager{
		routerHandlers:     make(map[string]http.Handler),
		serviceManager:     serviceManager,
		metricsRegistry:    metricsRegistry,
		middlewaresBuilder: middlewaresBuilder,
		chainBuilder:       chainBuilder,
		conf:               conf,
		tlsManager:         tlsManager,
	}
}

func (m *Manager) getHTTPRouters(ctx context.Context, entryPoints []string, tls bool) map[string]map[string]*runtime.RouterInfo {
	if m.conf != nil {
		// 找到所有设置了当前指定入口点的路由
		return m.conf.GetRoutersByEntryPoints(ctx, entryPoints, tls)
	}

	return make(map[string]map[string]*runtime.RouterInfo)
}

// BuildHandlers Builds handler for all entry points.
// 1、为指定的入口点构建路由处理器。其实原理很简单，就是根据指定的入口点，找到所有设置了当前指定入口点的路由，然后构建路由处理器。每个路由都有可能
// 配置了中间件，我们只需要把所有的路由遍历一遍，看看当前路由有没有配置指定的中间件，如果有，就构建中间件链，然后把路由处理器和中间件链组合在一起。
// 2、Q：如果一个路由没有设置特定的入口点，这里是怎么处理的？ A：一个路由如果没有设置特定的入口点，那么所有入口点的流量都可以经过这个路由
// 3、返回值的key为入口点的名字
// 3、TODO Q:为什么这里多了一个tls参数？
func (m *Manager) BuildHandlers(rootCtx context.Context, entryPoints []string, tls bool) map[string]http.Handler {
	entryPointHandlers := make(map[string]http.Handler)

	// 1、m.getHTTPRouters 找到所有设置了当前指定入口点的路由
	// 2、Traefik在初始化动态配置的时候会给没有设置入口点的路由，默认设置为所有入口点
	for entryPointName, routers := range m.getHTTPRouters(rootCtx, entryPoints, tls) {
		ctx := log.With(rootCtx, log.Str(log.EntryPointName, entryPointName))

		// 1、把当前入口点配置的所有路由构建为一个http.Handle链，包括中间件
		// TODO 路由选择应该就是在这里面配置的
		handler, err := m.buildEntryPointHandler(ctx, routers)
		if err != nil {
			log.FromContext(ctx).Error(err)
			continue
		}

		handlerWithAccessLog, err := alice.New(func(next http.Handler) (http.Handler, error) {
			// TODO 似乎是给请求增加一些元数据
			return accesslog.NewFieldHandler(next, log.EntryPointName, entryPointName, accesslog.InitServiceFields), nil
		}).Then(handler)
		if err != nil {
			log.FromContext(ctx).Error(err)
			entryPointHandlers[entryPointName] = handler
		} else {
			entryPointHandlers[entryPointName] = handlerWithAccessLog
		}
	}

	for _, entryPointName := range entryPoints {
		ctx := log.With(rootCtx, log.Str(log.EntryPointName, entryPointName))

		handler, ok := entryPointHandlers[entryPointName]
		// 如果当前入口点没有找到任何的路由信息，直接配置为NotFoundHandler
		if !ok || handler == nil {
			handler = BuildDefaultHTTPRouter()
		}

		// 公共插件，增加日志、指标、链路追踪等功能
		handlerWithMiddlewares, err := m.chainBuilder.Build(ctx, entryPointName).Then(handler)
		if err != nil {
			log.FromContext(ctx).Error(err)
			continue
		}
		entryPointHandlers[entryPointName] = handlerWithMiddlewares
	}

	return entryPointHandlers
}

func (m *Manager) buildEntryPointHandler(ctx context.Context,
	configs map[string]*runtime.RouterInfo, // 某个入口点配置的所有路由
) (http.Handler, error) {
	muxer, err := httpmuxer.NewMuxer()
	if err != nil {
		return nil, err
	}

	// 遍历所有路由信息，当前的路由信息是给一个入口点配置的路由信息
	for routerName, routerConfig := range configs {
		ctxRouter := log.With(provider.AddInContext(ctx, routerName), log.Str(log.RouterName, routerName))
		logger := log.FromContext(ctxRouter)

		// 用户自定义的路由优先级不能超过maxUserPriority
		if routerConfig.Priority > maxUserPriority && !strings.HasSuffix(routerName, "@internal") {
			err = fmt.Errorf("the router priority %d exceeds the max user-defined priority %d", routerConfig.Priority, maxUserPriority)
			routerConfig.AddError(err, true)
			logger.Error(err)
			continue
		}

		// 为当前路由构建http.Handler，在构建的过程中肯定需要构建中间件，因为有些请求通过路由匹配之后，需要经过一系列的中间件处理
		handler, err := m.buildRouterHandler(ctxRouter, routerName, routerConfig)
		if err != nil {
			routerConfig.AddError(err, true)
			logger.Error(err)
			continue
		}

		err = muxer.AddRoute(routerConfig.Rule, routerConfig.Priority, handler)
		if err != nil {
			routerConfig.AddError(err, true)
			logger.Error(err)
			continue
		}
	}

	muxer.SortRoutes()

	chain := alice.New()
	chain = chain.Append(func(next http.Handler) (http.Handler, error) {
		return recovery.New(ctx, next)
	})

	return chain.Then(muxer)
}

func (m *Manager) buildRouterHandler(ctx context.Context,
	routerName string, // 路由名字
	routerConfig *runtime.RouterInfo, // 路由信息
) (http.Handler, error) {
	// 1、如果当前路由已经构建好了，直接返回
	// 2、路由属于动态配置的一部分，如果路由动态更新了，是需要更新路由处理链的
	if handler, ok := m.routerHandlers[routerName]; ok {
		return handler, nil
	}

	// TODO 重点关注这里TLS的处理
	if routerConfig.TLS != nil {
		// Don't build the router if the TLSOptions configuration is invalid.
		tlsOptionsName := tls.DefaultTLSConfigName
		if len(routerConfig.TLS.Options) > 0 && routerConfig.TLS.Options != tls.DefaultTLSConfigName {
			tlsOptionsName = provider.GetQualifiedName(ctx, routerConfig.TLS.Options)
		}
		// TODO 这里应该是在获取证书
		if _, err := m.tlsManager.Get(tls.DefaultTLSStoreName, tlsOptionsName); err != nil {
			return nil, fmt.Errorf("building router handler: %w", err)
		}
	}

	handler, err := m.buildHTTPHandler(ctx, routerConfig, routerName)
	if err != nil {
		return nil, err
	}

	handlerWithAccessLog, err := alice.New(func(next http.Handler) (http.Handler, error) {
		return accesslog.NewFieldHandler(next, accesslog.RouterName, routerName, nil), nil
	}).Then(handler)
	if err != nil {
		log.FromContext(ctx).Error(err)
		m.routerHandlers[routerName] = handler
	} else {
		m.routerHandlers[routerName] = handlerWithAccessLog
	}

	return m.routerHandlers[routerName], nil
}

func (m *Manager) buildHTTPHandler(ctx context.Context,
	router *runtime.RouterInfo, // 路由信息
	routerName string, // 路由名
) (http.Handler, error) {
	var qualifiedNames []string
	// 收集当前路由配置的所有中间件，后续需要给当前路由构造http.Handler处理链就需要组合中间件
	for _, name := range router.Middlewares {
		qualifiedNames = append(qualifiedNames, provider.GetQualifiedName(ctx, name))
	}
	router.Middlewares = qualifiedNames

	// 如果当前路由没有配置服务，直接返回错误。这样的路由没有任何意义，因为无法把流量导入到一个合适的服务
	if router.Service == "" {
		return nil, errors.New("the service is missing on the router")
	}

	// 真正的流量处理，这里需要把流量导入到一个合适的Service
	sHandler, err := m.serviceManager.BuildHTTP(ctx, router.Service)
	if err != nil {
		return nil, err
	}

	// 构建中间件处理链，用于在请求进入服务之前，对请求进行一些处理；也可以在响应返回之前，对响应进行一些处理
	mHandler := m.middlewaresBuilder.BuildChain(ctx, router.Middlewares)

	// 链路追踪Handler
	tHandler := func(next http.Handler) (http.Handler, error) {
		return tracing.NewForwarder(ctx, routerName, router.Service, next), nil
	}

	// 实例化一个空的处理链
	chain := alice.New()

	if m.metricsRegistry != nil && m.metricsRegistry.IsRouterEnabled() {
		// 增加对于指标的处理
		chain = chain.Append(metricsMiddle.WrapRouterHandler(ctx, m.metricsRegistry, routerName, provider.GetQualifiedName(ctx, router.Service)))
	}

	// TODO
	if router.DefaultRule {
		chain = chain.Append(denyrouterrecursion.WrapHandler(routerName))
	}

	// 指标 -> 中间件 -> 链路追踪 -> 服务
	return chain.Extend(*mHandler).Append(tHandler).Then(sHandler)
}

// BuildDefaultHTTPRouter creates a default HTTP router.
// 这里应该就是最后一个Handler
func BuildDefaultHTTPRouter() http.Handler {
	return http.NotFoundHandler()
}
