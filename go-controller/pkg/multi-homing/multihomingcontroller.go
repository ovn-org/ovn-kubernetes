package multi_homing

import (
	"context"
)

type Controller interface {
	StartClusterMaster() error
	Run(ctx context.Context) error
}
