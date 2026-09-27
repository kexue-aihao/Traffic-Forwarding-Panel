package commerce

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type notificationTransport func(*http.Request) (*http.Response, error)

func (f notificationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTelegramNotificationURLValidation(t *testing.T) {
	valid := "https://api.telegram.org/bot123456789:ABCDEFGHIJKLMNOPQRST/sendMessage?chat_id=-1001234567890"
	for _, raw := range []string{
		valid,
		"https://api.telegram.org/bot123456789:ABCDEFGHIJKLMNOPQRST/sendMessage?chat_id=%40example_channel",
	} {
		if !validNotificationURL(raw, "telegram") {
			t.Fatalf("expected Telegram URL to be accepted: %s", raw)
		}
	}
	for _, raw := range []string{
		"https://api.telegram.org/bot123456789:ABCDEFGHIJKLMNOPQRST/sendMessage",
		"https://api.telegram.org/bot123456789:ABCDEFGHIJKLMNOPQRST/sendMessage?chat_id=1&extra=x",
		"https://api.telegram.org/bot123456789:ABCDEFGHIJKLMNOPQRST/getMe?chat_id=1",
		"https://example.com/bot123456789:ABCDEFGHIJKLMNOPQRST/sendMessage?chat_id=1",
	} {
		if validNotificationURL(raw, "telegram") {
			t.Fatalf("expected Telegram URL to be rejected: %s", raw)
		}
	}
}

func TestNotificationFormatsRetryAndPrivacy(t *testing.T) {
	for _, format := range []string{"feishu", "discord", "telegram"} {
		t.Run(format, func(t *testing.T) {
			s := fixture(t)
			ctx := context.Background()
			url := "https://open.feishu.cn/open-apis/bot/v2/hook/test"
			if format == "discord" {
				url = "https://discord.com/api/webhooks/1/token"
			} else if format == "telegram" {
				url = "https://api.telegram.org/bot123456789:ABCDEFGHIJKLMNOPQRST/sendMessage?chat_id=-1001234567890"
			}
			sub, _, err := s.CreateNotification(ctx, "u", url, []string{"*"}, false, format)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Write(ctx, func(tx *sql.Tx) error {
				return s.emitEventTx(ctx, tx, "u", "wallet.recharge", map[string]any{"secret": "private-value", "amount_cents": "9007199254740993"})
			}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			s.webhookClient = &http.Client{Transport: notificationTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				body, e := io.ReadAll(r.Body)
				if e != nil {
					t.Fatal(e)
				}
				if strings.Contains(string(body), "private-value") || strings.Contains(string(body), "9007199254740993") {
					t.Fatal("chat leaked raw payload")
				}
				var parsed map[string]any
				if json.Unmarshal(body, &parsed) != nil {
					t.Fatal(string(body))
				}
				if format == "discord" && parsed["allowed_mentions"] == nil {
					t.Fatal("mentions not suppressed")
				}
				if format == "telegram" && parsed["disable_web_page_preview"] != true {
					t.Fatal("telegram link previews not disabled")
				}
				if format == "telegram" && r.URL.Query().Get("chat_id") != "-1001234567890" {
					t.Fatalf("unexpected Telegram chat ID: %s", r.URL.Query().Get("chat_id"))
				}
				code := 200
				result := `{"code":0}`
				if format == "telegram" {
					result = `{"ok":false,"description":"retry"}`
				}
				if calls == 1 {
					if format == "feishu" {
						result = `{"code":19021}`
					} else if format == "telegram" {
						result = `{"ok":false,"description":"retry"}`
					} else {
						code = 429
					}
				} else if format == "telegram" {
					result = `{"ok":true}`
				}
				return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(result)), Header: make(http.Header)}, nil
			})}
			if count, e := s.RunWebhookDelivery(ctx, 20); e != nil || count != 1 {
				t.Fatal(count, e)
			}
			rows, e := s.WebhookDeliveries(ctx, "u")
			if e != nil || rows[0]["status"] == "delivered" {
				t.Fatal(rows, e)
			}
			now := s.Now()
			s.Now = func() time.Time { return now.Add(time.Minute) }
			if _, e = s.RunWebhookDelivery(ctx, 20); e != nil {
				t.Fatal(e)
			}
			rows, e = s.WebhookDeliveries(ctx, "u")
			if e != nil || rows[0]["status"] != "delivered" || calls != 2 {
				t.Fatal(rows, calls, e)
			}
			list, e := s.ListWebhooks(ctx, "u")
			if e != nil || len(list) != 1 || list[0].Format != format || list[0].ID != sub.ID {
				t.Fatal(list, e)
			}
			if format == "telegram" && strings.Contains(list[0].URL, "ABCDEFGHIJKLMNOPQRST") {
				t.Fatal("Telegram bot token leaked from subscription listing")
			}
		})
	}
}
