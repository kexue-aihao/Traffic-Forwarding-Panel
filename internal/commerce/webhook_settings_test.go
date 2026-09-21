package commerce

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWebhookMuteSuppressesPendingAndInflightRetry(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	s.Now = func() time.Time { return now }
	hook, _, err := s.CreateWebhook(ctx, "alice", "https://hooks.example.test/events", []string{"*"})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	released := false

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; w.WriteHeader(503) }))
	defer func() {
		if !released {
			close(release)
		}
		server.Close()
	}()
	s.webhookClient = server.Client()
	if _, err = s.DB.Exec(s.q("UPDATE commerce_webhook_subscriptions SET url=? WHERE id=?"), server.URL, hook.ID); err != nil {
		t.Fatal(err)
	}
	emit := func() {
		t.Helper()
		if err := s.Write(ctx, func(tx *sql.Tx) error {
			return s.EmitEventTx(ctx, tx, "alice", "node.offline", map[string]any{"node_id": "node"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	emit()
	done := make(chan error, 1)
	go func() { _, err := s.RunWebhookDelivery(ctx, 10); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery not started")
	}
	until := now.Add(time.Hour)
	settings := WebhookSettings{Events: []string{"*"}, Enabled: true, MutedUntil: &until, Version: 1}
	if err = s.UpdateWebhook(ctx, "alice", hook.ID, settings); err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateWebhook(ctx, "alice", hook.ID, settings); !errors.Is(err, ErrConflict) {
		t.Fatal("stale hook settings", err)
	}
	if err = s.UpdateWebhook(ctx, "other", hook.ID, settings); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("other owner changed settings", err)
	}
	close(release)
	released = true
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	emit()
	list, err := s.WebhookDeliveries(ctx, "alice")
	if err != nil || len(list) != 1 || list[0]["status"] != "suppressed" {
		t.Fatal("late failure revived suppressed delivery", list, err)
	}
	settings.Version = 2
	settings.MutedUntil = nil
	if err = s.UpdateWebhook(ctx, "alice", hook.ID, settings); err != nil {
		t.Fatal(err)
	}
	if sent, err := s.RunWebhookDelivery(ctx, 10); err != nil || sent != 0 {
		t.Fatal("unmuting sent backlog", sent, err)
	}
	hooks, err := s.ListWebhooks(ctx, "alice")
	if err != nil || hooks[0].Version != 3 || hooks[0].MutedUntil != nil {
		t.Fatal(hooks, err)
	}
	settings.Version = 3
	settings.Events = []string{"wallet.recharge"}
	if err = s.UpdateWebhook(ctx, "alice", hook.ID, settings); err != nil {
		t.Fatal(err)
	}
	emit()
	list, err = s.WebhookDeliveries(ctx, "alice")
	if err != nil || len(list) != 1 {
		t.Fatal("event filter ignored", list, err)
	}
}
