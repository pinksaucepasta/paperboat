package winpipe

import "context"

type absentNowKey struct{}

// AbsentNow makes Dial report a pipe with no server instance at once instead of waiting AbsentWait. It is for a pipe
// that is never re-bound in place, an older session owner generation's: absent there means its service stopped.
func AbsentNow(ctx context.Context) context.Context {
	return context.WithValue(ctx, absentNowKey{}, true)
}

func absentNow(ctx context.Context) bool { return ctx.Value(absentNowKey{}) != nil }
