//go:build !linux

package probe

import (
	"context"

	gnet "github.com/shirou/gopsutil/v4/net"
)

func connectionCounts(ctx context.Context) (tcp, udp *uint64) {
	if ctx.Err() != nil {
		return nil, nil
	}
	if connections, err := gnet.ConnectionsWithoutUidsWithContext(ctx, "tcp"); err == nil {
		count := uint64(0)
		for _, c := range connections {
			if c.Status == "ESTABLISHED" || c.Status == "CLOSE_WAIT" {
				count++
			}
		}
		tcp = &count
	}
	if connections, err := gnet.ConnectionsWithoutUidsWithContext(ctx, "udp"); err == nil {
		count := uint64(len(connections))
		udp = &count
	}
	return tcp, udp
}
