package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

// Auto-join settings structure for API
type autoJoinSettingsReq struct {
	Enabled            bool     `json:"enabled"`
	DailyLimit         int      `json:"daily_limit"`
	PreviewBeforeJoin  bool     `json:"preview_before_join"`
	WhitelistContacts  []string `json:"whitelist_contacts"`
	BlacklistKeywords  []string `json:"blacklist_keywords"`
}

// handleGetAutoJoinSettings returns auto-join settings for an account
func (a *API) handleGetAutoJoinSettings(w http.ResponseWriter, r *http.Request) {
	accountID := chi.URLParam(r, "id")
	
	exists, err := a.Store.AccountExists(accountID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	
	// Load settings from database
	var (
		enabled           int
		dailyLimit        int
		previewBeforeJoin int
		whitelistJSON     string
		blacklistJSON     string
	)
	
	err = a.Store.DB.QueryRow(`
		SELECT enabled, daily_limit, preview_before_join, 
		       COALESCE(whitelist_contacts, '[]'), COALESCE(blacklist_keywords, '[]')
		FROM auto_join_settings WHERE account_id=?
	`, accountID).Scan(&enabled, &dailyLimit, &previewBeforeJoin, &whitelistJSON, &blacklistJSON)
	
	if err == sql.ErrNoRows {
		// Return defaults
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":             false,
			"daily_limit":         20,
			"preview_before_join": true,
			"whitelist_contacts":  []string{},
			"blacklist_keywords":  []string{},
		})
		return
	}
	
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	
	// Parse JSON arrays
	var whitelist, blacklist []string
	_ = json.Unmarshal([]byte(whitelistJSON), &whitelist)
	_ = json.Unmarshal([]byte(blacklistJSON), &blacklist)
	
	if whitelist == nil {
		whitelist = []string{}
	}
	if blacklist == nil {
		blacklist = []string{}
	}
	
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":             enabled == 1,
		"daily_limit":         dailyLimit,
		"preview_before_join": previewBeforeJoin == 1,
		"whitelist_contacts":  whitelist,
		"blacklist_keywords":  blacklist,
	})
}

// handleUpdateAutoJoinSettings updates auto-join settings for an account
func (a *API) handleUpdateAutoJoinSettings(w http.ResponseWriter, r *http.Request) {
	accountID := chi.URLParam(r, "id")
	
	exists, err := a.Store.AccountExists(accountID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	
	var req autoJoinSettingsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	
	// Validate daily limit
	if req.DailyLimit < 1 {
		req.DailyLimit = 20
	}
	if req.DailyLimit > 100 {
		req.DailyLimit = 100 // Safety cap
	}
	
	// Convert arrays to JSON
	whitelistJSON, _ := json.Marshal(req.WhitelistContacts)
	blacklistJSON, _ := json.Marshal(req.BlacklistKeywords)
	
	// Upsert settings
	_, err = a.Store.DB.Exec(`
		INSERT INTO auto_join_settings 
		(account_id, enabled, daily_limit, preview_before_join, whitelist_contacts, blacklist_keywords)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET
			enabled=excluded.enabled,
			daily_limit=excluded.daily_limit,
			preview_before_join=excluded.preview_before_join,
			whitelist_contacts=excluded.whitelist_contacts,
			blacklist_keywords=excluded.blacklist_keywords
	`, accountID, btoi(req.Enabled), req.DailyLimit, btoi(req.PreviewBeforeJoin), 
	   string(whitelistJSON), string(blacklistJSON))
	
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	
	writeJSON(w, http.StatusOK, map[string]any{
		"updated": true,
		"message": "Auto-join settings updated successfully",
	})
}

// handleToggleAutoJoin quickly enables/disables auto-join
func (a *API) handleToggleAutoJoin(w http.ResponseWriter, r *http.Request) {
	accountID := chi.URLParam(r, "id")
	
	exists, err := a.Store.AccountExists(accountID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	
	// Upsert with default settings if not exists
	_, err = a.Store.DB.Exec(`
		INSERT INTO auto_join_settings (account_id, enabled, daily_limit, preview_before_join)
		VALUES (?, ?, 20, 1)
		ON CONFLICT(account_id) DO UPDATE SET enabled=excluded.enabled
	`, accountID, btoi(req.Enabled))
	
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	
	status := "disabled"
	if req.Enabled {
		status = "enabled"
	}
	
	writeJSON(w, http.StatusOK, map[string]any{
		"updated": true,
		"status":  status,
	})
}

// handleGetAutoJoinLogs returns auto-join history for an account
func (a *API) handleGetAutoJoinLogs(w http.ResponseWriter, r *http.Request) {
	accountID := chi.URLParam(r, "id")
	
	exists, err := a.Store.AccountExists(accountID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	
	// Parse query parameters
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= 500 {
			limit = l
		}
	}
	
	statusFilter := r.URL.Query().Get("status") // joined, failed, skipped, or empty for all
	
	// Build query
	query := `
		SELECT id, account_id, COALESCE(group_id, ''), COALESCE(group_name, ''), 
		       invite_code, COALESCE(shared_by, ''), COALESCE(shared_in, ''),
		       status, COALESCE(reason, ''), joined_at
		FROM auto_join_logs
		WHERE account_id=?
	`
	args := []interface{}{accountID}
	
	if statusFilter != "" {
		query += " AND status=?"
		args = append(args, statusFilter)
	}
	
	query += " ORDER BY joined_at DESC LIMIT ?"
	args = append(args, limit)
	
	rows, err := a.Store.DB.Query(query, args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	
	var logs []map[string]any
	for rows.Next() {
		var (
			id                                                       int64
			accountID, groupID, groupName, inviteCode, sharedBy, sharedIn string
			status, reason                                           string
			joinedAt                                                 time.Time
		)
		
		if err := rows.Scan(&id, &accountID, &groupID, &groupName, &inviteCode, 
			&sharedBy, &sharedIn, &status, &reason, &joinedAt); err != nil {
			continue
		}
		
		logs = append(logs, map[string]any{
			"id":           id,
			"account_id":   accountID,
			"group_id":     groupID,
			"group_name":   groupName,
			"invite_code":  inviteCode,
			"shared_by":    sharedBy,
			"shared_in":    sharedIn,
			"status":       status,
			"reason":       reason,
			"joined_at":    joinedAt.Format(time.RFC3339),
		})
	}
	
	if logs == nil {
		logs = []map[string]any{}
	}
	
	// Get stats
	var totalJoined, totalFailed, totalSkipped int64
	_ = a.Store.DB.QueryRow(`
		SELECT 
			COALESCE(SUM(CASE WHEN status='joined' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status='skipped' THEN 1 ELSE 0 END), 0)
		FROM auto_join_logs WHERE account_id=?
	`, accountID).Scan(&totalJoined, &totalFailed, &totalSkipped)
	
	// Get today's count
	var joinedToday int64
	_ = a.Store.DB.QueryRow(`
		SELECT COUNT(*) FROM auto_join_logs 
		WHERE account_id=? AND status='joined' 
		AND joined_at >= datetime('now', 'start of day')
	`, accountID).Scan(&joinedToday)
	
	writeJSON(w, http.StatusOK, map[string]any{
		"logs": logs,
		"stats": map[string]any{
			"total_joined":  totalJoined,
			"total_failed":  totalFailed,
			"total_skipped": totalSkipped,
			"joined_today":  joinedToday,
		},
	})
}

// handleManualJoin allows manual joining of a group via link
func (a *API) handleManualJoin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID  string `json:"account_id"`
		InviteCode string `json:"invite_code"`
		InviteLink string `json:"invite_link"` // Alternative: full link
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	
	if req.AccountID == "" {
		writeErr(w, http.StatusBadRequest, "account_id required")
		return
	}
	
	// Extract code from link if provided
	inviteCode := req.InviteCode
	if inviteCode == "" && req.InviteLink != "" {
		// Try to extract from link
		// Simple extraction: take part after last /
		parts := []rune(req.InviteLink)
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i] == '/' {
				inviteCode = string(parts[i+1:])
				break
			}
		}
	}
	
	if inviteCode == "" {
		writeErr(w, http.StatusBadRequest, "invite_code or invite_link required")
		return
	}
	
	exists, err := a.Store.AccountExists(req.AccountID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	
	// Process the invite code
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	
	// Use empty sharedBy/sharedIn for manual joins
	go a.AutoJoiner.ProcessInviteCode(ctx, req.AccountID, inviteCode, "manual", "manual")
	
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "processing",
		"message": "Join request submitted. Check logs for status.",
	})
}
