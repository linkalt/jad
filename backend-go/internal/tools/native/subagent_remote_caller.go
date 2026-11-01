package native

import "context"

type remoteCallerKey struct{}

// WithRemoteCaller marks a subagent call as made for a remote caller: caller is "remote:<token id>",
// set by the remote API from the authenticated token. Only the resume branch reads it.
func WithRemoteCaller(ctx context.Context, caller string) context.Context {
	return context.WithValue(ctx, remoteCallerKey{}, caller)
}

// remoteCallerOf is the remote caller a call carries, or "" for a local call.
func remoteCallerOf(ctx context.Context) string {
	caller, _ := ctx.Value(remoteCallerKey{}).(string)
	return caller
}
