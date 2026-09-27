package commerce

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func validNotificationURL(raw, format string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if format == "telegram" {
		return validTelegramURL(u)
	}
	if !validWebhookURL(raw) {
		return false
	}
	switch format {
	case "webhook":
		return true
	case "feishu":
		return u.Host == "open.feishu.cn" && strings.HasPrefix(u.Path, "/open-apis/bot/v2/hook/")
	case "discord":
		return u.Host == "discord.com" && strings.HasPrefix(u.Path, "/api/webhooks/")
	}
	return false
}

func validTelegramURL(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.Hostname() != "api.telegram.org" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	const suffix = "/sendMessage"
	if !strings.HasPrefix(u.Path, "/bot") || !strings.HasSuffix(u.Path, suffix) {
		return false
	}
	token := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/bot"), suffix)
	parts := strings.SplitN(token, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	for _, r := range parts[0] {
		if r < '0' || r > '9' {
			return false
		}
	}
	for _, r := range parts[1] {
		if !(r == '_' || r == '-' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	values := u.Query()
	chatIDs, ok := values["chat_id"]
	if !ok || len(values) != 1 || len(chatIDs) != 1 || strings.TrimSpace(chatIDs[0]) == "" || len(chatIDs[0]) > 128 {
		return false
	}
	return true
}

func notificationListURL(raw, format string) string {
	if format != "telegram" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "Telegram 机器人"
	}
	chatID := u.Query().Get("chat_id")
	if chatID == "" {
		return "https://api.telegram.org/botREDACTED/sendMessage"
	}
	return "https://api.telegram.org/botREDACTED/sendMessage?chat_id=" + url.QueryEscape(chatID)
}

func notificationBody(format, payload, event string) ([]byte, error) {
	if format == "" || format == "webhook" {
		return []byte(payload), nil
	}
	var v struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(payload), &v); err != nil {
		return nil, err
	}
	// Avoid exposing raw financial payloads, addresses, or credentials in chat.
	text := "流量控制台通知 · " + event + "\n事件编号：" + v.ID + "\n请登录面板查看详情。"
	switch format {
	case "feishu":
		return json.Marshal(map[string]any{"msg_type": "text", "content": map[string]string{"text": text}})
	case "discord":
		return json.Marshal(map[string]any{"content": text, "allowed_mentions": map[string]any{"parse": []string{}}})
	case "telegram":
		return json.Marshal(map[string]any{"text": text, "disable_web_page_preview": true})
	}
	return nil, errors.New("unknown notification format")
}
func notificationResponse(format string, res *http.Response) error {
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return errors.New("notification HTTP failure")
	}
	if format == "feishu" {
		var v struct {
			Code   *int `json:"code"`
			Status *int `json:"StatusCode"`
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, 65536)).Decode(&v); err != nil {
			return err
		}
		if v.Code != nil && *v.Code == 0 || v.Status != nil && *v.Status == 0 {
			return nil
		}
		return errors.New("Feishu rejected notification")
	}
	if format == "telegram" {
		var v struct {
			OK          bool   `json:"ok"`
			Description string `json:"description"`
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, 65536)).Decode(&v); err != nil {
			return err
		}
		if !v.OK {
			return errors.New("Telegram rejected notification")
		}
	}
	return nil
}
