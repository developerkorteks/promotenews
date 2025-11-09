package sender

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"

	"promote/internal/storage"
	"promote/internal/wa"
)

type MessageContent struct {
	Text      string   `json:"text"`
	ImageURLs []string `json:"image_urls"`
	VideoURLs []string `json:"video_urls"`
}

type Sender struct {
	Store   *storage.Store
	Manager *wa.Manager
	Client  *http.Client
}

func New(store *storage.Store, manager *wa.Manager) *Sender {
	return &Sender{
		Store:   store,
		Manager: manager,
		Client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// Retry/backoff & risk configuration
var (
	maxAttempts   = 3
	baseBackoff   = 2 * time.Second
	maxBackoff    = 20 * time.Second
	jitterPct     = 0.20
	riskThreshold = 3
)

type httpStatusError struct {
	code int
	url  string
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("fetch %s: status %d", e.url, e.code) }

func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*httpStatusError); ok {
		if e.code == 429 || (e.code >= 500 && e.code <= 599) {
			return true
		}
		return false
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "timeout"),
		strings.Contains(s, "temporary"),
		strings.Contains(s, "eof"),
		strings.Contains(s, "reset"),
		strings.Contains(s, "deadline"):
		return true
	default:
		return false
	}
}

func withRetry(ctx context.Context, fn func() error) error {
	attempt := 0
	backoff := baseBackoff
	for {
		err := fn()
		if err == nil {
			return nil
		}
		attempt++
		if attempt >= maxAttempts || !isRetryable(err) {
			return err
		}
		// exponential backoff with jitter
		jit := time.Duration(rand.Int63n(int64(float64(backoff) * jitterPct)))
		wait := backoff + jit
		if wait > maxBackoff {
			wait = maxBackoff
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func sleepRange(ctx context.Context, min, max time.Duration) error {
	if max <= min {
		select {
		case <-time.After(min):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	delta := max - min
	wait := min + time.Duration(rand.Int63n(int64(delta)))
	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Sender) bumpRiskAndMaybePause(groupID string) {
	_, _ = s.Store.DB.Exec(`UPDATE groups SET risk_score = risk_score + 1 WHERE id=?`, groupID)
	_, _ = s.Store.DB.Exec(`UPDATE groups SET enabled=0 WHERE id=? AND risk_score >= ?`, groupID, riskThreshold)
}

// SendToGroup sends content to a group JID string like "12345-67890@g.us" via a specific account.
// It personalizes "{group_name}" placeholder when available.
func (s *Sender) SendToGroup(ctx context.Context, accountID, groupJID string, content MessageContent) error {
	cli, err := s.Manager.GetClient(accountID)
	if err != nil {
		return err
	}
	if cli.Store == nil || cli.Store.ID == nil {
		return fmt.Errorf("account %s not paired/connected", accountID)
	}

	// Parse JID
	jid, err := types.ParseJID(groupJID)
	if err != nil {
		return fmt.Errorf("parse JID: %w", err)
	}

	// Load group name for personalization
	groupName := s.lookupGroupName(groupJID)

	// 1) Send text if provided (with retry/backoff)
	if strings.TrimSpace(content.Text) != "" {
		text := personalize(content.Text, groupName)
		err := withRetry(ctx, func() error {
			return s.sendText(ctx, cli, jid, text)
		})
		if err != nil {
			_ = s.logResult(accountID, groupJID, "", "failed", short(text), err.Error(), maxAttempts, time.Now())
			s.bumpRiskAndMaybePause(groupJID)
			log.Printf("[sender] text failed account=%s group=%s err=%v", accountID, groupJID, err)
			return err
		}
		_ = s.logResult(accountID, groupJID, "", "sent", short(content.Text), "", 1, time.Now())
		// small human-like pause between parts
		if err := sleepRange(ctx, 1*time.Second, 2*time.Second); err != nil {
			return err
		}
	}

	// 2) Send images (with retry/backoff per media)
	for idx, u := range content.ImageURLs {
		caption := personalize(content.Text, groupName)
		err := withRetry(ctx, func() error {
			return s.sendImageByURL(ctx, cli, jid, u, caption)
		})
		if err != nil {
			_ = s.logResult(accountID, groupJID, "", "failed", "image:"+u, err.Error(), idx+1, time.Now())
			s.bumpRiskAndMaybePause(groupJID)
			log.Printf("[sender] image failed account=%s group=%s url=%s err=%v", accountID, groupJID, u, err)
			return err
		}
		_ = s.logResult(accountID, groupJID, "", "sent", "image:"+u, "", idx+1, time.Now())
		// pacing
		if err := sleepRange(ctx, 1200*time.Millisecond, 2500*time.Millisecond); err != nil {
			return err
		}
	}

	// 3) Send videos (with retry/backoff per media)
	for idx, u := range content.VideoURLs {
		caption := personalize(content.Text, groupName)
		err := withRetry(ctx, func() error {
			return s.sendVideoByURL(ctx, cli, jid, u, caption)
		})
		if err != nil {
			_ = s.logResult(accountID, groupJID, "", "failed", "video:"+u, err.Error(), idx+1, time.Now())
			s.bumpRiskAndMaybePause(groupJID)
			log.Printf("[sender] video failed account=%s group=%s url=%s err=%v", accountID, groupJID, u, err)
			return err
		}
		_ = s.logResult(accountID, groupJID, "", "sent", "video:"+u, "", idx+1, time.Now())
		if err := sleepRange(ctx, 1500*time.Millisecond, 3000*time.Millisecond); err != nil {
			return err
		}
	}

	// update last_sent_at for cadence enforcement
	_, _ = s.Store.DB.Exec(`UPDATE groups SET last_sent_at=CURRENT_TIMESTAMP WHERE id=?`, groupJID)
	return nil
}

func (s *Sender) sendText(ctx context.Context, c *whatsmeow.Client, jid types.JID, text string) error {
	msg := &proto.Message{Conversation: strptr(text)}
	_, err := c.SendMessage(ctx, jid, msg)
	return err
}

func (s *Sender) sendImageByURL(ctx context.Context, c *whatsmeow.Client, jid types.JID, url, caption string) error {
	data, mime, err := s.fetch(ctx, url)
	if err != nil {
		return err
	}
	up, err := c.Upload(ctx, data, whatsmeow.MediaImage)
	if err != nil {
		return fmt.Errorf("upload image: %w", err)
	}
	length := uint64(len(data))
	img := &proto.ImageMessage{
		Caption:       optstr(caption),
		Mimetype:      optstr(mime),
		URL:           optstr(up.URL),
		DirectPath:    optstr(up.DirectPath),
		MediaKey:      up.MediaKey,
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    &length,
	}
	msg := &proto.Message{ImageMessage: img}
	_, err = c.SendMessage(ctx, jid, msg)
	return err
}

func (s *Sender) sendVideoByURL(ctx context.Context, c *whatsmeow.Client, jid types.JID, url, caption string) error {
	data, mime, err := s.fetch(ctx, url)
	if err != nil {
		return err
	}
	up, err := c.Upload(ctx, data, whatsmeow.MediaVideo)
	if err != nil {
		return fmt.Errorf("upload video: %w", err)
	}
	length := uint64(len(data))
	vid := &proto.VideoMessage{
		Caption:       optstr(caption),
		Mimetype:      optstr(mime),
		URL:           optstr(up.URL),
		DirectPath:    optstr(up.DirectPath),
		MediaKey:      up.MediaKey,
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    &length,
	}
	msg := &proto.Message{VideoMessage: vid}
	_, err = c.SendMessage(ctx, jid, msg)
	return err
}

func (s *Sender) fetch(ctx context.Context, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	res, err := s.Client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// return typed error for retry classification
		_, _ = io.Copy(io.Discard, res.Body)
		return nil, "", &httpStatusError{code: res.StatusCode, url: url}
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, "", err
	}
	ct := res.Header.Get("Content-Type")
	if ct == "" {
		// naive fallback
		if strings.Contains(strings.ToLower(url), ".jpg") || strings.Contains(strings.ToLower(url), ".jpeg") {
			ct = "image/jpeg"
		} else if strings.Contains(strings.ToLower(url), ".png") {
			ct = "image/png"
		} else if strings.Contains(strings.ToLower(url), ".mp4") {
			ct = "video/mp4"
		} else {
			ct = "application/octet-stream"
		}
	}
	return body, ct, nil
}

func (s *Sender) logResult(accountID, groupID, campaignID, status, preview, errMsg string, attempt int, scheduled time.Time) error {
	_, err := s.Store.DB.Exec(`INSERT INTO logs (account_id,group_id,campaign_id,status,error,message_preview,attempt,scheduled_for) 
	VALUES (?,?,?,?,?,?,?,?)`,
		accountID, groupID, nullIfEmpty(campaignID), status, errMsg, preview, attempt, scheduled)
	return err
}

func (s *Sender) lookupGroupName(groupID string) string {
	var name sql.NullString
	_ = s.Store.DB.QueryRow(`SELECT name FROM groups WHERE id=?`, groupID).Scan(&name)
	if name.Valid {
		return name.String
	}
	return ""
}

func personalize(text, groupName string) string {
	if text == "" {
		return text
	}
	return strings.ReplaceAll(text, "{group_name}", groupName)
}

func short(s string) string {
	if len(s) <= 128 {
		return s
	}
	return s[:128]
}

func strptr(s string) *string { return &s }
func optstr(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
