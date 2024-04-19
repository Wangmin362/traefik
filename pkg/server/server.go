package server

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"time"

	"github.com/traefik/traefik/v2/pkg/log"
	"github.com/traefik/traefik/v2/pkg/metrics"
	"github.com/traefik/traefik/v2/pkg/middlewares/accesslog"
	"github.com/traefik/traefik/v2/pkg/safe"
	"github.com/traefik/traefik/v2/pkg/server/middleware"
)

// Server is the reverse-proxy/load-balancer engine.
type Server struct {
	watcher        *ConfigurationWatcher    // 配置监听器，这里应该就是动态加载配置的地方
	tcpEntryPoints TCPEntryPoints           // TCP入口点
	udpEntryPoints UDPEntryPoints           // UDP入口点
	chainBuilder   *middleware.ChainBuilder // 中间件

	accessLoggerMiddleware *accesslog.Handler // 日志处理

	signals  chan os.Signal // 退出信号
	stopChan chan bool

	routinesPool *safe.Pool // 协程池
}

// NewServer returns an initialized Server.
func NewServer(routinesPool *safe.Pool, entryPoints TCPEntryPoints, entryPointsUDP UDPEntryPoints, watcher *ConfigurationWatcher,
	chainBuilder *middleware.ChainBuilder, accessLoggerMiddleware *accesslog.Handler,
) *Server {
	srv := &Server{
		watcher:                watcher,      // 动态配置，一旦配置发生变化，Traefik会动态加载
		tcpEntryPoints:         entryPoints,  // TCP入口点
		chainBuilder:           chainBuilder, // TODO 应该是一系列中间件
		accessLoggerMiddleware: accessLoggerMiddleware,
		signals:                make(chan os.Signal, 1),
		stopChan:               make(chan bool, 1),
		routinesPool:           routinesPool,   // 协程池
		udpEntryPoints:         entryPointsUDP, // UDP入口点
	}

	srv.configureSignals()

	return srv
}

// Start starts the server and Stop/Close it when context is Done.
func (s *Server) Start(ctx context.Context) {
	go func() {
		<-ctx.Done() // 一旦上下文结束，就马上停止Traefik Server
		logger := log.FromContext(ctx)
		logger.Info("I have to go...")
		logger.Info("Stopping server gracefully")
		s.Stop()
	}()

	s.tcpEntryPoints.Start() // 启动TCP入口点
	s.udpEntryPoints.Start() // 启动UDP入口点
	s.watcher.Start()        // 监听动态配置，一旦发现配置更新，就立马热加载动态配置

	s.routinesPool.GoCtx(s.listenSignals)
}

// Wait blocks until the server shutdown.
func (s *Server) Wait() {
	<-s.stopChan
}

// Stop stops the server.
func (s *Server) Stop() {
	defer log.WithoutContext().Info("Server stopped")

	s.tcpEntryPoints.Stop()
	s.udpEntryPoints.Stop()

	s.stopChan <- true
}

// Close destroys the server.
func (s *Server) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

	go func(ctx context.Context) {
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.Canceled) {
			return
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			panic("Timeout while stopping traefik, killing instance ✝")
		}
	}(ctx)

	stopMetricsClients()

	s.routinesPool.Stop()

	signal.Stop(s.signals)
	close(s.signals)

	close(s.stopChan)

	s.chainBuilder.Close()

	cancel()
}

func stopMetricsClients() {
	metrics.StopDatadog()
	metrics.StopStatsd()
	metrics.StopInfluxDB()
	metrics.StopInfluxDB2()
}
