package middlewares

import (
	"context"

	"github.com/traefik/traefik/v2/pkg/log"
)

// GetLoggerCtx creates a logger context with the middleware fields.
// TODO 在Traefik中，中间间被分为几类，每一类都有哪些中间件？
func GetLoggerCtx(ctx context.Context, middleware, middlewareType string) context.Context {
	return log.With(ctx, log.Str(log.MiddlewareName, middleware), log.Str(log.MiddlewareType, middlewareType))
}
