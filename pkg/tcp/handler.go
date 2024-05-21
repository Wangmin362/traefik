package tcp

import (
	"net"
)

// Handler is the TCP Handlers interface.
type Handler interface {
	// ServeTCP
	// 1、TODO golang http定义的Handler的原型为ServeHTTP(ResponseWriter, *Request), 这里估计有一点类似的意味
	// 2、用于处理一个TCP连接
	// 3、HTTP连接的处理本质上就是TCP连接的处理
	ServeTCP(conn WriteCloser)
}

// The HandlerFunc type is an adapter to allow the use of
// ordinary functions as handlers.
// Handler的适配器
type HandlerFunc func(conn WriteCloser)

// ServeTCP serves tcp.
func (f HandlerFunc) ServeTCP(conn WriteCloser) {
	f(conn)
}

// WriteCloser describes a net.Conn with a CloseWrite method.
// TODO 如何理解这个抽象？ 可以向连接当中写数据，同时也可以关闭连接？
type WriteCloser interface {
	net.Conn
	// CloseWrite on a network connection, indicates that the issuer of the call
	// has terminated sending on that connection.
	// It corresponds to sending a FIN packet.
	CloseWrite() error
}
