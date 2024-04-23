package service

import (
	"net/http"

	"github.com/gorilla/mux"
	"github.com/traefik/traefik/v2/pkg/api"
	"github.com/traefik/traefik/v2/pkg/api/dashboard"
	"github.com/traefik/traefik/v2/pkg/config/runtime"
	"github.com/traefik/traefik/v2/pkg/config/static"
	"github.com/traefik/traefik/v2/pkg/metrics"
	"github.com/traefik/traefik/v2/pkg/safe"
)

// ManagerFactory a factory of service manager.
type ManagerFactory struct {
	metricsRegistry metrics.Registry

	roundTripperManager *RoundTripperManager

	// 只要启用了Dashboard，那么就会初始化这个属性
	api              func(configuration *runtime.Configuration) http.Handler
	restHandler      http.Handler
	dashboardHandler http.Handler
	metricsHandler   http.Handler
	pingHandler      http.Handler
	acmeHTTPHandler  http.Handler

	routinesPool *safe.Pool
}

// NewManagerFactory creates a new ManagerFactory.
// 用于管理Traefik中API
func NewManagerFactory(
	staticConfiguration static.Configuration, // 静态配置
	routinesPool *safe.Pool, // 协程池
	metricsRegistry metrics.Registry, // 指标测量
	roundTripperManager *RoundTripperManager, // 用于管理Traefik和后端服务之间的连接处理
	acmeHTTPHandler http.Handler, // 处理ACME流量
) *ManagerFactory {
	factory := &ManagerFactory{
		metricsRegistry:     metricsRegistry,
		routinesPool:        routinesPool,
		roundTripperManager: roundTripperManager,
		acmeHTTPHandler:     acmeHTTPHandler,
	}

	// 启用Traefik内部API
	if staticConfiguration.API != nil {
		// 用于启用Traefik内部的API
		apiRouterBuilder := api.NewBuilder(staticConfiguration)

		// 如果启用了Dashboard，那么保留Dashboard API
		if staticConfiguration.API.Dashboard {
			factory.dashboardHandler = dashboard.Handler{}
			factory.api = func(configuration *runtime.Configuration) http.Handler {
				router := apiRouterBuilder(configuration).(*mux.Router)
				dashboard.Append(router, nil)
				return router
			}
		} else {
			factory.api = apiRouterBuilder
		}
	}

	// 用于启用/api/providers/{provider}
	if staticConfiguration.Providers != nil && staticConfiguration.Providers.Rest != nil {
		factory.restHandler = staticConfiguration.Providers.Rest.CreateRouter()
	}

	// 用于启用指标API
	if staticConfiguration.Metrics != nil && staticConfiguration.Metrics.Prometheus != nil {
		factory.metricsHandler = metrics.PrometheusHandler()
	}

	// This check is necessary because even when staticConfiguration.Ping == nil ,
	// the affectation would make factory.pingHandle become a typed nil, which does not pass the nil test,
	// and would break things elsewhere.
	// 用于启用Ping API
	if staticConfiguration.Ping != nil {
		factory.pingHandler = staticConfiguration.Ping
	}

	return factory
}

// Build creates a service manager.
func (f *ManagerFactory) Build(configuration *runtime.Configuration) *InternalHandlers {
	svcManager := NewManager(configuration.Services, f.metricsRegistry, f.routinesPool, f.roundTripperManager)

	var apiHandler http.Handler
	if f.api != nil { // 只要启用了Dashboard，那么就会初始化这个属性
		apiHandler = f.api(configuration)
	}

	return NewInternalHandlers(svcManager, apiHandler, f.restHandler, f.metricsHandler, f.pingHandler, f.dashboardHandler, f.acmeHTTPHandler)
}
