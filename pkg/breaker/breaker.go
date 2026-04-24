package breaker

import (
	"context"
	"sync/atomic"

	"github.com/kyverno/kyverno/pkg/metrics"
)

var reportsBreaker atomic.Value

// nopBreaker is a pass-through used when the reports breaker has not been initialized
// (e.g., in tests or standalone runs). It always executes the inner function.
type nopBreaker struct{}

func (nopBreaker) Do(ctx context.Context, inner func(context.Context) error) error {
	if inner == nil {
		return nil
	}
	return inner(ctx)
}

func GetReportsBreaker() Breaker {
	if v := reportsBreaker.Load(); v != nil {
		return v.(Breaker)
	}
	return nopBreaker{}
}

func SetReportsBreaker(b Breaker) {
	reportsBreaker.Store(b)
}

type Breaker interface {
	Do(context.Context, func(context.Context) error) error
}

type breaker struct {
	name string
	open func(context.Context) bool
}

func NewBreaker(name string, open func(context.Context) bool) *breaker {
	return &breaker{
		name: name,
		open: open,
	}
}

func (b *breaker) Do(ctx context.Context, inner func(context.Context) error) error {
	metrics := metrics.GetBreakerMetrics()

	if metrics != nil {
		metrics.RecordTotalIncrease(ctx, b.name)
	}
	if b.open != nil && b.open(ctx) {
		if metrics != nil {
			metrics.RecordDrop(ctx, b.name)
		}
		return nil
	}
	if inner == nil {
		return nil
	}
	return inner(ctx)
}
