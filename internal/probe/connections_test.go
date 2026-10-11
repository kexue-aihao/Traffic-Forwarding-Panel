package probe

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestProcConnectionCounts(t *testing.T) {
	number := func(v uint64) *uint64 { return &v }
	const snmp = "Ip: Forwarding DefaultTTL\nIp: 1 64\nTcp: RtoAlgorithm MaxConn CurrEstab InSegs\nTcp: 1 -1 7 12345\n"
	for _, tc := range []struct {
		name                  string
		snmp, v4, v6          string
		snmpErr, v4Err, v6Err error
		tcp, udp              *uint64
	}{
		{name: "dual stack", snmp: snmp, v4: "TCP: inuse 99 orphan 2 tw 500\nUDP: inuse 2 mem 10\n", v6: "TCP6: inuse 80\nUDP6: inuse 3\n", tcp: number(7), udp: number(5)},
		{name: "zero is available", snmp: "Tcp: CurrEstab\nTcp: 0\n", v4: "UDP: inuse 0\n", v6: "UDP6: inuse 0\n", tcp: number(0), udp: number(0)},
		{name: "IPv4 only", snmp: snmp, v4: "UDP: inuse 2\n", v6Err: os.ErrNotExist, tcp: number(7), udp: number(2)},
		{name: "IPv6 unreadable is not zero", snmp: snmp, v4: "UDP: inuse 2\n", v6Err: os.ErrPermission, tcp: number(7)},
		{name: "UDP unreadable keeps TCP", snmp: snmp, v4Err: os.ErrPermission, tcp: number(7)},
		{name: "TCP unreadable keeps UDP", snmpErr: os.ErrPermission, v4: "UDP: inuse 2\n", v6: "UDP6: inuse 3\n", udp: number(5)},
		{name: "TCP column mismatch", snmp: "Tcp: CurrEstab InSegs\nTcp: 7\n", v4Err: os.ErrNotExist},
		{name: "TCP missing values", snmp: "Tcp: CurrEstab\n", v4Err: os.ErrNotExist},
		{name: "TCP missing gauge", snmp: "Tcp: InSegs\nTcp: 12345\n", v4Err: os.ErrNotExist},
		{name: "TCP negative", snmp: "Tcp: CurrEstab\nTcp: -1\n", v4Err: os.ErrNotExist},
		{name: "TCP invalid", snmp: "Tcp: CurrEstab\nTcp: unknown\n", v4Err: os.ErrNotExist},
		{name: "UDP missing counter", snmp: snmp, v4: "UDP: mem 10\n", tcp: number(7)},
		{name: "UDP negative", snmp: snmp, v4: "UDP: inuse -1\n", tcp: number(7)},
		{name: "UDP6 missing counter", snmp: snmp, v4: "UDP: inuse 2\n", v6: "TCP6: inuse 3\n", tcp: number(7)},
		{name: "UDP sum overflow", snmp: snmp, v4: "UDP: inuse 18446744073709551615\n", v6: "UDP6: inuse 1\n", tcp: number(7)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tcp, udp := procConnectionCounts(t.Context(), func(path string) ([]byte, error) {
				switch path {
				case "net/snmp":
					return []byte(tc.snmp), tc.snmpErr
				case "net/sockstat":
					return []byte(tc.v4), tc.v4Err
				case "net/sockstat6":
					return []byte(tc.v6), tc.v6Err
				default:
					return nil, errors.New("unexpected proc path")
				}
			})
			if !reflect.DeepEqual(tcp, tc.tcp) || !reflect.DeepEqual(udp, tc.udp) {
				t.Fatalf("TCP/UDP = %v/%v, want %v/%v", printableCount(tcp), printableCount(udp), printableCount(tc.tcp), printableCount(tc.udp))
			}
		})
	}
}

func printableCount(v *uint64) any {
	if v == nil {
		return nil
	}
	return *v
}

func TestCancelledConnectionSampleDoesNotRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	tcp, udp := procConnectionCounts(ctx, func(string) ([]byte, error) {
		t.Fatal("cancelled sample read proc")
		return nil, nil
	})
	if tcp != nil || udp != nil {
		t.Fatal("cancelled sample fabricated counts")
	}
}
