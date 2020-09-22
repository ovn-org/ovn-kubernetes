package informer

// These constants can be removed at some point
// They are here to provide backwards-compatibility with the
// pkg/factory code which provided defaults.
// The package consumer should make these decisions instead.
const (
	// DefaultResyncInterval is the default interval that all caches should
	// periodically resync. AddEventHandlerWithResyncPeriod can specify a
	// per handler resync period if necessary
	DefaultResyncInterval = 0
	// DefaultNodeInformerThreadiness is the number of worker routines spawned
	// to services the Node event queue
	DefaultNodeInformerThreadiness = 10
	// DefaultInformerThreadiness is the number of goroutines spawned
	// to service an informer event queue
	DefaultInformerThreadiness = 1
	// MaxRetries is the maximum number of times we'll retry an item on the queue
	MaxRetries = 3
)
