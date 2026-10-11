package tunnel

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

func TestMuxLargeWindowInteroperatesWithLegacyPeer(t *testing.T) {
	for _, legacyReceiver := range []bool{false, true} {
		name := "legacy_sender"
		if legacyReceiver {
			name = "legacy_receiver"
		}
		t.Run(name, func(t *testing.T) {
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			old, current := muxConfig(), muxConfig()
			old.MaxStreamWindowSize = 256 << 10
			clientConfig, serverConfig := old, current
			if legacyReceiver {
				clientConfig, serverConfig = current, old
			}
			client, err := yamux.Client(left, clientConfig)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			server, err := yamux.Server(right, serverConfig)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			sender, err := client.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer sender.Close()
			receiver, err := server.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer receiver.Close()
			sender.SetDeadline(time.Now().Add(3 * time.Second))
			receiver.SetDeadline(time.Now().Add(3 * time.Second))
			payload := bytes.Repeat([]byte("mux-window"), 1<<17)
			sent := make(chan error, 1)
			go func() { sent <- writeAll(sender, payload) }()
			if !legacyReceiver {
				// The new receiver accepts more than the old 256 KiB before
				// the application drains anything or sends another update.
				select {
				case err := <-sent:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("large stream stalled at legacy flow-control window")
				}
			}
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(receiver, got); err != nil || !bytes.Equal(got, payload) {
				t.Fatal("window negotiation corrupted the stream", err)
			}
			if legacyReceiver {
				if err := <-sent; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
