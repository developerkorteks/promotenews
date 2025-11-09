package sender

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"

	"promote/internal/storage"
	"promote/internal/wa"
)

type MessageContent struct {
	Text        string   `json:"text"`
	ImageURLs   []string `json:"image_urls"`
	VideoURLs   []string `json:"video_urls"`
	AudioURLs   []string `json:"audio_urls"`
	StickerURLs []string `json:"sticker_urls"`
	DocURLs     []string `json:"doc_urls"`
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

	// 4) Send audios (with retry/backoff per media)
	for idx, u := range content.AudioURLs {
		err := withRetry(ctx, func() error {
			return s.sendAudioByURL(ctx, cli, jid, u)
		})
		if err != nil {
			_ = s.logResult(accountID, groupJID, "", "failed", "audio:"+u, err.Error(), idx+1, time.Now())
			s.bumpRiskAndMaybePause(groupJID)
			log.Printf("[sender] audio failed account=%s group=%s url=%s err=%v", accountID, groupJID, u, err)
			return err
		}
		_ = s.logResult(accountID, groupJID, "", "sent", "audio:"+u, "", idx+1, time.Now())
		// pacing
		if err := sleepRange(ctx, 1200*time.Millisecond, 2500*time.Millisecond); err != nil {
			return err
		}
	}

	// 5) Send stickers (with retry/backoff per media)
	for idx, u := range content.StickerURLs {
		err := withRetry(ctx, func() error {
			return s.sendStickerByURL(ctx, cli, jid, u)
		})
		if err != nil {
			_ = s.logResult(accountID, groupJID, "", "failed", "sticker:"+u, err.Error(), idx+1, time.Now())
			s.bumpRiskAndMaybePause(groupJID)
			log.Printf("[sender] sticker failed account=%s group=%s url=%s err=%v", accountID, groupJID, u, err)
			return err
		}
		_ = s.logResult(accountID, groupJID, "", "sent", "sticker:"+u, "", idx+1, time.Now())
		// pacing
		if err := sleepRange(ctx, 1200*time.Millisecond, 2500*time.Millisecond); err != nil {
			return err
		}
	}

	// 6) Send documents (with retry/backoff per media)
	for idx, u := range content.DocURLs {
		caption := personalize(content.Text, groupName)
		err := withRetry(ctx, func() error {
			return s.sendDocumentByURL(ctx, cli, jid, u, caption)
		})
		if err != nil {
			_ = s.logResult(accountID, groupJID, "", "failed", "doc:"+u, err.Error(), idx+1, time.Now())
			s.bumpRiskAndMaybePause(groupJID)
			log.Printf("[sender] document failed account=%s group=%s url=%s err=%v", accountID, groupJID, u, err)
			return err
		}
		_ = s.logResult(accountID, groupJID, "", "sent", "doc:"+u, "", idx+1, time.Now())
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

func (s *Sender) sendAudioByURL(ctx context.Context, c *whatsmeow.Client, jid types.JID, url string) error {
	data, mime, err := s.fetch(ctx, url)
	if err != nil {
		return err
	}
	up, err := c.Upload(ctx, data, whatsmeow.MediaAudio)
	if err != nil {
		return fmt.Errorf("upload audio: %w", err)
	}
	length := uint64(len(data))
	am := &proto.AudioMessage{
		Mimetype:      optstr(mime),
		URL:           optstr(up.URL),
		DirectPath:    optstr(up.DirectPath),
		MediaKey:      up.MediaKey,
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    &length,
		// Ptt: proto.Bool(true), // uncomment if you want voice note style
	}
	msg := &proto.Message{AudioMessage: am}
	_, err = c.SendMessage(ctx, jid, msg)
	return err
}

func (s *Sender) sendStickerByURL(ctx context.Context, c *whatsmeow.Client, jid types.JID, url string) error {
	data, mime, err := s.fetch(ctx, url)
	if err != nil {
		return err
	}
	up, err := c.Upload(ctx, data, whatsmeow.MediaImage)
	if err != nil {
		return fmt.Errorf("upload sticker: %w", err)
	}
	length := uint64(len(data))
	st := &proto.StickerMessage{
		Mimetype:      optstr(mime),
		URL:           optstr(up.URL),
		DirectPath:    optstr(up.DirectPath),
		MediaKey:      up.MediaKey,
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    &length,
	}
	msg := &proto.Message{StickerMessage: st}
	_, err = c.SendMessage(ctx, jid, msg)
	return err
}

func (s *Sender) sendDocumentByURL(ctx context.Context, c *whatsmeow.Client, jid types.JID, url, caption string) error {
	data, mime, err := s.fetch(ctx, url)
	if err != nil {
		return err
	}
	up, err := c.Upload(ctx, data, whatsmeow.MediaDocument)
	if err != nil {
		return fmt.Errorf("upload document: %w", err)
	}
	length := uint64(len(data))
	fname := fileNameFromURL(url)
	doc := &proto.DocumentMessage{
		Caption:       optstr(caption),
		Mimetype:      optstr(mime),
		FileName:      optstr(fname),
		URL:           optstr(up.URL),
		DirectPath:    optstr(up.DirectPath),
		MediaKey:      up.MediaKey,
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    &length,
	}
	msg := &proto.Message{DocumentMessage: doc}
	_, err = c.SendMessage(ctx, jid, msg)
	return err
}

func fileNameFromURL(u string) string {
	s := u
	if i := strings.Index(s, "?"); i >= 0 {
		s = s[:i]
	}
	if j := strings.LastIndex(s, "/"); j >= 0 && j < len(s)-1 {
		return s[j+1:]
	}
	return "file"
}

func (s *Sender) fetch(ctx context.Context, url string) ([]byte, string, error) {
	// Handle local uploads served by our app: "/uploads/..." or "uploads/..."
	if strings.HasPrefix(url, "/uploads/") || strings.HasPrefix(url, "uploads/") {
		path := url
		// normalize leading slash
		if strings.HasPrefix(path, "/") {
			path = path[1:]
		}
		// security: must stay under uploads/
		if !strings.HasPrefix(path, "uploads/") {
			return nil, "", fmt.Errorf("invalid local upload path")
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, "", err
		}
		defer f.Close()
		body, err := io.ReadAll(f)
		if err != nil {
			return nil, "", err
		}
		// derive content-type based on file extension as a fallback
		lower := strings.ToLower(path)
		ct := "application/octet-stream"
		switch {
		case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
			ct = "image/jpeg"
		case strings.HasSuffix(lower, ".png"):
			ct = "image/png"
		case strings.HasSuffix(lower, ".webp"):
			ct = "image/webp"
		case strings.HasSuffix(lower, ".mp4"):
			ct = "video/mp4"
		case strings.HasSuffix(lower, ".mp3"):
			ct = "audio/mpeg"
		case strings.HasSuffix(lower, ".ogg"):
			ct = "audio/ogg"
		case strings.HasSuffix(lower, ".wav"):
			ct = "audio/wav"
		case strings.HasSuffix(lower, ".m4a"):
			ct = "audio/m4a"
		}
		return body, ct, nil
	}

	// Remote URLs: fetch via HTTP client
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
		// naive fallback using URL extension
		lower := strings.ToLower(url)
		switch {
		case strings.Contains(lower, ".jpg"), strings.Contains(lower, ".jpeg"):
			ct = "image/jpeg"
		case strings.Contains(lower, ".png"):
			ct = "image/png"
		case strings.Contains(lower, ".webp"):
			ct = "image/webp"
		case strings.Contains(lower, ".mp4"):
			ct = "video/mp4"
		case strings.Contains(lower, ".mp3"):
			ct = "audio/mpeg"
		case strings.Contains(lower, ".ogg"):
			ct = "audio/ogg"
		case strings.Contains(lower, ".wav"):
			ct = "audio/wav"
		case strings.Contains(lower, ".m4a"):
			ct = "audio/m4a"
		default:
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
	// Personalisasi waktu lokal Asia/Jakarta (WIB) untuk placeholder {time_now}
	loc, err := time.LoadLocation("Asia/Jakarta")
	now := time.Now()
	if err == nil && loc != nil {
		now = now.In(loc)
	}
	timeNow := now.Format("15:04") // contoh: "08:39"

	r := strings.NewReplacer(
		"{group_name}", groupName,
		"{time_now}", timeNow,
	)
	return r.Replace(text)
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

// Build MessageContent from a random enabled template (DB-level rotation).
func (s *Sender) RandomTemplateContent(ctx context.Context) (MessageContent, error) {
	var text, imgJSON, vidJSON, stJSON, docJSON string
	err := s.Store.DB.QueryRowContext(ctx, `
		SELECT
			COALESCE(text,''),
			COALESCE(images_json,''),
			COALESCE(videos_json,''),
			COALESCE(stickers_json,''),
			COALESCE(docs_json,'')
		FROM templates
		WHERE enabled=1
		ORDER BY RANDOM()
		LIMIT 1
	`).Scan(&text, &imgJSON, &vidJSON, &stJSON, &docJSON)
	if err != nil {
		return MessageContent{}, err
	}
	content := MessageContent{
		Text:        text,
		ImageURLs:   parseJSONArr(imgJSON),
		VideoURLs:   parseJSONArr(vidJSON),
		StickerURLs: parseJSONArr(stJSON),
		DocURLs:     parseJSONArr(docJSON),
	}
	return content, nil
}

// Convenience wrapper to send using a random active template.
func (s *Sender) SendToGroupUsingRandomTemplate(ctx context.Context, accountID, groupJID string) error {
	content, err := s.RandomTemplateContent(ctx)
	if err != nil {
		return fmt.Errorf("no active template or query failed: %w", err)
	}
	return s.SendToGroup(ctx, accountID, groupJID, content)
}

func parseJSONArr(s string) []string {
	var arr []string
	if strings.TrimSpace(s) == "" {
		return arr
	}
	_ = json.Unmarshal([]byte(s), &arr)
	return arr
}
