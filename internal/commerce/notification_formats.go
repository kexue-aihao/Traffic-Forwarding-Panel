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
	if !validWebhookURL(raw) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
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
	return nil
}
