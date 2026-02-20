// Package logger provides context-based zap logger access.
package logger

import (
	"context"

	"go.uber.org/zap"
)

type contextKey struct{}

// NewContext returns a copy of ctx carrying log.
func NewContext(ctx context.Context, log *zap.Logger) context.Context {
	return context.WithValue(ctx, contextKey{}, log)
}

// L returns the logger stored in ctx, falling back to zap.L().
func L(ctx context.Context) *zap.Logger {
	if log, ok := ctx.Value(contextKey{}).(*zap.Logger); ok && log != nil {
		return log
	}
	return zap.L()
}
