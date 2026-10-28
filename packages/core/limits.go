package core

import (
	"context"
	"errors"
)

// ErrResourceLimit reports an explicit read budget being exceeded. Results
// are never silently truncated. Narrow the selection or use an offline backup.
var ErrResourceLimit = errors.New("read budget exceeded, narrow the selection or use an offline backup")

type ReadLimits struct {
	MaxEvents int
	MaxBytes  int64
}
type readLimitsKey struct{}

func WithReadLimits(ctx context.Context, limits ReadLimits) context.Context {
	return context.WithValue(ctx, readLimitsKey{}, limits)
}
func ReadLimitsFromContext(ctx context.Context) ReadLimits {
	limits, _ := ctx.Value(readLimitsKey{}).(ReadLimits)
	return limits
}
