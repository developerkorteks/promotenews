package autojoin

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"promote/internal/storage"
	"promote/internal/wa"
)

// AutoJoiner handles automatic group joining from invite links
type AutoJoiner struct {
	Store   *storage.Store
	Manager *wa.Manager
	
	// Rate limiting: last join time per account
	lastJoinTime map[string]time.Time
	minInterval  time.Duration // Minimum interval between joins (default: 3 seconds)
}

// New creates a new AutoJoiner instance
func New(store *storage.Store, manager *wa.Manager) *AutoJoiner {
	return &AutoJoiner{
		Store:        store,
		Manager:      manager,
		lastJoinTime: make(map[string]time.Time),
		minInterval:  3 * time.Second, // Safe default
	}
}

// HandleMessage is the event handler untuk incoming messages
// Call this dari wa.Manager event handler
func (aj *AutoJoiner) HandleMessage(accountID string, evt *events.Message) {
	// Skip jika bukan message biasa
	if evt == nil || evt.Message == nil {
		return
	}
	
	// Extract text from message
	text := aj.extractTextFromMessage(evt.Message)
	if text == "" {
		return
	}
	
	// Check if message contains group link
	if !HasGroupLink(text) {
		return
	}
	
	// Extract invite codes
	codes := ExtractInviteCodes(text)
	if len(codes) == 0 {
		return
	}
	
	log.Printf("[autojoin] detected %d group link(s) from %s in account %s", 
		len(codes), evt.Info.Sender.String(), accountID)
	
	// Get sender JID
	senderJID := evt.Info.Sender.String()
	chatJID := evt.Info.Chat.String()
	
	// Process each invite code
	for _, code := range codes {
		aj.ProcessInviteCode(context.Background(), accountID, code, senderJID, chatJID)
	}
}

// ProcessInviteCode processes a single invite code
func (aj *AutoJoiner) ProcessInviteCode(ctx context.Context, accountID, inviteCode, sharedBy, sharedIn string) {
	// Normalize and validate code
	code := NormalizeInviteCode(inviteCode)
	if !ValidateInviteCode(code) {
		log.Printf("[autojoin] invalid invite code: %s", inviteCode)
		aj.logAttempt(accountID, "", "", code, sharedBy, sharedIn, "skipped", string(FilterReasonInvalidCode))
		return
	}
	
	// Load settings for this account
	settings, err := aj.loadSettings(accountID)
	if err != nil {
		log.Printf("[autojoin] failed to load settings for account %s: %v", accountID, err)
		return
	}
	
	// Check if auto-join is enabled
	if !settings.Enabled {
		log.Printf("[autojoin] auto-join disabled for account %s", accountID)
		aj.logAttempt(accountID, "", "", code, sharedBy, sharedIn, "skipped", string(FilterReasonDisabled))
		return
	}
	
	// Count joins today
	joinsToday, err := aj.countJoinsToday(accountID)
	if err != nil {
		log.Printf("[autojoin] failed to count joins today: %v", err)
		return
	}
	
	// Create filter
	filter := &Filter{
		Enabled:            settings.Enabled,
		DailyLimit:         settings.DailyLimit,
		WhitelistContacts:  ParseJSONArray(settings.WhitelistContacts),
		BlacklistKeywords:  ParseJSONArray(settings.BlacklistKeywords),
		PreviewBeforeJoin:  settings.PreviewBeforeJoin,
	}
	
	// Preview group info if enabled
	var groupName string
	if filter.PreviewBeforeJoin {
		groupInfo, err := aj.previewGroup(ctx, accountID, code)
		if err != nil {
			log.Printf("[autojoin] failed to preview group: %v", err)
			aj.logAttempt(accountID, "", "", code, sharedBy, sharedIn, "failed", fmt.Sprintf("preview_failed: %v", err))
			return
		}
		groupName = groupInfo.Name
		log.Printf("[autojoin] preview: group '%s' has %d participants", groupName, len(groupInfo.Participants))
	}
	
	// Apply filters
	shouldJoin, reason := filter.ShouldJoin(sharedBy, groupName, int(joinsToday))
	if !shouldJoin {
		log.Printf("[autojoin] skipped joining group (code: %s) - reason: %s", code, reason)
		aj.logAttempt(accountID, "", groupName, code, sharedBy, sharedIn, "skipped", string(reason))
		return
	}
	
	// Check rate limiting
	if !aj.checkRateLimit(accountID) {
		log.Printf("[autojoin] rate limit - waiting before next join")
		aj.logAttempt(accountID, "", groupName, code, sharedBy, sharedIn, "skipped", string(FilterReasonRateLimit))
		return
	}
	
	// Check if already joined
	if aj.isAlreadyJoined(accountID, code) {
		log.Printf("[autojoin] already joined this group (code: %s)", code)
		aj.logAttempt(accountID, "", groupName, code, sharedBy, sharedIn, "skipped", string(FilterReasonAlreadyJoined))
		return
	}
	
	// Rate limit: wait before joining
	aj.waitForRateLimit(ctx, accountID)
	
	// Join the group!
	groupJID, err := aj.joinGroup(ctx, accountID, code)
	if err != nil {
		log.Printf("[autojoin] failed to join group (code: %s): %v", code, err)
		aj.logAttempt(accountID, "", groupName, code, sharedBy, sharedIn, "failed", err.Error())
		return
	}
	
	// Success!
	log.Printf("[autojoin] ✅ successfully joined group: %s (code: %s)", groupJID.String(), code)
	
	// Get final group info
	if groupName == "" {
		if info, err := aj.Manager.GetClient(accountID); err == nil {
			if groupInfo, err := info.GetGroupInfo(ctx, groupJID); err == nil {
				groupName = groupInfo.Name
			}
		}
	}
	
	// Log success
	aj.logAttempt(accountID, groupJID.String(), groupName, code, sharedBy, sharedIn, "joined", "")
	
	// Update last join time
	aj.lastJoinTime[accountID] = time.Now()
	
	// Sync groups to database (async)
	go func() {
		time.Sleep(2 * time.Second)
		if _, err := aj.Manager.FetchAndSyncGroups(context.Background(), accountID); err != nil {
			log.Printf("[autojoin] failed to sync groups after join: %v", err)
		}
	}()
}

// joinGroup joins a group using invite code
func (aj *AutoJoiner) joinGroup(ctx context.Context, accountID, inviteCode string) (types.JID, error) {
	client, err := aj.Manager.GetClient(accountID)
	if err != nil {
		return types.JID{}, fmt.Errorf("get client: %w", err)
	}
	
	// Ensure connected
	if !client.IsConnected() {
		if err := client.Connect(); err != nil {
			return types.JID{}, fmt.Errorf("connect: %w", err)
		}
		// Wait for connection to stabilize
		time.Sleep(2 * time.Second)
	}
	
	// Join with timeout
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	
	groupJID, err := client.JoinGroupWithLink(ctx2, inviteCode)
	if err != nil {
		return types.JID{}, fmt.Errorf("join: %w", err)
	}
	
	return groupJID, nil
}

// previewGroup gets group info before joining
func (aj *AutoJoiner) previewGroup(ctx context.Context, accountID, inviteCode string) (*types.GroupInfo, error) {
	client, err := aj.Manager.GetClient(accountID)
	if err != nil {
		return nil, fmt.Errorf("get client: %w", err)
	}
	
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	
	info, err := client.GetGroupInfoFromLink(ctx2, inviteCode)
	if err != nil {
		return nil, fmt.Errorf("get info: %w", err)
	}
	
	return info, nil
}

// checkRateLimit checks if we can join now (without waiting)
func (aj *AutoJoiner) checkRateLimit(accountID string) bool {
	lastJoin, exists := aj.lastJoinTime[accountID]
	if !exists {
		return true
	}
	elapsed := time.Since(lastJoin)
	return elapsed >= aj.minInterval
}

// waitForRateLimit waits if necessary to respect rate limits
func (aj *AutoJoiner) waitForRateLimit(ctx context.Context, accountID string) {
	lastJoin, exists := aj.lastJoinTime[accountID]
	if !exists {
		return
	}
	
	elapsed := time.Since(lastJoin)
	if elapsed < aj.minInterval {
		waitTime := aj.minInterval - elapsed
		log.Printf("[autojoin] rate limit: waiting %v before next join", waitTime)
		select {
		case <-time.After(waitTime):
		case <-ctx.Done():
		}
	}
}

// isAlreadyJoined checks if we already joined this group
func (aj *AutoJoiner) isAlreadyJoined(accountID, inviteCode string) bool {
	var count int
	err := aj.Store.DB.QueryRow(`
		SELECT COUNT(*) FROM auto_join_logs 
		WHERE account_id=? AND invite_code=? AND status='joined'
	`, accountID, inviteCode).Scan(&count)
	return err == nil && count > 0
}

// extractTextFromMessage extracts text content from various message types
func (aj *AutoJoiner) extractTextFromMessage(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}
	
	// Text message
	if msg.Conversation != nil {
		return *msg.Conversation
	}
	
	// Extended text
	if msg.ExtendedTextMessage != nil && msg.ExtendedTextMessage.Text != nil {
		return *msg.ExtendedTextMessage.Text
	}
	
	// Image with caption
	if msg.ImageMessage != nil && msg.ImageMessage.Caption != nil {
		return *msg.ImageMessage.Caption
	}
	
	// Video with caption
	if msg.VideoMessage != nil && msg.VideoMessage.Caption != nil {
		return *msg.VideoMessage.Caption
	}
	
	// Document with caption
	if msg.DocumentMessage != nil && msg.DocumentMessage.Caption != nil {
		return *msg.DocumentMessage.Caption
	}
	
	return ""
}

// Helper functions for database operations

func (aj *AutoJoiner) loadSettings(accountID string) (*AutoJoinSettings, error) {
	settings := &AutoJoinSettings{
		Enabled:            false,
		DailyLimit:         20, // Default
		PreviewBeforeJoin:  true,
		WhitelistContacts:  "[]",
		BlacklistKeywords:  "[]",
	}
	
	err := aj.Store.DB.QueryRow(`
		SELECT enabled, daily_limit, preview_before_join, 
		       COALESCE(whitelist_contacts, '[]'), COALESCE(blacklist_keywords, '[]')
		FROM auto_join_settings WHERE account_id=?
	`, accountID).Scan(&settings.Enabled, &settings.DailyLimit, &settings.PreviewBeforeJoin,
		&settings.WhitelistContacts, &settings.BlacklistKeywords)
	
	if err == sql.ErrNoRows {
		// No settings yet, return defaults
		return settings, nil
	}
	
	return settings, err
}

func (aj *AutoJoiner) countJoinsToday(accountID string) (int64, error) {
	var count int64
	err := aj.Store.DB.QueryRow(`
		SELECT COUNT(*) FROM auto_join_logs 
		WHERE account_id=? AND status='joined' 
		AND joined_at >= datetime('now', 'start of day')
	`, accountID).Scan(&count)
	return count, err
}

func (aj *AutoJoiner) logAttempt(accountID, groupID, groupName, inviteCode, sharedBy, sharedIn, status, reason string) error {
	_, err := aj.Store.DB.Exec(`
		INSERT INTO auto_join_logs 
		(account_id, group_id, group_name, invite_code, shared_by, shared_in, status, reason, joined_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`, accountID, nullStr(groupID), nullStr(groupName), inviteCode, nullStr(sharedBy), nullStr(sharedIn), status, nullStr(reason))
	return err
}

func nullStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// AutoJoinSettings represents auto-join configuration for an account
type AutoJoinSettings struct {
	Enabled            bool
	DailyLimit         int
	PreviewBeforeJoin  bool
	WhitelistContacts  string // JSON array
	BlacklistKeywords  string // JSON array
}
