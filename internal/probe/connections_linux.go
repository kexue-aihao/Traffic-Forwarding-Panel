//go:build linux

package probe

import (
	"context"
	"os"
	"path/filepath"
)

func connectionCounts(ctx context.Context) (*uint64, *uint64) {
	return procConnectionCounts(ctx, func(name string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return os.ReadFile(filepath.Join("/proc", name))
	})
}
