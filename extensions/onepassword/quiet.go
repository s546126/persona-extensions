package main

// Quiet asks (Persona's SecretsResolveParams.quiet, nwverse/persona#17636). 1Password's app authorizes op on a computer
// for 10 idle minutes, 12 hours at most, and until it locks (the app locks itself after its own idle time): after that
// the next op run shows the user a prompt, and with nobody at the desk it waits until it times out. So an ask that has
// an earlier answer to fall back on (a value Persona keeps, metadata this program keeps, a description) never runs
// an op that could prompt: the program on the computer first asks op whoami, which never prompts and answers only while
// the authorization holds, and answers codeLocked at once when it does not. The caller then answers what it had.
// Only an ask with nothing to fall back on (a first reference, a vault never listed) may have the user authorize op.

import "context"

type quietKey struct{}

// withQuiet marks ctx's asks as quiet: op on the computer must not prompt for them.
func withQuiet(ctx context.Context) context.Context { return context.WithValue(ctx, quietKey{}, true) }

func isQuiet(ctx context.Context) bool { q, _ := ctx.Value(quietKey{}).(bool); return q }

// quietIfKept is ctx made quiet when get finds something kept to answer instead.
func quietIfKept[T any](a *app, ctx context.Context, get func(*metaCache) (kept[T], bool)) context.Context {
	a.metaMu.Lock()
	_, ok := get(&a.meta)
	a.metaMu.Unlock()
	if ok {
		return withQuiet(ctx)
	}
	return ctx
}

// needsUser says err is op on a computer that could not answer without the user (not authorized now, locked, no
// account) or a computer that is offline: an earlier answer stands in for it. A time-out is not one of them: a quiet
// ask never waits on a prompt (authorized turns a slow whoami into codeLocked), so a time-out is 1Password or the
// network being slow, which the page and the caller report as it is.
func needsUser(err error) bool {
	switch opCode(err) {
	case codeLocked, codeNotSignedIn, "offline":
		return true
	}
	return false
}
