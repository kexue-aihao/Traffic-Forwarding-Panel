package agent

import (
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

// Includes real WAL fsync. Acknowledgements between bounded rounds are outside
// the timer so longer runs do not hit the production spool limit.
func BenchmarkChargeLatency(b *testing.B) {
	for _, workers := range []int{1, 32} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			s, err := OpenStore(filepath.Join(b.TempDir(), "state.json"))
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			if err := s.SetIdentity(contract.Registered{NodeID: "bench", Token: "test"}); err != nil {
				b.Fatal(err)
			}
			rule := testRule("127.0.0.1:1")
			rule.Lease.Bytes = 1 << 60
			rule.Lease.ExpiresAt = time.Now().Add(time.Hour)
			until := rule.Lease.ExpiresAt
			latencies := make([]int64, b.N)
			var syncStart time.Time
			var syncTime time.Duration
			var syncs int
			hook := func(point string) error {
				if point == "sync_before" {
					syncStart = time.Now()
				} else if point == "sync_after" {
					syncTime += time.Since(syncStart)
					syncs++
				}
				return nil
			}
			b.SetBytes(32 * 1024)
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for base := 0; base < b.N; base += 1024 {
				end := min(base+1024, b.N)
				s.mu.Lock()
				s.fault = hook
				s.mu.Unlock()
				var index atomic.Int64
				index.Store(int64(base))
				var wg sync.WaitGroup
				b.StartTimer()
				for w := 0; w < min(workers, end-base); w++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for {
							i := int(index.Add(1)) - 1
							if i >= end {
								return
							}
							start := time.Now()
							if err := s.Charge(rule, until, true, 32*1024); err != nil {
								b.Error(err)
								return
							}
							latencies[i] = time.Since(start).Nanoseconds()
						}
					}()
				}
				wg.Wait()
				b.StopTimer()
				s.mu.Lock()
				s.fault = nil
				s.mu.Unlock()
				pending := s.Pending()
				ids := make([]string, len(pending))
				for i, record := range pending {
					ids[i] = record.ID
				}
				if err := s.Confirm(ids); err != nil {
					b.Fatal(err)
				}
			}
			slices.Sort(latencies)
			b.ReportMetric(float64(latencies[(b.N-1)*50/100])/1000, "p50-us")
			b.ReportMetric(float64(latencies[(b.N-1)*95/100])/1000, "p95-us")
			if syncs > 0 {
				b.ReportMetric(float64(syncTime.Nanoseconds())/float64(syncs)/1000, "fsync-us")
				b.ReportMetric(float64(b.N)/float64(syncs), "charges/sync")
			}
		})
	}
}
