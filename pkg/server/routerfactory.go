package server

import (
	"context"

	"github.com/traefik/traefik/v2/pkg/config/runtime"
	"github.com/traefik/traefik/v2/pkg/config/static"
	"github.com/traefik/traefik/v2/pkg/log"
	"github.com/traefik/traefik/v2/pkg/metrics"
	"github.com/traefik/traefik/v2/pkg/server/middleware"
	tcpmiddleware "github.com/traefik/traefik/v2/pkg/server/middleware/tcp"
	"github.com/traefik/traefik/v2/pkg/server/router"
	tcprouter "github.com/traefik/traefik/v2/pkg/server/router/tcp"
	udprouter "github.com/traefik/traefik/v2/pkg/server/router/udp"
	"github.com/traefik/traefik/v2/pkg/server/service"
	"github.com/traefik/traefik/v2/pkg/server/service/tcp"
	"github.com/traefik/traefik/v2/pkg/server/service/udp"
	"github.com/traefik/traefik/v2/pkg/tls"
	udptypes "github.com/traefik/traefik/v2/pkg/udp"
)

// RouterFactory the factory of TCP/UDP routers.
// TODO 这里为什么叫做路由工厂？ 没有看到任何和路由相关的东西？
type RouterFactory struct {
	entryPointsTCP []string
	entryPointsUDP []string

	// 用于管理Traefik中各种API，主要是Traefik内部API、Dashboard API、普罗米修斯指标 API、Ping API
	managerFactory *service.ManagerFactory
	// 指标注册中心，一般就是普罗米修斯
	metricsRegistry metrics.Registry

	// 用户配置的远端插件和本地插件
	pluginBuilder middleware.PluginsBuilder

	// 每个请求的处理链，已经配置了公共的日志、链路追踪、指标中间件
	chainBuilder *middleware.ChainBuilder
	// TODO 这玩意是怎么管理TLS证书的？
	tlsManager *tls.Manager
}

// NewRouterFactory creates a new RouterFactory.
// 把入口点按照不同的协议进行归类，不是TCP入口点，就是TCP入口点
func NewRouterFactory(
	staticConfiguration static.Configuration, // 静态配置
	managerFactory *service.ManagerFactory, // 用于管理Traefik中各种API，主要是Traefik内部API、Dashboard API、普罗米修斯指标 API、Ping API
	tlsManager *tls.Manager, // TLS
	chainBuilder *middleware.ChainBuilder, // 每个请求的处理链，已经配置了公共的日志、链路追踪、指标中间件
	pluginBuilder middleware.PluginsBuilder, // 用户配置的远端插件和本地插件
	metricsRegistry metrics.Registry, // 指标注册中心，一般就是普罗米修斯
) *RouterFactory {
	var entryPointsTCP, entryPointsUDP []string
	// 遍历所有的入口点
	for name, cfg := range staticConfiguration.EntryPoints {
		// 获取入口点配置的协议
		protocol, err := cfg.GetProtocol()
		if err != nil {
			// Should never happen because Traefik should not start if protocol is invalid.
			log.WithoutContext().Errorf("Invalid protocol: %v", err)
		}

		if protocol == "udp" {
			entryPointsUDP = append(entryPointsUDP, name)
		} else { // 入口点不配置协议，默认及时TCP协议
			entryPointsTCP = append(entryPointsTCP, name)
		}
	}

	return &RouterFactory{
		entryPointsTCP:  entryPointsTCP,
		entryPointsUDP:  entryPointsUDP,
		managerFactory:  managerFactory,
		metricsRegistry: metricsRegistry,
		tlsManager:      tlsManager,
		chainBuilder:    chainBuilder,
		pluginBuilder:   pluginBuilder,
	}
}

// CreateRouters creates new TCPRouters and UDPRouters.
func (f *RouterFactory) CreateRouters(rtConf *runtime.Configuration) (map[string]*tcprouter.Router, map[string]udptypes.Handler) {
	ctx := context.Background()

	// 1、HTTP 构建Traefik的内部API处理逻辑
	// 2、serviceManager 用于管理Traefik中各种API，主要是Traefik内部API、Dashboard API、普罗米修斯指标 API、Ping API
	// 3、serviceManager本质上就是一个http.Handler，最核心的功能就是转发流量给后端服务
	serviceManager := f.managerFactory.Build(rtConf)

	// HTTP中间件Builder，用于根据指定的中间件名字，构建中间件链，其实就是一个http.Handler
	middlewaresBuilder := middleware.NewBuilder(rtConf.Middlewares, serviceManager, f.pluginBuilder)

	routerManager := router.NewManager(rtConf, serviceManager, middlewaresBuilder, f.chainBuilder, f.metricsRegistry, f.tlsManager)

	handlersNonTLS := routerManager.BuildHandlers(ctx, f.entryPointsTCP, false)
	handlersTLS := routerManager.BuildHandlers(ctx, f.entryPointsTCP, true)

	serviceManager.LaunchHealthCheck()

	// TCP
	svcTCPManager := tcp.NewManager(rtConf)

	middlewaresTCPBuilder := tcpmiddleware.NewBuilder(rtConf.TCPMiddlewares)

	rtTCPManager := tcprouter.NewManager(rtConf, svcTCPManager, middlewaresTCPBuilder, handlersNonTLS, handlersTLS, f.tlsManager)
	routersTCP := rtTCPManager.BuildHandlers(ctx, f.entryPointsTCP)

	// UDP
	svcUDPManager := udp.NewManager(rtConf)
	rtUDPManager := udprouter.NewManager(rtConf, svcUDPManager)
	routersUDP := rtUDPManager.BuildHandlers(ctx, f.entryPointsUDP)

	rtConf.PopulateUsedBy()

	return routersTCP, routersUDP
}
