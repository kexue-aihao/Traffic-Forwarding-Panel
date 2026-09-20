// Package testdb creates disposable SQL databases for integration tests.
package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

// Open defaults to SQLite. TFP_TEST_DRIVER and TFP_TEST_DSN select a server
// instance where this test identity may create/drop its own random database.
// Existing databases/tables are never reset or reused by the test.
func Open(t testing.TB) *storage.Store {
	t.Helper()
	driver := os.Getenv("TFP_TEST_DRIVER")
	dsn := os.Getenv("TFP_TEST_DSN")
	if driver == "" || driver == "sqlite" {
		s, e := storage.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "test.db"))
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	if dsn == "" {
		t.Fatal("TFP_TEST_DSN required with TFP_TEST_DRIVER")
	}
	var random [12]byte
	if _, e := rand.Read(random[:]); e != nil {
		t.Fatal(e)
	}
	name := "tfp_test_" + hex.EncodeToString(random[:])
	var adminDriver, testDSN string
	switch driver {
	case "postgres":
		adminDriver = "pgx"
		u, e := url.Parse(dsn)
		if e != nil {
			t.Fatal(e)
		}
		if u.Scheme != "postgres" && u.Scheme != "postgresql" {
			t.Fatal("test PostgreSQL DSN must be URL")
		}
		u.Path = "/" + name
		testDSN = u.String()
	case "mysql":
		adminDriver = "mysql"
		cfg, e := mysql.ParseDSN(dsn)
		if e != nil {
			t.Fatal(e)
		}
		cfg.DBName = name
		testDSN = cfg.FormatDSN()
	default:
		t.Fatalf("unsupported test driver %q", driver)
	}
	admin, e := sql.Open(adminDriver, dsn)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = admin.Exec("CREATE DATABASE " + name); e != nil {
		admin.Close()
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE " + name); err != nil {
			t.Errorf("drop disposable database: %v", err)
		}
		admin.Close()
	})
	s, e := storage.Open(context.Background(), driver, testDSN)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
