package autojoin

import (
	"strings"
)

// FilterReason adalah alasan mengapa auto-join di-skip
type FilterReason string

const (
	FilterReasonDisabled       FilterReason = "auto_join_disabled"
	FilterReasonDailyLimit     FilterReason = "daily_limit_reached"
	FilterReasonNotWhitelisted FilterReason = "sender_not_whitelisted"
	FilterReasonBlacklisted    FilterReason = "keyword_blacklisted"
	FilterReasonAlreadyJoined  FilterReason = "already_joined"
	FilterReasonInvalidCode    FilterReason = "invalid_invite_code"
	FilterReasonRateLimit      FilterReason = "rate_limit"
)

// Filter handles filtering logic untuk auto-join
type Filter struct {
	Enabled            bool
	DailyLimit         int
	WhitelistContacts  []string // JID list, empty = allow all
	BlacklistKeywords  []string // Lowercase keywords
	PreviewBeforeJoin  bool
}

// ShouldJoin menentukan apakah boleh join berdasarkan filter rules
// Returns: (should join, reason if not)
func (f *Filter) ShouldJoin(senderJID string, groupName string, joinsToday int) (bool, FilterReason) {
	// Check if enabled
	if !f.Enabled {
		return false, FilterReasonDisabled
	}
	
	// Check daily limit
	if joinsToday >= f.DailyLimit {
		return false, FilterReasonDailyLimit
	}
	
	// Check whitelist (if configured)
	if len(f.WhitelistContacts) > 0 {
		if !f.isWhitelisted(senderJID) {
			return false, FilterReasonNotWhitelisted
		}
	}
	
	// Check blacklist keywords in group name
	if groupName != "" && f.isBlacklisted(groupName) {
		return false, FilterReasonBlacklisted
	}
	
	return true, ""
}

// isWhitelisted checks if sender is in whitelist
func (f *Filter) isWhitelisted(senderJID string) bool {
	senderJID = strings.ToLower(senderJID)
	for _, allowed := range f.WhitelistContacts {
		if strings.ToLower(allowed) == senderJID {
			return true
		}
	}
	return false
}

// isBlacklisted checks if group name contains blacklisted keywords
func (f *Filter) isBlacklisted(groupName string) bool {
	lowerName := strings.ToLower(groupName)
	for _, keyword := range f.BlacklistKeywords {
		if strings.Contains(lowerName, strings.ToLower(keyword)) {
			return true
		}
	}
	return false
}

// ParseJSONArray parses JSON array string to slice
func ParseJSONArray(jsonStr string) []string {
	if jsonStr == "" || jsonStr == "[]" || jsonStr == "null" {
		return nil
	}
	// Simple parsing for JSON array of strings
	jsonStr = strings.TrimSpace(jsonStr)
	jsonStr = strings.Trim(jsonStr, "[]")
	if jsonStr == "" {
		return nil
	}
	
	var result []string
	parts := strings.Split(jsonStr, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		part = strings.Trim(part, `"`)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

// ToJSONArray converts slice to JSON array string
func ToJSONArray(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	var quoted []string
	for _, item := range items {
		quoted = append(quoted, `"`+item+`"`)
	}
	return "[" + strings.Join(quoted, ",") + "]"
}
