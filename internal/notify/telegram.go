package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Telegram struct {
	BotToken string
	ChatID   string
}

func (t Telegram) Send(text string) error {
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.PostForm("https://api.telegram.org/bot"+t.BotToken+"/sendMessage", url.Values{"chat_id": {t.ChatID}, "text": {text}})
	if err != nil {
		return errors.New(strings.ReplaceAll(err.Error(), t.BotToken, "[redacted]"))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Telegram HTTP %d", response.StatusCode)
	}
	var result struct {
		OK bool `json:"ok"`
	}
	d := json.NewDecoder(response.Body)
	if err := d.Decode(&result); err != nil {
		return fmt.Errorf("Telegram JSON: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("unexpected data after Telegram JSON")
	}
	if !result.OK {
		return errors.New("Telegram returned ok=false")
	}
	return nil
}
