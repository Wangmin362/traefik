package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	stdlog "log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/daemon"
	"github.com/go-acme/lego/v4/challenge"
	gokitmetrics "github.com/go-kit/kit/metrics"
	"github.com/sirupsen/logrus"
	"github.com/traefik/paerser/cli"
	"github.com/traefik/traefik/v2/cmd"
	"github.com/traefik/traefik/v2/cmd/healthcheck"
	cmdVersion "github.com/traefik/traefik/v2/cmd/version"
	tcli "github.com/traefik/traefik/v2/pkg/cli"
	"github.com/traefik/traefik/v2/pkg/collector"
	"github.com/traefik/traefik/v2/pkg/config/dynamic"
	"github.com/traefik/traefik/v2/pkg/config/runtime"
	"github.com/traefik/traefik/v2/pkg/config/static"
	"github.com/traefik/traefik/v2/pkg/log"
	"github.com/traefik/traefik/v2/pkg/metrics"
	"github.com/traefik/traefik/v2/pkg/middlewares/accesslog"
	"github.com/traefik/traefik/v2/pkg/provider/acme"
	"github.com/traefik/traefik/v2/pkg/provider/aggregator"
	"github.com/traefik/traefik/v2/pkg/provider/traefik"
	"github.com/traefik/traefik/v2/pkg/safe"
	"github.com/traefik/traefik/v2/pkg/server"
	"github.com/traefik/traefik/v2/pkg/server/middleware"
	"github.com/traefik/traefik/v2/pkg/server/service"
	traefiktls "github.com/traefik/traefik/v2/pkg/tls"
	"github.com/traefik/traefik/v2/pkg/tracing"
	"github.com/traefik/traefik/v2/pkg/tracing/jaeger"
	"github.com/traefik/traefik/v2/pkg/types"
	"github.com/traefik/traefik/v2/pkg/version"
	"github.com/vulcand/oxy/v2/roundrobin"
)

// 启动参数命令 --configFile=cmd/traefik/config/static.yml
func main() {
	// traefik config inits 初始化Traefik配置
	tConfig := cmd.NewTraefikConfiguration()

	loaders := []cli.ResourceLoader{&tcli.FileLoader{}, &tcli.FlagLoader{}, &tcli.EnvLoader{}}

	cmdTraefik := &cli.Command{
		Name: "traefik",
		Description: `Traefik is a modern HTTP reverse proxy and load balancer made to deploy microservices with ease.
Complete documentation is available at https://traefik.io`,
		Configuration: tConfig,
		Resources:     loaders,
		// TODO 启动Traefik服务
		Run: func(_ []string) error {
			return runCmd(&tConfig.Configuration)
		},
	}

	// 增加healthcheck命令
	err := cmdTraefik.AddCommand(healthcheck.NewCmd(&tConfig.Configuration, loaders))
	if err != nil {
		stdlog.Println(err)
		os.Exit(1)
	}

	// 增加version子命令
	err = cmdTraefik.AddCommand(cmdVersion.NewCmd())
	if err != nil {
		stdlog.Println(err)
		os.Exit(1)
	}

	err = cli.Execute(cmdTraefik)
	if err != nil {
		stdlog.Println(err)
		logrus.Exit(1)
	}

	logrus.Exit(0)
}

func runCmd(
	staticConfiguration *static.Configuration, // 静态配置
) error {
	// TODO 日志
	configureLogging(staticConfiguration)

	// TODO 这玩意有啥用？
	http.DefaultTransport.(*http.Transport).Proxy = http.ProxyFromEnvironment

	// 设置默认的负载均衡策略权重
	if err := roundrobin.SetDefaultWeight(0); err != nil {
		log.WithoutContext().Errorf("Could not set round robin default weight: %v", err)
	}

	// 静态配置初始化，设置一些默认的值
	staticConfiguration.SetEffectiveConfiguration()
	if err := staticConfiguration.ValidateConfiguration(); err != nil {
		return err
	}

	log.WithoutContext().Infof("Traefik version %s built on %s", version.Version, version.BuildDate)

	// 序列化静态配置
	jsonConf, err := json.Marshal(staticConfiguration)
	if err != nil {
		log.WithoutContext().Errorf("Could not marshal static configuration: %v", err)
		log.WithoutContext().Debugf("Static configuration loaded [struct] %#v", staticConfiguration)
	} else {
		log.WithoutContext().Debugf("Static configuration loaded %s", string(jsonConf))
	}

	// TODO 怎么检查版本的
	if staticConfiguration.Global.CheckNewVersion {
		checkNewVersion()
	}

	// 给Traefik发送一些统计数据，默认是禁用的
	stats(staticConfiguration)

	// TODO 初始化TraefikServer
	svr, err := setupServer(staticConfiguration)
	if err != nil {
		return err
	}

	// 接收SIGINT, SIGTERM信号
	ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	if staticConfiguration.Ping != nil {
		staticConfiguration.Ping.WithContext(ctx)
	}

	// 启动Traefik Server
	svr.Start(ctx)
	defer svr.Close()

	// TODO go 操作Systemd
	sent, err := daemon.SdNotify(false, "READY=1")
	if !sent && err != nil {
		log.WithoutContext().Errorf("Failed to notify: %v", err)
	}

	// systemD相关的东西
	t, err := daemon.SdWatchdogEnabled(false)
	if err != nil {
		log.WithoutContext().Errorf("Could not enable Watchdog: %v", err)
	} else if t != 0 {
		// Send a ping each half time given
		t /= 2
		log.WithoutContext().Infof("Watchdog activated with timer duration %s", t)
		safe.Go(func() {
			tick := time.Tick(t)
			for range tick {
				resp, errHealthCheck := healthcheck.Do(*staticConfiguration)
				if resp != nil {
					_ = resp.Body.Close()
				}

				if staticConfiguration.Ping == nil || errHealthCheck == nil {
					if ok, _ := daemon.SdNotify(false, "WATCHDOG=1"); !ok {
						log.WithoutContext().Error("Fail to tick watchdog")
					}
				} else {
					log.WithoutContext().Error(errHealthCheck)
				}
			}
		})
	}

	// 等待Traefik Server结束
	svr.Wait()
	log.WithoutContext().Info("Shutting down")
	return nil
}

func setupServer(staticConfiguration *static.Configuration) (*server.Server, error) {
	// 1、把所有的Provider集中到一起，也就是providerAggregator;
	// 2、Provider可以理解为不同的平台，比如K8S，Docker，File等
	// 3、Provider是Traefik中非常重要的一个概念，其核心作用其实就是为了给Traefik提供动态篇配置。也就说“服务发现”概念
	providerAggregator := aggregator.NewProviderAggregator(*staticConfiguration.Providers)

	ctx := context.Background()
	routinesPool := safe.NewPool(ctx) // 实例化协程池

	// adds internal provider  这个Provider用于暴露Traefik API
	err := providerAggregator.AddProvider(traefik.New(*staticConfiguration))
	if err != nil {
		return nil, err
	}

	// ACME

	// TODO TLSManager是如何管理证书的？当客户端根据不同的域名请求不同的服务时，TLSManger时如何返回给客户端对应的证书的？
	tlsManager := traefiktls.NewManager()
	// httpChallenge是用于是一种通过 HTTP 协议进行的验证方法，常用于自动化的安全验证过程中，如域名所有权验证或自动化证书管理
	// 例如使用 Let's Encrypt 进行 SSL/TLS 证书的自动化颁发和续期）。它的基本原理是证明请求方（通常是服务器或其代理）对特定域名的控制权。
	httpChallengeProvider := acme.NewChallengeHTTP()

	// TODO ALPN即应用层协议协商（Application Layer Protocol Negotiation），用于客户端和服务端之间进行TLS握手的时候客户端可以声明
	// 自己需要使用的应用层协议，譬如HTTP/2, HTTP/1.1, SPDY等等
	tlsChallengeProvider := acme.NewChallengeTLSALPN()
	err = providerAggregator.AddProvider(tlsChallengeProvider)
	if err != nil {
		return nil, err
	}

	// TODO ACME用于自动更新证书。 为什么在Traefik的设计中， ACME也是一个Provider
	acmeProviders := initACMEProvider(staticConfiguration, &providerAggregator, tlsManager, httpChallengeProvider, tlsChallengeProvider)

	// Entrypoints

	// TODO TCP EntryPoint实现，Traefik会监听配置的端点
	serverEntryPointsTCP, err := server.NewTCPEntryPoints(staticConfiguration.EntryPoints, staticConfiguration.HostResolver)
	if err != nil {
		return nil, err
	}

	// TODO UDP EntryPoint实现
	serverEntryPointsUDP, err := server.NewUDPEntryPoints(staticConfiguration.EntryPoints)
	if err != nil {
		return nil, err
	}

	if staticConfiguration.Pilot != nil {
		log.WithoutContext().Warn("Traefik Pilot has been removed.")
	}

	// 启用Traefik的API
	if staticConfiguration.API != nil {
		version.DisableDashboardAd = staticConfiguration.API.DisableDashboardAd
	}

	// Plugins

	// TODO 插件原理
	// 1、加载本地插件和远端插件到内存中
	// 2、远端插件指的是Traefik插件仓库中的插件
	// 3、本地插件指的是用户自定义的插件
	// 4、加载插件的时候，会根据插件的配置信息，实例化插件
	// 5、Traefik的插件支持Provider和Middleware两种类型
	// TODO Traefik本身自带的插件在哪里加载？
	pluginBuilder, err := createPluginBuilder(staticConfiguration)
	if err != nil {
		log.WithoutContext().WithError(err).Error("Plugins are disabled because an error has occurred.")
	}

	// Providers plugins

	// TODO 加载Provider插件
	for name, conf := range staticConfiguration.Providers.Plugin {
		if pluginBuilder == nil {
			break
		}

		p, err := pluginBuilder.BuildProvider(name, conf)
		if err != nil {
			return nil, fmt.Errorf("plugin: failed to build provider: %w", err)
		}

		err = providerAggregator.AddProvider(p)
		if err != nil {
			return nil, fmt.Errorf("plugin: failed to add provider: %w", err)
		}
	}

	// Metrics

	metricRegistries := registerMetricClients(staticConfiguration.Metrics)
	metricsRegistry := metrics.NewMultiRegistry(metricRegistries)

	// Service manager factory

	// RoundTripper用于抽象对于HTTP请求的处理，Traefik实现了smartRoundTripper用于处理HTTP, HTTP/2流量
	// TODO Traefik是怎么实现的这个RoundTripper的？
	roundTripperManager := service.NewRoundTripperManager()
	// 从ACMEProvider中获取ACME Handler
	acmeHTTPHandler := getHTTPChallengeHandler(acmeProviders, httpChallengeProvider)
	// 1、用于管理Traefik中各种API，主要是Traefik内部API、Dashboard API、普罗米修斯指标 API、Ping API
	// 2、最最核心的其实ManagerFactory会管理客户端到服务端之间的请求，也就是说Traefik转发流量就是在ManagerFactory中完成的
	// TODO 继续分析
	managerFactory := service.NewManagerFactory(*staticConfiguration, routinesPool, metricsRegistry, roundTripperManager, acmeHTTPHandler)

	// Router factory

	accessLog := setupAccessLog(staticConfiguration.AccessLog)
	// TODO 配置链路追踪相关
	tracer := setupTracing(staticConfiguration.Tracing)

	// 1、构建指定入口点的请求链，增加通用的日志、链路追踪、指标中间件
	// 2、这里为入口点添加的中间件都是公共的，所以是通用的方法
	chainBuilder := middleware.NewChainBuilder(metricsRegistry, accessLog, tracer)
	// 1、把入口点按照不同的协议进行归类，不是TCP入口点，就是TCP入口点
	// 2、这里仅仅涉及到入口点的分类，并没有涉及到路由的组装
	routerFactory := server.NewRouterFactory(
		*staticConfiguration, // 静态配置
		managerFactory,       // 用于管理Traefik中各种API，主要是Traefik内部API、Dashboard API、普罗米修斯指标 API、Ping API。其中也包括对于真实流量的转发
		tlsManager,           // TLS
		chainBuilder,         // 每个请求的处理链，已经配置了公共的日志、链路追踪、指标中间件
		pluginBuilder,        // 用户配置的远端插件和本地插件
		metricsRegistry,      // 指标注册中心，一般就是普罗米修斯
	)

	// Watcher

	// TODO 配置动态发现
	watcher := server.NewConfigurationWatcher(
		routinesPool,
		providerAggregator,
		getDefaultsEntrypoints(staticConfiguration), // TODO 什么叫做默认入口点？ 有啥用？
		"internal",
	)

	// TLS 动态更新证书，不同的路由可能需要配置不同的证书，因此Traefik需要支持证书的动态更新
	watcher.AddListener(func(conf dynamic.Configuration) {
		ctx := context.Background()
		// 更新各个路由需要使用的证书
		tlsManager.UpdateConfigs(ctx, conf.TLS.Stores, conf.TLS.Options, conf.TLS.Certificates)

		// 增加指标相关的东西
		gauge := metricsRegistry.TLSCertsNotAfterTimestampGauge()
		for _, certificate := range tlsManager.GetServerCertificates() {
			appendCertMetric(gauge, certificate)
		}
	})

	// Metrics
	watcher.AddListener(func(_ dynamic.Configuration) {
		metricsRegistry.ConfigReloadsCounter().Add(1)
		metricsRegistry.LastConfigReloadSuccessGauge().Set(float64(time.Now().Unix()))
	})

	// Server Transports
	// TODO 似乎是在动态配置HTTPServerTransport
	watcher.AddListener(func(conf dynamic.Configuration) {
		roundTripperManager.Update(conf.HTTP.ServersTransports)
	})

	// Switch router
	// TODO 重点分析
	watcher.AddListener(switchRouter(routerFactory, serverEntryPointsTCP, serverEntryPointsUDP))

	// Metrics
	if metricsRegistry.IsEpEnabled() || metricsRegistry.IsRouterEnabled() || metricsRegistry.IsSvcEnabled() {
		var eps []string
		for key := range serverEntryPointsTCP {
			eps = append(eps, key)
		}
		watcher.AddListener(func(conf dynamic.Configuration) {
			metrics.OnConfigurationUpdate(conf, eps)
		})
	}

	// TLS challenge ACME证书自动更新相关的
	watcher.AddListener(tlsChallengeProvider.ListenConfiguration)

	// ACME
	resolverNames := map[string]struct{}{}
	for _, p := range acmeProviders {
		resolverNames[p.ResolverName] = struct{}{}
		watcher.AddListener(p.ListenConfiguration)
	}

	// Certificate resolver logs
	watcher.AddListener(func(config dynamic.Configuration) {
		for rtName, rt := range config.HTTP.Routers {
			if rt.TLS == nil || rt.TLS.CertResolver == "" {
				continue
			}

			if _, ok := resolverNames[rt.TLS.CertResolver]; !ok {
				log.WithoutContext().Errorf("the router %s uses a non-existent resolver: %s", rtName, rt.TLS.CertResolver)
			}
		}
	})

	return server.NewServer(routinesPool, serverEntryPointsTCP, serverEntryPointsUDP, watcher, chainBuilder, accessLog), nil
}

func getHTTPChallengeHandler(acmeProviders []*acme.Provider, httpChallengeProvider http.Handler) http.Handler {
	var acmeHTTPHandler http.Handler
	for _, p := range acmeProviders {
		if p != nil && p.HTTPChallenge != nil {
			acmeHTTPHandler = httpChallengeProvider
			break
		}
	}
	return acmeHTTPHandler
}

func getDefaultsEntrypoints(staticConfiguration *static.Configuration) []string {
	// TODO 什么叫做默认入口点？ 有啥用？
	var defaultEntryPoints []string
	for name, cfg := range staticConfiguration.EntryPoints {
		protocol, err := cfg.GetProtocol()
		if err != nil {
			// Should never happen because Traefik should not start if protocol is invalid.
			log.WithoutContext().Errorf("Invalid protocol: %v", err)
		}

		if protocol != "udp" && name != static.DefaultInternalEntryPointName {
			defaultEntryPoints = append(defaultEntryPoints, name)
		}
	}

	sort.Strings(defaultEntryPoints)
	return defaultEntryPoints
}

func switchRouter(
	routerFactory *server.RouterFactory, // TODO 路由工厂
	serverEntryPointsTCP server.TCPEntryPoints, // TCP入口点
	serverEntryPointsUDP server.UDPEntryPoints, // UDP入口点
) func(conf dynamic.Configuration) {
	return func(conf dynamic.Configuration) {
		// 1、当动态配置发生变动的时候，就会调用到这里，conf为动态配置
		// 2、runtime.NewConfig(conf)用于把动态配置转为运行时配置，实际上就是重新组织了一遍数据结构  TODO 思考这样坐的好处，为什么不直接使用动态配置？
		rtConf := runtime.NewConfig(conf)

		// 1、routers为TCP路由
		// 2、udpRouters为UDP路由
		routers, udpRouters := routerFactory.CreateRouters(rtConf)

		serverEntryPointsTCP.Switch(routers)
		serverEntryPointsUDP.Switch(udpRouters)
	}
}

// initACMEProvider creates an acme provider from the ACME part of globalConfiguration.
func initACMEProvider(
	c *static.Configuration,
	providerAggregator *aggregator.ProviderAggregator,
	tlsManager *traefiktls.Manager,
	httpChallengeProvider,
	tlsChallengeProvider challenge.Provider,
) []*acme.Provider {
	localStores := map[string]*acme.LocalStore{}

	var resolvers []*acme.Provider
	for name, resolver := range c.CertificatesResolvers {
		if resolver.ACME == nil {
			continue
		}

		if localStores[resolver.ACME.Storage] == nil {
			localStores[resolver.ACME.Storage] = acme.NewLocalStore(resolver.ACME.Storage)
		}

		p := &acme.Provider{
			Configuration:         resolver.ACME,
			Store:                 localStores[resolver.ACME.Storage],
			ResolverName:          name,
			HTTPChallengeProvider: httpChallengeProvider,
			TLSChallengeProvider:  tlsChallengeProvider,
		}

		if err := providerAggregator.AddProvider(p); err != nil {
			log.WithoutContext().Errorf("The ACME resolver %q is skipped from the resolvers list because: %v", name, err)
			continue
		}

		p.SetTLSManager(tlsManager)

		p.SetConfigListenerChan(make(chan dynamic.Configuration))

		resolvers = append(resolvers, p)
	}

	return resolvers
}

func registerMetricClients(metricsConfig *types.Metrics) []metrics.Registry {
	if metricsConfig == nil {
		return nil
	}

	var registries []metrics.Registry

	if metricsConfig.Prometheus != nil {
		ctx := log.With(context.Background(), log.Str(log.MetricsProviderName, "prometheus"))
		prometheusRegister := metrics.RegisterPrometheus(ctx, metricsConfig.Prometheus)
		if prometheusRegister != nil {
			registries = append(registries, prometheusRegister)
			log.FromContext(ctx).Debug("Configured Prometheus metrics")
		}
	}

	if metricsConfig.Datadog != nil {
		ctx := log.With(context.Background(), log.Str(log.MetricsProviderName, "datadog"))
		registries = append(registries, metrics.RegisterDatadog(ctx, metricsConfig.Datadog))
		log.FromContext(ctx).Debugf("Configured Datadog metrics: pushing to %s once every %s",
			metricsConfig.Datadog.Address, metricsConfig.Datadog.PushInterval)
	}

	if metricsConfig.StatsD != nil {
		ctx := log.With(context.Background(), log.Str(log.MetricsProviderName, "statsd"))
		registries = append(registries, metrics.RegisterStatsd(ctx, metricsConfig.StatsD))
		log.FromContext(ctx).Debugf("Configured StatsD metrics: pushing to %s once every %s",
			metricsConfig.StatsD.Address, metricsConfig.StatsD.PushInterval)
	}

	if metricsConfig.InfluxDB != nil {
		ctx := log.With(context.Background(), log.Str(log.MetricsProviderName, "influxdb"))
		registries = append(registries, metrics.RegisterInfluxDB(ctx, metricsConfig.InfluxDB))
		log.FromContext(ctx).Debugf("Configured InfluxDB metrics: pushing to %s once every %s",
			metricsConfig.InfluxDB.Address, metricsConfig.InfluxDB.PushInterval)
	}

	if metricsConfig.InfluxDB2 != nil {
		ctx := log.With(context.Background(), log.Str(log.MetricsProviderName, "influxdb2"))
		influxDB2Register := metrics.RegisterInfluxDB2(ctx, metricsConfig.InfluxDB2)
		if influxDB2Register != nil {
			registries = append(registries, influxDB2Register)
			log.FromContext(ctx).Debugf("Configured InfluxDB v2 metrics: pushing to %s (%s org/%s bucket) once every %s",
				metricsConfig.InfluxDB2.Address, metricsConfig.InfluxDB2.Org, metricsConfig.InfluxDB2.Bucket, metricsConfig.InfluxDB2.PushInterval)
		}
	}

	return registries
}

func appendCertMetric(gauge gokitmetrics.Gauge, certificate *x509.Certificate) {
	sort.Strings(certificate.DNSNames)

	labels := []string{
		"cn", certificate.Subject.CommonName,
		"serial", certificate.SerialNumber.String(),
		"sans", strings.Join(certificate.DNSNames, ","),
	}

	notAfter := float64(certificate.NotAfter.Unix())

	gauge.With(labels...).Set(notAfter)
}

func setupAccessLog(conf *types.AccessLog) *accesslog.Handler {
	if conf == nil {
		return nil
	}

	accessLoggerMiddleware, err := accesslog.NewHandler(conf)
	if err != nil {
		log.WithoutContext().Warnf("Unable to create access logger: %v", err)
		return nil
	}

	return accessLoggerMiddleware
}

func setupTracing(conf *static.Tracing) *tracing.Tracing {
	if conf == nil {
		return nil
	}

	var backend tracing.Backend

	if conf.Jaeger != nil {
		backend = conf.Jaeger
	}

	if conf.Zipkin != nil {
		if backend != nil {
			log.WithoutContext().Error("Multiple tracing backend are not supported: cannot create Zipkin backend.")
		} else {
			backend = conf.Zipkin
		}
	}

	if conf.Datadog != nil {
		if backend != nil {
			log.WithoutContext().Error("Multiple tracing backend are not supported: cannot create Datadog backend.")
		} else {
			backend = conf.Datadog
		}
	}

	if conf.Instana != nil {
		if backend != nil {
			log.WithoutContext().Error("Multiple tracing backend are not supported: cannot create Instana backend.")
		} else {
			backend = conf.Instana
		}
	}

	if conf.Haystack != nil {
		if backend != nil {
			log.WithoutContext().Error("Multiple tracing backend are not supported: cannot create Haystack backend.")
		} else {
			backend = conf.Haystack
		}
	}

	if conf.Elastic != nil {
		if backend != nil {
			log.WithoutContext().Error("Multiple tracing backend are not supported: cannot create Elastic backend.")
		} else {
			backend = conf.Elastic
		}
	}

	if backend == nil {
		log.WithoutContext().Debug("Could not initialize tracing, using Jaeger by default")
		defaultBackend := &jaeger.Config{}
		defaultBackend.SetDefaults()
		backend = defaultBackend
	}

	tracer, err := tracing.NewTracing(conf.ServiceName, conf.SpanNameLimit, backend)
	if err != nil {
		log.WithoutContext().Warnf("Unable to create tracer: %v", err)
		return nil
	}
	return tracer
}

func configureLogging(staticConfiguration *static.Configuration) {
	// configure default log flags
	stdlog.SetFlags(stdlog.Lshortfile | stdlog.LstdFlags)

	// configure log level
	// an explicitly defined log level always has precedence. if none is
	// given and debug mode is disabled, the default is ERROR, and DEBUG
	// otherwise.
	levelStr := "error"
	if staticConfiguration.Log != nil && staticConfiguration.Log.Level != "" {
		levelStr = strings.ToLower(staticConfiguration.Log.Level)
	}

	level, err := logrus.ParseLevel(levelStr)
	if err != nil {
		log.WithoutContext().Errorf("Error getting level: %v", err)
	}
	log.SetLevel(level)

	var logFile string
	if staticConfiguration.Log != nil && len(staticConfiguration.Log.FilePath) > 0 {
		logFile = staticConfiguration.Log.FilePath
	}

	// configure log format
	var formatter logrus.Formatter
	if staticConfiguration.Log != nil && staticConfiguration.Log.Format == "json" {
		formatter = &logrus.JSONFormatter{}
	} else {
		disableColors := len(logFile) > 0
		formatter = &logrus.TextFormatter{DisableColors: disableColors, FullTimestamp: true, DisableSorting: true}
	}
	log.SetFormatter(formatter)

	if len(logFile) > 0 {
		dir := filepath.Dir(logFile)

		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.WithoutContext().Errorf("Failed to create log path %s: %s", dir, err)
		}

		err = log.OpenFile(logFile)
		logrus.RegisterExitHandler(func() {
			if err := log.CloseFile(); err != nil {
				log.WithoutContext().Errorf("Error while closing log: %v", err)
			}
		})
		if err != nil {
			log.WithoutContext().Errorf("Error while opening log file %s: %v", logFile, err)
		}
	}
}

func checkNewVersion() {
	ticker := time.Tick(24 * time.Hour)
	safe.Go(func() {
		for time.Sleep(10 * time.Minute); ; <-ticker {
			version.CheckNewVersion()
		}
	})
}

func stats(staticConfiguration *static.Configuration) {
	logger := log.WithoutContext()

	if staticConfiguration.Global.SendAnonymousUsage {
		logger.Info(`Stats collection is enabled.`)
		logger.Info(`Many thanks for contributing to Traefik's improvement by allowing us to receive anonymous information from your configuration.`)
		logger.Info(`Help us improve Traefik by leaving this feature on :)`)
		logger.Info(`More details on: https://doc.traefik.io/traefik/contributing/data-collection/`)
		collect(staticConfiguration)
	} else {
		logger.Info(`
Stats collection is disabled.
Help us improve Traefik by turning this feature on :)
More details on: https://doc.traefik.io/traefik/contributing/data-collection/
`)
	}
}

func collect(staticConfiguration *static.Configuration) {
	ticker := time.Tick(24 * time.Hour)
	safe.Go(func() {
		for time.Sleep(10 * time.Minute); ; <-ticker {
			if err := collector.Collect(staticConfiguration); err != nil {
				log.WithoutContext().Debug(err)
			}
		}
	})
}
