package modelgen

type options struct {
	dryRun bool
}

type Option func(o *options) error

func newOptions(opts ...Option) (*options, error) {
	o := &options{}
	for _, opt := range opts {
		if err := opt(o); err != nil {
			return nil, err
		}
	}
	return o, nil
}

func WithDryRun() Option {
	return func(o *options) error {
		o.dryRun = true
		return nil
	}
}
