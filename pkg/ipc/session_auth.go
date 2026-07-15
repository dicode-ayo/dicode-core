package ipc

import "context"

// sessionAuthKey is the unexported context key marking a request as
// session-authenticated. Unexported-typed so no other package can set or forge
// it — the flag is load-bearing (it suppresses HMAC/replay verification), so it
// must be settable in exactly one place (the webui auth guard) and absent must
// mean "not session-authed".
type sessionAuthKey struct{}

// WithSessionAuth marks ctx as belonging to a request that passed dicode session
// auth. Set by the webui webhook auth guard, and only after confirming the
// request did not arrive via the relay.
func WithSessionAuth(ctx context.Context) context.Context {
	return context.WithValue(ctx, sessionAuthKey{}, true)
}

// SessionAuthed reports whether ctx was marked session-authenticated. Absent ⇒
// false ⇒ fail-closed: HMAC signature and replay checks stay in force.
func SessionAuthed(ctx context.Context) bool {
	v, _ := ctx.Value(sessionAuthKey{}).(bool)
	return v
}
