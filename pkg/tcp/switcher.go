package tcp

import (
	"github.com/traefik/traefik/v2/pkg/safe"
)

// HandlerSwitcher is a TCP handler switcher.
// TODO 如何理解这玩意的设计？
type HandlerSwitcher struct {
	router safe.Safe // 用于保存Router
}

// ServeTCP forwards the TCP connection to the current active handler.
func (s *HandlerSwitcher) ServeTCP(conn WriteCloser) {
	// 获取Router
	handler := s.router.Get()
	h, ok := handler.(Handler)
	if ok {
		h.ServeTCP(conn)
	} else {
		// TODO 一般应该不会达到这里、如果没有找到路由，直接关闭连接
		conn.Close()
	}
}

// Switch sets the new TCP handler to use for new connections.
func (s *HandlerSwitcher) Switch(handler Handler) {
	s.router.Set(handler)
}
