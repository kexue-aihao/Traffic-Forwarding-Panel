package platform

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/testdb"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// BenchmarkUsageCapacity runs a real HTTP/auth/SQL/lease-accounting hot path
// against a database with 500 nodes, 10k users and 100k rules. It is intentionally
// opt-in (go test -run '^$' -bench BenchmarkUsageCapacity -benchtime=10x).
// This measures one 200-record batch, not full production capacity or SLA.
func BenchmarkUsageCapacity(b *testing.B) {
	s := New(testdb.Open(b), Options{})
	ctx := context.Background()
	start := time.Now()
	write := func(fn func(*sql.Tx) error) {
		b.Helper()
		if e := s.Store.Write(ctx, storage.Normal, fn); e != nil {
			b.Fatal(e)
		}
	}
	write(func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, s.q(`INSERT INTO cp_groups(id,name,payload,version) VALUES(?,?,?,1)`), "bench-group", "bench", strJSON(contract.Group{ID: "bench-group", Name: "bench", PortMin: 1, PortMax: 65535}))
		return e
	})
	for offset := 0; offset < 10000; offset += 500 {
		write(func(tx *sql.Tx) error {
			stmt, e := tx.PrepareContext(ctx, s.q(`INSERT INTO cp_users(id,username,password_hash,role,disabled) VALUES(?,?,?,'user',0)`))
			if e != nil {
				return e
			}
			defer stmt.Close()
			for i := offset; i < offset+500; i++ {
				uid := fmt.Sprintf("u%05d", i)
				if _, e = stmt.ExecContext(ctx, uid, uid, "benchmark-nonlogin"); e != nil {
					return e
				}
			}
			return nil
		})
	}
	write(func(tx *sql.Tx) error {
		stmt, e := tx.PrepareContext(ctx, s.q(`INSERT INTO cp_nodes(id,name,token_hash,payload,desired_version,applied_version,apply_error,last_seen) VALUES(?,?,?,?,1,0,'',0)`))
		if e != nil {
			return e
		}
		defer stmt.Close()
		for i := 0; i < 500; i++ {
			nid := fmt.Sprintf("n%03d", i)
			if _, e = stmt.ExecContext(ctx, nid, nid, digest(nid), strJSON(contract.Node{ID: nid})); e != nil {
				return e
			}
		}
		return nil
	})
	expiry := time.Now().UTC().Add(24 * time.Hour)
	for offset := 0; offset < 100000; offset += 500 {
		write(func(tx *sql.Tx) error {
			ruleStmt, e := tx.PrepareContext(ctx, s.q(`INSERT INTO cp_rules(id,user_id,node_id,group_id,payload,version,deleted,release_version) VALUES(?,?,?,'bench-group',?,1,0,0)`))
			if e != nil {
				return e
			}
			defer ruleStmt.Close()
			leaseStmt, e := tx.PrepareContext(ctx, s.q(`INSERT INTO cp_rule_leases(id,rule_id,node_id,entitlement_id,bytes_allocated,bytes_used,expires_at) VALUES(?,?,?,'admin-test',?,0,?)`))
			if e != nil {
				return e
			}
			defer leaseStmt.Close()
			for i := offset; i < offset+500; i++ {
				rid := fmt.Sprintf("r%06d", i)
				uid := fmt.Sprintf("u%05d", i%10000)
				nid := fmt.Sprintf("n%03d", i/200)
				lease := &contract.Lease{ID: "l" + rid, EntitlementID: "admin-test", ExpiresAt: expiry, Bytes: 1 << 50}
				rule := contract.Rule{ID: rid, UserID: uid, NodeID: nid, GroupID: "bench-group", Name: rid, Network: "tcp", Transport: "direct", Listen: fmt.Sprintf("0.0.0.0:%d", 10000+i%200), Target: "127.0.0.1:8080", Enabled: true, Version: 1, Lease: lease}
				if _, e = ruleStmt.ExecContext(ctx, rid, uid, nid, strJSON(rule)); e != nil {
					return e
				}
				if _, e = leaseStmt.ExecContext(ctx, lease.ID, rid, nid, lease.Bytes, expiry.Unix()); e != nil {
					return e
				}
			}
			return nil
		})
	}
	b.Logf("seeded 500 nodes/10000 users/100000 rules in %s; synthetic leases isolate control-plane cost, commercial settlement excluded", time.Since(start))
	mux := http.NewServeMux()
	s.Register(mux)
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		nodeIndex := iteration % 500
		nid := fmt.Sprintf("n%03d", nodeIndex)
		batch := contract.UsageBatch{Records: make([]contract.UsageRecord, 200)}
		now := time.Now().UTC()
		for j := range batch.Records {
			rid := fmt.Sprintf("r%06d", nodeIndex*200+j)
			batch.Records[j] = contract.UsageRecord{ID: fmt.Sprintf("event-%d-%d", iteration, j), NodeID: nid, RuleID: rid, LeaseID: "l" + rid, EntitlementID: "admin-test", StartedAt: now.Add(-time.Second), EndedAt: now, UploadBytes: 512, DownloadBytes: 512}
		}
		payload, e := json.Marshal(batch)
		if e != nil {
			b.Fatal(e)
		}
		req := httptest.NewRequest("POST", "http://bench/api/v1/agent/usage", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+nid)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != 200 {
			b.Fatal(rr.Code, rr.Body.String())
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N*200)/b.Elapsed().Seconds(), "records/s")
}
