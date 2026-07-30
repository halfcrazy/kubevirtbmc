package requestid

import "context"

type ctxKey struct{}

// Header is the HTTP header used to propagate a request id.
const Header = "X-Request-ID"

func From(ctx context.Context) string {
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}

func With(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}
