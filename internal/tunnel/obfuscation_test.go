package tunnel

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestRandomPaddingObfsRoundTrip(t *testing.T) {
	obfs := &RandomPaddingObfs{minPad: 10, maxPad: 255}

	tests := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"small", []byte("hello")},
		{"medium", bytes.Repeat([]byte("x"), 1024)},
		{"large", bytes.Repeat([]byte("y"), 32*1024)},
		{"near-maxFrame", bytes.Repeat([]byte("z"), maxFrame-500)}, // Leave room for overhead
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obfuscated, err := obfs.ObfuscateWrite(tt.data)
			if err != nil {
				t.Fatalf("ObfuscateWrite failed: %v", err)
			}

			// Check overhead
			if len(obfuscated) < len(tt.data)+2+obfs.minPad {
				t.Errorf("obfuscated size %d < expected min %d", len(obfuscated), len(tt.data)+2+obfs.minPad)
			}
			if len(obfuscated) > len(tt.data)+2+obfs.maxPad {
				t.Errorf("obfuscated size %d > expected max %d", len(obfuscated), len(tt.data)+2+obfs.maxPad)
			}

			// Deobfuscate
			plaintext, err := obfs.DeobfuscateRead(obfuscated)
			if err != nil {
				t.Fatalf("DeobfuscateRead failed: %v", err)
			}

			if !bytes.Equal(plaintext, tt.data) {
				t.Errorf("roundtrip mismatch: got %d bytes, want %d bytes", len(plaintext), len(tt.data))
			}
		})
	}
}

func TestRandomPaddingObfsErrors(t *testing.T) {
	obfs := &RandomPaddingObfs{minPad: 10, maxPad: 255}

	t.Run("truncated frame", func(t *testing.T) {
		_, err := obfs.DeobfuscateRead([]byte{0x00})
		if err == nil {
			t.Error("expected error for truncated frame")
		}
	})

	t.Run("invalid length", func(t *testing.T) {
		invalidFrame := []byte{0xFF, 0xFF, 0x00} // length = 65535, data = 1 byte
		_, err := obfs.DeobfuscateRead(invalidFrame)
		if err == nil {
			t.Error("expected error for invalid length")
		}
	})

	t.Run("invalid padding range", func(t *testing.T) {
		invalidObfs := &RandomPaddingObfs{minPad: 100, maxPad: 10}
		_, err := invalidObfs.ObfuscateWrite([]byte("test"))
		if err == nil {
			t.Error("expected error for invalid padding range")
		}
	})
}

func TestNewObfuscator(t *testing.T) {
	t.Run("random-padding", func(t *testing.T) {
		cfg := &contract.ObfuscationConfig{
			Strategy: "random-padding",
			Params: map[string]any{
				"min_pad": float64(10),
				"max_pad": float64(255),
			},
		}
		obfs, err := newObfuscator(cfg)
		if err != nil {
			t.Fatalf("newObfuscator failed: %v", err)
		}
		if obfs == nil {
			t.Fatal("expected non-nil obfuscator")
		}

		// Test it works
		data := []byte("test data")
		obfuscated, err := obfs.ObfuscateWrite(data)
		if err != nil {
			t.Fatalf("ObfuscateWrite failed: %v", err)
		}
		plaintext, err := obfs.DeobfuscateRead(obfuscated)
		if err != nil {
			t.Fatalf("DeobfuscateRead failed: %v", err)
		}
		if !bytes.Equal(plaintext, data) {
			t.Error("roundtrip mismatch")
		}
	})

	t.Run("none strategy", func(t *testing.T) {
		cfg := &contract.ObfuscationConfig{Strategy: "none"}
		obfs, err := newObfuscator(cfg)
		if err != nil {
			t.Fatalf("newObfuscator failed: %v", err)
		}
		if obfs != nil {
			t.Error("expected nil obfuscator for 'none' strategy")
		}
	})

	t.Run("empty strategy", func(t *testing.T) {
		cfg := &contract.ObfuscationConfig{Strategy: ""}
		obfs, err := newObfuscator(cfg)
		if err != nil {
			t.Fatalf("newObfuscator failed: %v", err)
		}
		if obfs != nil {
			t.Error("expected nil obfuscator for empty strategy")
		}
	})

	t.Run("unsupported strategy", func(t *testing.T) {
		cfg := &contract.ObfuscationConfig{Strategy: "unknown"}
		_, err := newObfuscator(cfg)
		if err == nil {
			t.Error("expected error for unsupported strategy")
		}
	})

	t.Run("nil config", func(t *testing.T) {
		obfs, err := newObfuscator(nil)
		if err != nil {
			t.Fatalf("newObfuscator failed: %v", err)
		}
		if obfs != nil {
			t.Error("expected nil obfuscator for nil config")
		}
	})
}

func TestObfsConnReadWrite(t *testing.T) {
	// Create a pair of connected TCP sockets for testing
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer listener.Close()

	serverCh := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("Accept failed: %v", err)
			return
		}
		serverCh <- conn
	}()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer client.Close()

	server := <-serverCh
	defer server.Close()

	obfs := &RandomPaddingObfs{minPad: 10, maxPad: 50}
	obfsClient := &obfsConn{Conn: client, strategy: obfs}
	obfsServer := &obfsConn{Conn: server, strategy: obfs}

	testData := []byte("Hello, obfuscation layer!")

	// Write from client
	doneCh := make(chan error, 1)
	go func() {
		n, err := obfsClient.Write(testData)
		if err != nil {
			doneCh <- err
			return
		}
		if n != len(testData) {
			doneCh <- io.ErrShortWrite
			return
		}
		doneCh <- nil
	}()

	// Read from server
	buf := make([]byte, len(testData))
	n, err := io.ReadFull(obfsServer, buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n != len(testData) {
		t.Errorf("Read returned %d, want %d", n, len(testData))
	}
	if !bytes.Equal(buf, testData) {
		t.Errorf("Read data mismatch: got %q, want %q", buf, testData)
	}

	// Check write completed without error
	if err := <-doneCh; err != nil {
		t.Errorf("Write failed: %v", err)
	}
}

func BenchmarkRandomPaddingObfuscate(b *testing.B) {
	obfs := &RandomPaddingObfs{minPad: 10, maxPad: 255}
	data := make([]byte, 32*1024)
	rand.Read(data)

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		_, err := obfs.ObfuscateWrite(data)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRandomPaddingDeobfuscate(b *testing.B) {
	obfs := &RandomPaddingObfs{minPad: 10, maxPad: 255}
	data := make([]byte, 32*1024)
	rand.Read(data)
	obfuscated, _ := obfs.ObfuscateWrite(data)

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		_, err := obfs.DeobfuscateRead(obfuscated)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func TestTimingPerturbObfsRoundTrip(t *testing.T) {
	obfs := &TimingPerturbObfs{minDelay: 1 * time.Millisecond, maxDelay: 10 * time.Millisecond}

	tests := []struct {
		name string
		data []byte
	}{
		{"small", []byte("test data")},
		{"medium", bytes.Repeat([]byte("x"), 1024)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			obfuscated, err := obfs.ObfuscateWrite(tt.data)
			elapsed := time.Since(start)

			if err != nil {
				t.Fatalf("ObfuscateWrite failed: %v", err)
			}

			if elapsed < obfs.minDelay {
				t.Errorf("delay %v < minDelay %v", elapsed, obfs.minDelay)
			}

			plaintext, err := obfs.DeobfuscateRead(obfuscated)
			if err != nil {
				t.Fatalf("DeobfuscateRead failed: %v", err)
			}

			if !bytes.Equal(plaintext, tt.data) {
				t.Errorf("roundtrip mismatch")
			}
		})
	}
}

func TestTimingPerturbObfsErrors(t *testing.T) {
	t.Run("invalid delay range", func(t *testing.T) {
		obfs := &TimingPerturbObfs{minDelay: 100 * time.Millisecond, maxDelay: 10 * time.Millisecond}
		_, err := obfs.ObfuscateWrite([]byte("test"))
		if err == nil {
			t.Error("expected error for invalid delay range")
		}
	})
}

func TestTLSTrafficMimicObfsRoundTrip(t *testing.T) {
	obfs := &TLSTrafficMimicObfs{}

	tests := []struct {
		name string
		data []byte
	}{
		{"small", []byte("hello")},
		{"medium", bytes.Repeat([]byte("x"), 1024)},
		{"large", bytes.Repeat([]byte("y"), 16*1024)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obfuscated, err := obfs.ObfuscateWrite(tt.data)
			if err != nil {
				t.Fatalf("ObfuscateWrite failed: %v", err)
			}

			if len(obfuscated) != len(tt.data)+5 {
				t.Errorf("obfuscated size %d != expected %d", len(obfuscated), len(tt.data)+5)
			}

			if obfuscated[0] != tlsRecordTypeAppData {
				t.Errorf("invalid TLS record type: %x", obfuscated[0])
			}

			version := binary.BigEndian.Uint16(obfuscated[1:3])
			if version != tlsVersion1_2 {
				t.Errorf("invalid TLS version: %x", version)
			}

			plaintext, err := obfs.DeobfuscateRead(obfuscated)
			if err != nil {
				t.Fatalf("DeobfuscateRead failed: %v", err)
			}

			if !bytes.Equal(plaintext, tt.data) {
				t.Errorf("roundtrip mismatch")
			}
		})
	}
}

func TestTLSTrafficMimicObfsErrors(t *testing.T) {
	obfs := &TLSTrafficMimicObfs{}

	t.Run("empty payload", func(t *testing.T) {
		_, err := obfs.ObfuscateWrite([]byte{})
		if err == nil {
			t.Error("expected error for empty payload")
		}
	})

	t.Run("truncated record", func(t *testing.T) {
		_, err := obfs.DeobfuscateRead([]byte{0x17, 0x03})
		if err == nil {
			t.Error("expected error for truncated record")
		}
	})

	t.Run("invalid record type", func(t *testing.T) {
		invalidRecord := []byte{0xFF, 0x03, 0x03, 0x00, 0x05, 0x01, 0x02, 0x03, 0x04, 0x05}
		_, err := obfs.DeobfuscateRead(invalidRecord)
		if err == nil {
			t.Error("expected error for invalid record type")
		}
	})

	t.Run("invalid length", func(t *testing.T) {
		invalidRecord := []byte{0x17, 0x03, 0x03, 0xFF, 0xFF, 0x01}
		_, err := obfs.DeobfuscateRead(invalidRecord)
		if err == nil {
			t.Error("expected error for invalid length")
		}
	})
}

func TestNewObfuscatorAllStrategies(t *testing.T) {
	t.Run("timing-perturb", func(t *testing.T) {
		cfg := &contract.ObfuscationConfig{
			Strategy: "timing-perturb",
			Params: map[string]any{
				"min_delay_ms": float64(1),
				"max_delay_ms": float64(10),
			},
		}
		obfs, err := newObfuscator(cfg)
		if err != nil {
			t.Fatalf("newObfuscator failed: %v", err)
		}
		if obfs == nil {
			t.Fatal("expected non-nil obfuscator")
		}

		data := []byte("test")
		obfuscated, err := obfs.ObfuscateWrite(data)
		if err != nil {
			t.Fatalf("ObfuscateWrite failed: %v", err)
		}
		plaintext, err := obfs.DeobfuscateRead(obfuscated)
		if err != nil {
			t.Fatalf("DeobfuscateRead failed: %v", err)
		}
		if !bytes.Equal(plaintext, data) {
			t.Error("roundtrip mismatch")
		}
	})

	t.Run("tls-mimic", func(t *testing.T) {
		cfg := &contract.ObfuscationConfig{Strategy: "tls-mimic"}
		obfs, err := newObfuscator(cfg)
		if err != nil {
			t.Fatalf("newObfuscator failed: %v", err)
		}
		if obfs == nil {
			t.Fatal("expected non-nil obfuscator")
		}

		data := []byte("test data")
		obfuscated, err := obfs.ObfuscateWrite(data)
		if err != nil {
			t.Fatalf("ObfuscateWrite failed: %v", err)
		}
		plaintext, err := obfs.DeobfuscateRead(obfuscated)
		if err != nil {
			t.Fatalf("DeobfuscateRead failed: %v", err)
		}
		if !bytes.Equal(plaintext, data) {
			t.Error("roundtrip mismatch")
		}
	})

	t.Run("invalid timing-perturb params", func(t *testing.T) {
		cfg := &contract.ObfuscationConfig{
			Strategy: "timing-perturb",
			Params: map[string]any{
				"min_delay_ms": float64(100),
				"max_delay_ms": float64(10),
			},
		}
		_, err := newObfuscator(cfg)
		if err == nil {
			t.Error("expected error for invalid delay range")
		}
	})
}
