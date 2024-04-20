package server

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	stdlog "log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containous/alice"
	"github.com/pires/go-proxyproto"
	"github.com/sirupsen/logrus"
	"github.com/traefik/traefik/v2/pkg/config/static"
	"github.com/traefik/traefik/v2/pkg/ip"
	"github.com/traefik/traefik/v2/pkg/log"
	"github.com/traefik/traefik/v2/pkg/middlewares"
	"github.com/traefik/traefik/v2/pkg/middlewares/forwardedheaders"
	"github.com/traefik/traefik/v2/pkg/middlewares/requestdecorator"
	"github.com/traefik/traefik/v2/pkg/safe"
	"github.com/traefik/traefik/v2/pkg/server/router"
	tcprouter "github.com/traefik/traefik/v2/pkg/server/router/tcp"
	"github.com/traefik/traefik/v2/pkg/server/service"
	"github.com/traefik/traefik/v2/pkg/tcp"
	"github.com/traefik/traefik/v2/pkg/types"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

var httpServerLogger = stdlog.New(log.WithoutContext().WriterLevel(logrus.DebugLevel), "", 0)

type key string

const (
	connStateKey key = "connState"
	// TODO 这玩意有啥用？
	debugConnectionEnv string = "DEBUG_CONNECTION"
)

var (
	clientConnectionStates   = map[string]*connState{}
	clientConnectionStatesMu = sync.RWMutex{}
)

type connState struct {
	// TODO 连接状态，应按是TCP连接状态
	State string
	// TODO 应该指的是TCP Keepalive状态
	KeepAliveState string
	// TODO
	Start time.Time
	// TODO 当前连接中的HTTP请求数量？
	HTTPRequestCount int
}

// TODO 这玩意是干嘛用的？ 看名字似乎是用来转发流量的
type httpForwarder struct {
	net.Listener               // 既然是HTTP的，那么必然是基于TCP协议的类型。Listener.Accept()获取到的就是网络连接
	connChan     chan net.Conn // 网络连接channel
	errChan      chan error
}

func newHTTPForwarder(ln net.Listener) *httpForwarder {
	return &httpForwarder{
		Listener: ln,
		connChan: make(chan net.Conn), // 没有缓冲
		errChan:  make(chan error),
	}
}

// ServeTCP uses the connection to serve it later in "Accept".
func (h *httpForwarder) ServeTCP(conn tcp.WriteCloser) {
	h.connChan <- conn
}

// Accept retrieves a served connection in ServeTCP.
func (h *httpForwarder) Accept() (net.Conn, error) {
	select {
	case conn := <-h.connChan:
		return conn, nil
	case err := <-h.errChan:
		return nil, err
	}
}

// TCPEntryPoints holds a map of TCPEntryPoint (the entrypoint names being the keys).
type TCPEntryPoints map[string]*TCPEntryPoint

// NewTCPEntryPoints creates a new TCPEntryPoints.
func NewTCPEntryPoints(entryPointsConfig static.EntryPoints, hostResolverConfig *types.HostResolverConfig) (TCPEntryPoints, error) {
	if os.Getenv(debugConnectionEnv) != "" {
		expvar.Publish("clientConnectionStates", expvar.Func(func() any {
			return clientConnectionStates
		}))
	}

	// 解析配置文件，把用户配置的入口点，一个个实力换位TCPEntryPoint
	serverEntryPointsTCP := make(TCPEntryPoints)
	for entryPointName, config := range entryPointsConfig {
		// 获取当前入口点的协议，要么是TCP，要么是UDP
		protocol, err := config.GetProtocol()
		if err != nil {
			return nil, fmt.Errorf("error while building entryPoint %s: %w", entryPointName, err)
		}

		// 这里实在处理TCP入口点，因此直接忽略除了TCP以外的其他任何协议
		if protocol != "tcp" {
			continue
		}

		ctx := log.With(context.Background(), log.Str(log.EntryPointName, entryPointName))

		// 实例化TCP入口点
		serverEntryPointsTCP[entryPointName], err = NewTCPEntryPoint(ctx, config, hostResolverConfig)
		if err != nil {
			return nil, fmt.Errorf("error while building entryPoint %s: %w", entryPointName, err)
		}
	}
	return serverEntryPointsTCP, nil
}

// Start the server entry points.
func (eps TCPEntryPoints) Start() {
	// 挨个在协程当中启动各个入口点
	for entryPointName, serverEntryPoint := range eps {
		ctx := log.With(context.Background(), log.Str(log.EntryPointName, entryPointName))
		go serverEntryPoint.Start(ctx)
	}
}

// Stop the server entry points.
func (eps TCPEntryPoints) Stop() {
	var wg sync.WaitGroup

	for epn, ep := range eps {
		wg.Add(1)

		go func(entryPointName string, entryPoint *TCPEntryPoint) {
			defer wg.Done()

			ctx := log.With(context.Background(), log.Str(log.EntryPointName, entryPointName))
			entryPoint.Shutdown(ctx)

			log.FromContext(ctx).Debugf("Entry point %s closed", entryPointName)
		}(epn, ep)
	}

	wg.Wait()
}

// Switch the TCP routers.
func (eps TCPEntryPoints) Switch(routersTCP map[string]*tcprouter.Router) {
	for entryPointName, rt := range routersTCP {
		eps[entryPointName].SwitchRouter(rt)
	}
}

// TCPEntryPoint is the TCP server.
// TODO 中间件是怎么在入口点中体现的？
type TCPEntryPoint struct {
	// Traefik将来会针对每一个TCP入口点启动一个Listener，监听端口流量
	listener net.Listener
	// 1、其实就是用于获取Router的玩意，然后根据当前请求在Router中找到最合适的一个路由处理流量
	// 2、其实可以简单的理解为就是Router，当一个TCP连接进来之后，Router需要负责根据用户配置的路由匹配一个最合适的路由，并把流量转发给对应的服务
	switcher *tcp.HandlerSwitcher
	// 入口点的静态配置
	transportConfiguration *static.EntryPointsTransport
	// TODO 应该是和链路追踪有关管的东西
	tracker    *connectionTracker
	httpServer *httpServer
	// TODO 为什么HttpsServer可以直接使用HTTPServer? 我猜测实在Listener会进行区分，配置证书
	httpsServer *httpServer

	http3Server *http3server
}

// NewTCPEntryPoint creates a new TCPEntryPoint.
func NewTCPEntryPoint(ctx context.Context,
	configuration *static.EntryPoint, // 可以理解为用户配置的静态文件
	hostResolverConfig *types.HostResolverConfig, // TODO 这玩意有啥用？
) (*TCPEntryPoint, error) {
	// 实例化一个连接追踪器
	tracker := newConnectionTracker()

	// 监听入口点，并且对于每个客户端连接，都设置为keepalive
	listener, err := buildListener(ctx, configuration)
	if err != nil {
		return nil, fmt.Errorf("error preparing server: %w", err)
	}

	// TODO 理解里面的路由设计
	// TODO 为什么这里只初始化了HTTPForwarder, HTTPSForwarder
	rt := &tcprouter.Router{}

	// TODO 似乎是自己做域名解析
	reqDecorator := requestdecorator.New(hostResolverConfig)

	// TODO 实例化一个HTTPServer
	httpServer, err := createHTTPServer(ctx, listener, configuration, true, reqDecorator)
	if err != nil {
		return nil, fmt.Errorf("error preparing http server: %w", err)
	}

	rt.SetHTTPForwarder(httpServer.Forwarder)

	// TODO 实例化一个HTTPSServer
	httpsServer, err := createHTTPServer(ctx, listener, configuration, false, reqDecorator)
	if err != nil {
		return nil, fmt.Errorf("error preparing https server: %w", err)
	}

	// TODO 实例化一个HTTP3Server
	h3Server, err := newHTTP3Server(ctx, configuration, httpsServer)
	if err != nil {
		return nil, fmt.Errorf("error preparing http3 server: %w", err)
	}

	rt.SetHTTPSForwarder(httpsServer.Forwarder)

	// 其实就是Router
	tcpSwitcher := &tcp.HandlerSwitcher{}
	tcpSwitcher.Switch(rt)

	return &TCPEntryPoint{
		listener:               listener,
		switcher:               tcpSwitcher,
		transportConfiguration: configuration.Transport,
		tracker:                tracker,
		httpServer:             httpServer,
		httpsServer:            httpsServer,
		http3Server:            h3Server,
	}, nil
}

// Start starts the TCP server.
// 1、针对于一个TCP入口点，其实都需要启动一个TCP Server，用于在用户指定的入口上监听网络数据包
func (e *TCPEntryPoint) Start(ctx context.Context) {
	logger := log.FromContext(ctx)
	logger.Debug("Starting TCP Server")

	if e.http3Server != nil { // 如果支持http3，就启动http3 Server
		go func() { _ = e.http3Server.Start() }()
	}

	for {
		conn, err := e.listener.Accept() // 阻塞，等待客户端连接到当前的入口点
		if err != nil {
			logger.Error(err)

			var opErr *net.OpError
			if errors.As(err, &opErr) && opErr.Temporary() {
				continue
			}

			var urlErr *url.Error
			if errors.As(err, &urlErr) && urlErr.Temporary() {
				continue
			}

			e.httpServer.Forwarder.errChan <- err
			e.httpsServer.Forwarder.errChan <- err

			return
		}

		// 封装为WriteCloser
		writeCloser, err := writeCloser(conn)
		if err != nil {
			panic(err)
		}

		// 启动一个协程处理当前的连接
		// TODO 这里的safe有何用意？什么情况下是不完全的？有哪些临界值？
		safe.Go(func() {
			// Enforce read/write deadlines at the connection level,
			// because when we're peeking the first byte to determine whether we are doing TLS,
			// the deadlines at the server level are not taken into account.
			if e.transportConfiguration.RespondingTimeouts.ReadTimeout > 0 {
				// 设置读取的超时间
				// TODO 如果在规定时间之内，没有读取到数据，TCP连接是怎么处理的？
				err := writeCloser.SetReadDeadline(time.Now().Add(time.Duration(e.transportConfiguration.RespondingTimeouts.ReadTimeout)))
				if err != nil {
					logger.Errorf("Error while setting read deadline: %v", err)
				}
			}

			if e.transportConfiguration.RespondingTimeouts.WriteTimeout > 0 {
				// 设置连接写入的超时间
				// TODO 如果在规定时间之内，无法写入数据到TCP连接，TCP连接是怎么处理的？
				err = writeCloser.SetWriteDeadline(time.Now().Add(time.Duration(e.transportConfiguration.RespondingTimeouts.WriteTimeout)))
				if err != nil {
					logger.Errorf("Error while setting write deadline: %v", err)
				}
			}

			// TODO 这里的逻辑应该是：找到当前入口点的所有Router，挨个匹配，以最精确的匹配路由为准，把数据io拷贝到目标服务，当前数据流入
			// 服务之前涉及到请求消息的处理。在数据流出服务转发给客户端之前，可以对于数据进行修改。这些都是中间件的职责
			e.switcher.ServeTCP(newTrackedConnection(writeCloser, e.tracker))
		})
	}
}

// Shutdown stops the TCP connections.
func (e *TCPEntryPoint) Shutdown(ctx context.Context) {
	logger := log.FromContext(ctx)

	reqAcceptGraceTimeOut := time.Duration(e.transportConfiguration.LifeCycle.RequestAcceptGraceTimeout)
	if reqAcceptGraceTimeOut > 0 {
		logger.Infof("Waiting %s for incoming requests to cease", reqAcceptGraceTimeOut)
		time.Sleep(reqAcceptGraceTimeOut)
	}

	graceTimeOut := time.Duration(e.transportConfiguration.LifeCycle.GraceTimeOut)
	ctx, cancel := context.WithTimeout(ctx, graceTimeOut)
	logger.Debugf("Waiting %s seconds before killing connections.", graceTimeOut)

	// 零值初始化
	var wg sync.WaitGroup

	shutdownServer := func(server stoppable) {
		defer wg.Done()
		err := server.Shutdown(ctx)
		if err == nil {
			return
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			logger.Debugf("Server failed to shutdown within deadline because: %s", err)
			if err = server.Close(); err != nil {
				logger.Error(err)
			}
			return
		}
		logger.Error(err)
		// We expect Close to fail again because Shutdown most likely failed when trying to close a listener.
		// We still call it however, to make sure that all connections get closed as well.
		server.Close()
	}

	if e.httpServer.Server != nil {
		wg.Add(1)
		go shutdownServer(e.httpServer.Server)
	}

	if e.httpsServer.Server != nil {
		wg.Add(1)
		go shutdownServer(e.httpsServer.Server)

		if e.http3Server != nil {
			wg.Add(1)
			go shutdownServer(e.http3Server)
		}
	}

	if e.tracker != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := e.tracker.Shutdown(ctx)
			if err == nil {
				return
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				logger.Debugf("Server failed to shutdown before deadline because: %s", err)
			}
			e.tracker.Close()
		}()
	}

	wg.Wait()
	cancel()
}

// SwitchRouter switches the TCP router handler.
// TODO 什么叫做切换路由？
func (e *TCPEntryPoint) SwitchRouter(rt *tcprouter.Router) {
	rt.SetHTTPForwarder(e.httpServer.Forwarder)

	httpHandler := rt.GetHTTPHandler()
	if httpHandler == nil {
		httpHandler = router.BuildDefaultHTTPRouter()
	}

	e.httpServer.Switcher.UpdateHandler(httpHandler)

	rt.SetHTTPSForwarder(e.httpsServer.Forwarder)

	httpsHandler := rt.GetHTTPSHandler()
	if httpsHandler == nil {
		httpsHandler = router.BuildDefaultHTTPRouter()
	}

	e.httpsServer.Switcher.UpdateHandler(httpsHandler)

	e.switcher.Switch(rt)

	if e.http3Server != nil {
		e.http3Server.Switch(rt)
	}
}

// writeCloserWrapper wraps together a connection, and the concrete underlying
// connection type that was found to satisfy WriteCloser.
type writeCloserWrapper struct {
	net.Conn
	writeCloser tcp.WriteCloser
}

func (c *writeCloserWrapper) CloseWrite() error {
	return c.writeCloser.CloseWrite()
}

// writeCloser returns the given connection, augmented with the WriteCloser
// implementation, if any was found within the underlying conn.
func writeCloser(conn net.Conn) (tcp.WriteCloser, error) {
	switch typedConn := conn.(type) {
	case *proxyproto.Conn: // TODO 这个连接类型有啥用？ 什么时候会是这种类型的连接？
		underlying, ok := typedConn.TCPConn()
		if !ok {
			return nil, errors.New("underlying connection is not a tcp connection")
		}
		return &writeCloserWrapper{writeCloser: underlying, Conn: typedConn}, nil
	case *net.TCPConn:
		return typedConn, nil
	default:
		return nil, fmt.Errorf("unknown connection type %T", typedConn)
	}
}

// tcpKeepAliveListener sets TCP keep-alive timeouts on accepted
// connections.
type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (net.Conn, error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return nil, err
	}

	// 设置当前的连接为keepalive
	if err := tc.SetKeepAlive(true); err != nil {
		return nil, err
	}

	// TODO 这里设置为3分钟是什么意思？
	if err := tc.SetKeepAlivePeriod(3 * time.Minute); err != nil {
		// Some systems, such as OpenBSD, have no user-settable per-socket TCP
		// keepalive options.
		if !errors.Is(err, syscall.ENOPROTOOPT) {
			return nil, err
		}
	}

	return tc, nil
}

func buildProxyProtocolListener(ctx context.Context, entryPoint *static.EntryPoint, listener net.Listener) (net.Listener, error) {
	timeout := entryPoint.Transport.RespondingTimeouts.ReadTimeout
	// proxyproto use 200ms if ReadHeaderTimeout is set to 0 and not no timeout
	if timeout == 0 {
		timeout = -1
	}
	proxyListener := &proxyproto.Listener{Listener: listener, ReadHeaderTimeout: time.Duration(timeout)}

	if entryPoint.ProxyProtocol.Insecure {
		log.FromContext(ctx).Infof("Enabling ProxyProtocol without trusted IPs: Insecure")
		return proxyListener, nil
	}

	checker, err := ip.NewChecker(entryPoint.ProxyProtocol.TrustedIPs)
	if err != nil {
		return nil, err
	}

	proxyListener.Policy = func(upstream net.Addr) (proxyproto.Policy, error) {
		ipAddr, ok := upstream.(*net.TCPAddr)
		if !ok {
			return proxyproto.REJECT, fmt.Errorf("type error %v", upstream)
		}

		if !checker.ContainsIP(ipAddr.IP) {
			log.FromContext(ctx).Debugf("IP %s is not in trusted IPs list, ignoring ProxyProtocol Headers and bypass connection", ipAddr.IP)
			return proxyproto.IGNORE, nil
		}
		return proxyproto.USE, nil
	}

	log.FromContext(ctx).Infof("Enabling ProxyProtocol for trusted IPs %v", entryPoint.ProxyProtocol.TrustedIPs)

	return proxyListener, nil
}

func buildListener(ctx context.Context, entryPoint *static.EntryPoint) (net.Listener, error) {
	listener, err := net.Listen("tcp", entryPoint.GetAddress())
	if err != nil {
		return nil, fmt.Errorf("error opening listener: %w", err)
	}

	// 强制转为TCP Keepalive连接，里面对于每个连接都设置的keepalive
	listener = tcpKeepAliveListener{listener.(*net.TCPListener)}

	// TODO 如果设置了代理协议，那么需要处理代理协议相关的东西
	if entryPoint.ProxyProtocol != nil {
		listener, err = buildProxyProtocolListener(ctx, entryPoint, listener)
		if err != nil {
			return nil, fmt.Errorf("error creating proxy protocol listener: %w", err)
		}
	}
	return listener, nil
}

func newConnectionTracker() *connectionTracker {
	return &connectionTracker{
		conns: make(map[net.Conn]struct{}),
	}
}

// TODO 连接追踪是如何设计的？
type connectionTracker struct {
	conns map[net.Conn]struct{}
	lock  sync.RWMutex
}

// AddConnection add a connection in the tracked connections list.
func (c *connectionTracker) AddConnection(conn net.Conn) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.conns[conn] = struct{}{}
}

// RemoveConnection remove a connection from the tracked connections list.
func (c *connectionTracker) RemoveConnection(conn net.Conn) {
	c.lock.Lock()
	defer c.lock.Unlock()
	delete(c.conns, conn)
}

func (c *connectionTracker) isEmpty() bool {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return len(c.conns) == 0
}

// Shutdown wait for the connection closing.
func (c *connectionTracker) Shutdown(ctx context.Context) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if c.isEmpty() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Close close all the connections in the tracked connections list.
func (c *connectionTracker) Close() {
	c.lock.Lock()
	defer c.lock.Unlock()
	for conn := range c.conns {
		if err := conn.Close(); err != nil {
			log.WithoutContext().Errorf("Error while closing connection: %v", err)
		}
		delete(c.conns, conn)
	}
}

type stoppable interface {
	Shutdown(ctx context.Context) error
	Close() error
}

type stoppableServer interface {
	stoppable
	Serve(listener net.Listener) error
}

type httpServer struct {
	Server    stoppableServer
	Forwarder *httpForwarder                   // TODO 看起来是用于转发流量
	Switcher  *middlewares.HTTPHandlerSwitcher // TODO 如何理解这玩意？
}

func createHTTPServer(
	ctx context.Context,
	ln net.Listener, // 入口点监听器
	configuration *static.EntryPoint, // 入口点的静态配置
	withH2c bool, // 这个参数似乎就是用于区分当前创建的是HTTPServer还是HTTPSServer。 withH2c=true则是创建HTTPServer，否则则创建HTTPServer
	reqDecorator *requestdecorator.RequestDecorator, // TODO 似乎是用于给请求上下文增加CNAME相关东西
) (*httpServer, error) {
	if configuration.HTTP2.MaxConcurrentStreams < 0 {
		return nil, errors.New("max concurrent streams value must be greater than or equal to zero")
	}

	httpSwitcher := middlewares.NewHandlerSwitcher(router.BuildDefaultHTTPRouter())

	next, err := alice.New(requestdecorator.WrapHandler(reqDecorator)).Then(httpSwitcher)
	if err != nil {
		return nil, err
	}

	var handler http.Handler
	handler, err = forwardedheaders.NewXForwarded(
		configuration.ForwardedHeaders.Insecure,
		configuration.ForwardedHeaders.TrustedIPs,
		next)
	if err != nil {
		return nil, err
	}

	handler = denyFragment(handler)
	if configuration.HTTP.EncodeQuerySemicolons {
		handler = encodeQuerySemicolons(handler)
	} else {
		handler = http.AllowQuerySemicolons(handler)
	}

	if withH2c { // True则是创建HttpServer，否则创建的是HttpsServer
		handler = h2c.NewHandler(handler, &http2.Server{
			MaxConcurrentStreams: uint32(configuration.HTTP2.MaxConcurrentStreams),
		})
	}

	debugConnection := os.Getenv(debugConnectionEnv) != ""
	if debugConnection || (configuration.Transport != nil && (configuration.Transport.KeepAliveMaxTime > 0 || configuration.Transport.KeepAliveMaxRequests > 0)) {
		handler = newKeepAliveMiddleware(handler, configuration.Transport.KeepAliveMaxRequests, configuration.Transport.KeepAliveMaxTime)
	}

	serverHTTP := &http.Server{
		Handler:      handler,
		ErrorLog:     httpServerLogger,
		ReadTimeout:  time.Duration(configuration.Transport.RespondingTimeouts.ReadTimeout),
		WriteTimeout: time.Duration(configuration.Transport.RespondingTimeouts.WriteTimeout),
		IdleTimeout:  time.Duration(configuration.Transport.RespondingTimeouts.IdleTimeout),
	}

	// TODO 配置keepalive
	if debugConnection || (configuration.Transport != nil && (configuration.Transport.KeepAliveMaxTime > 0 || configuration.Transport.KeepAliveMaxRequests > 0)) {
		serverHTTP.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
			cState := &connState{Start: time.Now()}
			if debugConnection {
				clientConnectionStatesMu.Lock()
				clientConnectionStates[getConnKey(c)] = cState
				clientConnectionStatesMu.Unlock()
			}
			return context.WithValue(ctx, connStateKey, cState)
		}

		if debugConnection {
			serverHTTP.ConnState = func(c net.Conn, state http.ConnState) {
				clientConnectionStatesMu.Lock()
				if clientConnectionStates[getConnKey(c)] != nil {
					clientConnectionStates[getConnKey(c)].State = state.String()
				}
				clientConnectionStatesMu.Unlock()
			}
		}
	}

	prevConnContext := serverHTTP.ConnContext
	serverHTTP.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		// This adds an empty struct in order to store a RoundTripper in the ConnContext in case of Kerberos or NTLM.
		// TODO 这里是在干嘛？
		ctx = service.AddTransportOnContext(ctx)
		if prevConnContext != nil {
			return prevConnContext(ctx, c)
		}
		return ctx
	}

	// ConfigureServer configures HTTP/2 with the MaxConcurrentStreams option for the given server.
	// Also keeping behavior the same as
	// https://cs.opensource.google/go/go/+/refs/tags/go1.17.7:src/net/http/server.go;l=3262
	if !strings.Contains(os.Getenv("GODEBUG"), "http2server=0") {
		err = http2.ConfigureServer(serverHTTP, &http2.Server{
			MaxConcurrentStreams: uint32(configuration.HTTP2.MaxConcurrentStreams),
			NewWriteScheduler:    func() http2.WriteScheduler { return http2.NewPriorityWriteScheduler(nil) },
		})
		if err != nil {
			return nil, fmt.Errorf("configure HTTP/2 server: %w", err)
		}
	}

	listener := newHTTPForwarder(ln)
	go func() {
		// 启动HTTPServer，等待客户端连接
		err := serverHTTP.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.FromContext(ctx).Errorf("Error while starting server: %v", err)
		}
	}()
	return &httpServer{
		Server:    serverHTTP,
		Forwarder: listener,
		Switcher:  httpSwitcher,
	}, nil
}

func getConnKey(conn net.Conn) string {
	return fmt.Sprintf("%s => %s", conn.RemoteAddr(), conn.LocalAddr())
}

func newTrackedConnection(conn tcp.WriteCloser, tracker *connectionTracker) *trackedConnection {
	// TODO 如何追踪当前的连接？
	tracker.AddConnection(conn)
	return &trackedConnection{
		WriteCloser: conn,
		tracker:     tracker,
	}
}

type trackedConnection struct {
	tracker *connectionTracker
	tcp.WriteCloser
}

func (t *trackedConnection) Close() error {
	t.tracker.RemoveConnection(t.WriteCloser)
	return t.WriteCloser.Close()
}

// This function is inspired by http.AllowQuerySemicolons.
func encodeQuerySemicolons(h http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.RawQuery, ";") {
			r2 := new(http.Request)
			*r2 = *req
			r2.URL = new(url.URL)
			*r2.URL = *req.URL

			r2.URL.RawQuery = strings.ReplaceAll(req.URL.RawQuery, ";", "%3B")
			// Because the reverse proxy director is building query params from requestURI it needs to be updated as well.
			r2.RequestURI = r2.URL.RequestURI()

			h.ServeHTTP(rw, r2)
		} else {
			h.ServeHTTP(rw, req)
		}
	})
}

// When go receives an HTTP request, it assumes the absence of fragment URL.
// However, it is still possible to send a fragment in the request.
// In this case, Traefik will encode the '#' character, altering the request's intended meaning.
// To avoid this behavior, the following function rejects requests that include a fragment in the URL.
func denyFragment(h http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.RawPath, "#") {
			log.WithoutContext().Debugf("Rejecting request because it contains a fragment in the URL path: %s", req.URL.RawPath)
			rw.WriteHeader(http.StatusBadRequest)

			return
		}

		h.ServeHTTP(rw, req)
	})
}
