// Package netx implements address preference on the node that owns the socket.
package netx

import (
	"context"
	"errors"
	"net"
	"slices"
	"time"
)

func Addresses(ctx context.Context, address string, prefer bool) ([]string, error) {
	h, p, e := net.SplitHostPort(address)
	if e != nil {
		return nil, e
	}
	if net.ParseIP(h) != nil {
		return []string{address}, nil
	}
	ips, e := net.DefaultResolver.LookupIPAddr(ctx, h)
	if e != nil {
		return nil, e
	}
	if prefer {
		slices.SortStableFunc(ips, func(a, b net.IPAddr) int {
			av, bv := a.IP.To4() == nil, b.IP.To4() == nil
			if av == bv {
				return 0
			}
			if av {
				return -1
			}
			return 1
		})
	}
	out := []string{}
	for _, ip := range ips {
		host := ip.IP.String()
		if ip.Zone != "" {
			host += "%" + ip.Zone
		}
		out = append(out, net.JoinHostPort(host, p))
	}
	if len(out) == 0 {
		return nil, errors.New("no endpoint addresses")
	}
	return out, nil
}
func Dial(ctx context.Context, network, address string, prefer bool) (net.Conn, error) {
	if !prefer {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, address)
	}
	addresses, e := Addresses(ctx, address, true)
	if e != nil {
		return nil, e
	}
	if network != "tcp" {
		var last error
		for _, a := range addresses {
			c, e := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, a)
			if e == nil {
				return c, nil
			}
			last = e
		}
		return nil, last
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		c net.Conn
		e error
	}
	ch := make(chan result)
	for i, a := range addresses {
		go func(i int, a string) {
			if i > 0 {
				t := time.NewTimer(time.Duration(i) * 250 * time.Millisecond)
				defer t.Stop()
				select {
				case <-t.C:
				case <-ctx.Done():
					return
				}
			}
			c, e := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", a)
			select {
			case ch <- result{c, e}:
			case <-ctx.Done():
				if c != nil {
					c.Close()
				}
			}
		}(i, a)
	}
	var last error
	for range addresses {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-ch:
			if r.e == nil {
				return r.c, nil
			}
			last = r.e
		}
	}
	return nil, last
}
