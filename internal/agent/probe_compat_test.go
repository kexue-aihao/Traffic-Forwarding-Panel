package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestProbeBootTimeFallsBackForLegacyPanelAndRetriesAfterUpgrade(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	legacy, requests, accepted := true, 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests++
		var p map[string]any
		if err := json.NewDecoder(req.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		if p["uptime_seconds"] != "42" {
			t.Error("fallback changed uptime", p)
		}
		if legacy && p["booted_at"] != nil {
			w.WriteHeader(400)
			return
		}
		accepted++
		w.WriteHeader(204)
	}))
	defer srv.Close()
	a := Agent{URL: srv.URL, HTTP: srv.Client(), Store: s}
	boot := time.Now().UTC().Add(-42 * time.Second)
	seconds := uint64(42)
	p := contract.Probe{NodeID: "n", SampledAt: time.Now().UTC(), BootedAt: &boot, UptimeSeconds: &seconds}
	for range 2 {
		if err := a.sendProbe(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	if requests != 3 || accepted != 2 || a.probeBootRetry.IsZero() || p.BootedAt == nil {
		t.Fatal("legacy fallback was not cached or mutated the original sample", requests, accepted)
	}
	a.probeMu.Lock()
	a.probeBootRetry = time.Now().Add(-time.Second)
	a.probeMu.Unlock()
	legacy = false
	if err := a.sendProbe(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if requests != 4 || accepted != 3 {
		t.Fatal("upgraded panel was not probed again")
	}
}

func TestProbeBootTimeDoesNotMaskAuthenticationOrServerFailures(t *testing.T) {
	for _, code := range []int{400, 401, 403, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			s, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { requests++; w.WriteHeader(code) }))
			defer srv.Close()
			a := Agent{URL: srv.URL, HTTP: srv.Client(), Store: s}
			boot := time.Now().UTC()
			if err := a.sendProbe(context.Background(), contract.Probe{BootedAt: &boot}); err == nil {
				t.Fatal("probe failure was hidden")
			}
			want := 1
			if code == 400 {
				want = 2
			}
			if requests != want || !a.probeBootRetry.IsZero() {
				t.Fatal("failed fallback was cached or forbidden request retried", requests)
			}
		})
	}
}
