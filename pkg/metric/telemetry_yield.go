package metric

import "context"

type telemetryYieldKey struct{}

// WithTelemetryWorkYield applies cooperative pacing to long streaming work.
// It is configured by the trusted maintenance worker, never a client setting.
func WithTelemetryWorkYield(ctx context.Context, yield func(context.Context) error) context.Context {
	return context.WithValue(ctx, telemetryYieldKey{}, yield)
}

func telemetryYield(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if yield, ok := ctx.Value(telemetryYieldKey{}).(func(context.Context) error); ok {
		return yield(ctx)
	}
	return nil
}
