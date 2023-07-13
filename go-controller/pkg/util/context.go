package util

import "context"

// CancelableContext utility wraps a context that can be canceled
type CancelableContext struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// Done returns a channel that is closed when this or any parent context is
// canceled
func (ctx *CancelableContext) Done() <-chan struct{} {
	return ctx.ctx.Done()
}

// Cancel this context
func (ctx *CancelableContext) Cancel() {
	ctx.cancel()
}

func NewCancelableContext() CancelableContext {
	return newCancelableContext(context.Background())
}

func NewCancelableContextChild(ctx CancelableContext) CancelableContext {
	return newCancelableContext(ctx.ctx)
}

func newCancelableContext(ctx context.Context) CancelableContext {
	ctx, cancel := context.WithCancel(ctx)
	return CancelableContext{
		ctx:    ctx,
		cancel: cancel,
	}
}
