package multi_homing

import (
	"context"
)

type Controller interface {
	Run(ctx context.Context) error
}
