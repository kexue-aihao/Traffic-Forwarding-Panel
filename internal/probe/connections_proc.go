package probe

import (
	"bufio"
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
)

// Read aggregate kernel counters instead of walking every process or socket.
// Linux's TCP CurrEstab already includes both IPv4 and IPv6; UDP inuse is
// per-family and must be summed. Unavailable/invalid counters stay unknown.
func procConnectionCounts(ctx context.Context, readFile func(string) ([]byte, error)) (tcp, udp *uint64) {
	if ctx.Err() != nil {
		return nil, nil
	}
	if data, err := readFile("net/snmp"); err == nil {
		tcp = tcpCurrent(data)
	}
	data4, err := readFile("net/sockstat")
	if err != nil {
		return tcp, nil
	}
	v4 := socketInUse(data4, "UDP:")
	if v4 == nil {
		return tcp, nil
	}
	data6, err := readFile("net/sockstat6")
	if errors.Is(err, os.ErrNotExist) {
		return tcp, v4 // IPv6 is not compiled/available in this namespace.
	}
	if err != nil {
		return tcp, nil
	}
	v6 := socketInUse(data6, "UDP6:")
	if v6 == nil || *v4 > ^uint64(0)-*v6 {
		return tcp, nil
	}
	total := *v4 + *v6
	return tcp, &total
}

func tcpCurrent(data []byte) *uint64 {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		header := strings.Fields(scanner.Text())
		if len(header) == 0 || header[0] != "Tcp:" {
			continue
		}
		if !scanner.Scan() {
			return nil
		}
		values := strings.Fields(scanner.Text())
		if len(values) != len(header) || values[0] != "Tcp:" {
			return nil
		}
		for i, name := range header {
			if name == "CurrEstab" {
				value, err := strconv.ParseUint(values[i], 10, 64)
				if err == nil {
					return &value
				}
				return nil
			}
		}
		return nil
	}
	return nil
}

func socketInUse(data []byte, protocol string) *uint64 {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] != protocol {
			continue
		}
		for i := 1; i+1 < len(fields); i += 2 {
			if fields[i] == "inuse" {
				value, err := strconv.ParseUint(fields[i+1], 10, 64)
				if err == nil {
					return &value
				}
				return nil
			}
		}
		return nil
	}
	return nil
}
