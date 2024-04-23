package chain

import (
	"context"
	"net/http"

	"github.com/containous/alice"
	"github.com/traefik/traefik/v2/pkg/config/dynamic"
	"github.com/traefik/traefik/v2/pkg/log"
	"github.com/traefik/traefik/v2/pkg/middlewares"
)

const (
	typeName = "Chain"
)

// TODO 如何理解这玩意？看起来也是一个中间件
// 把给定的中间件构造为一个中间链
type chainBuilder interface {
	BuildChain(ctx context.Context, middlewares []string) *alice.Chain
}

// New creates a chain middleware.
func New(ctx context.Context, next http.Handler, config dynamic.Chain, builder chainBuilder, name string) (http.Handler, error) {
	log.FromContext(middlewares.GetLoggerCtx(ctx, name, typeName)).Debug("Creating middleware")

	middlewareChain := builder.BuildChain(ctx, config.Middlewares)
	return middlewareChain.Then(next)
}
