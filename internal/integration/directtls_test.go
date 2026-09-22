package integration

import (
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

// direct-tls 把「入口到目标」这一段包进 TLS：没有出口参与，加密在对端终止。
//
// 这个测试的目标端**只接受 TLS 连接**，所以一次成功的转发本身就证明入口确实
// 说了 TLS；随后再用一个只接受明文的目标做反向证明，避免把「碰巧能连上」
// 当成加密生效。
func TestDirectTLSEncryptsTheTargetHop(t *testing.T) {
	pair, roots := certificate(t)
	f := newFixture(t, tunnel.Client{TLS: &tls.Config{RootCAs: roots}})

	// ── 会说 TLS 的目标 ──────────────────────────────────────────
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { l.Close() })
	go echoAccept(tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{pair}}))

	// 证书签的是 localhost，所以目标也必须用这个名字拨 —— 校验名默认取
	// target 的主机部分。
	_, port, _ := net.SplitHostPort(l.Addr().String())
	rule := f.rule("tcp", "direct-tls", net.JoinHostPort("localhost", port), nil)
	f.sync()
	transfer(t, rule, []byte("tls-to-target"))

	// 规则里显式给校验名时也要能用（目标是纯 IP、证书签的却是域名，就是这种）。
	named := f.rule("tcp", "direct-tls", l.Addr().String(), &contract.Tunnel{ServerName: "localhost"})
	f.sync()
	transfer(t, named, []byte("tls-to-target-by-name"))

	// ── 反向证明：明文目标必须握手失败 ───────────────────────────
	plain, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { plain.Close() })
	// 接受后立刻关掉：入口的 TLS 握手会立刻拿到 EOF，不必等超时。
	go func() {
		for {
			c, e := plain.Accept()
			if e != nil {
				return
			}
			c.Close()
		}
	}()
	broken := f.rule("tcp", "direct-tls", plain.Addr().String(), nil)
	f.sync()
	c, e := net.DialTimeout("tcp", broken.Listen, 2*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, e = c.Write([]byte("plaintext")); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Read(make([]byte, 1)); e == nil {
		t.Fatal("明文目标不该转发成功：direct-tls 没有真的做 TLS 握手")
	}
}

func echoAccept(l net.Listener) {
	for {
		c, e := l.Accept()
		if e != nil {
			return
		}
		go func() {
			defer c.Close()
			buf := make([]byte, 512)
			for {
				n, e := c.Read(buf)
				if n > 0 {
					if _, e := c.Write(buf[:n]); e != nil {
						return
					}
				}
				if e != nil {
					return
				}
			}
		}()
	}
}
