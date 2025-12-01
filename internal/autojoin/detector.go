package autojoin

import (
	"regexp"
	"strings"
)

// Link patterns untuk WhatsApp group invite
var (
	// Pattern untuk link WhatsApp group
	groupLinkPattern = regexp.MustCompile(`https?://chat\.whatsapp\.com/([A-Za-z0-9]+)`)
	// Pattern alternatif (kadang tanpa https)
	groupLinkPatternAlt = regexp.MustCompile(`chat\.whatsapp\.com/([A-Za-z0-9]+)`)
)

// ExtractInviteCodes mengekstrak semua invite code dari teks pesan
// Returns: slice of invite codes yang ditemukan
func ExtractInviteCodes(text string) []string {
	if text == "" {
		return nil
	}
	
	var codes []string
	seen := make(map[string]bool)
	
	// Try main pattern first
	matches := groupLinkPattern.FindAllStringSubmatch(text, -1)
	for _, match := range matches {
		if len(match) > 1 {
			code := match[1]
			if !seen[code] {
				codes = append(codes, code)
				seen[code] = true
			}
		}
	}
	
	// Try alternative pattern if no match
	if len(codes) == 0 {
		matches = groupLinkPatternAlt.FindAllStringSubmatch(text, -1)
		for _, match := range matches {
			if len(match) > 1 {
				code := match[1]
				if !seen[code] {
					codes = append(codes, code)
					seen[code] = true
				}
			}
		}
	}
	
	return codes
}

// HasGroupLink cek apakah text mengandung link group
func HasGroupLink(text string) bool {
	return groupLinkPattern.MatchString(text) || groupLinkPatternAlt.MatchString(text)
}

// NormalizeInviteCode membersihkan invite code dari karakter tidak valid
func NormalizeInviteCode(code string) string {
	code = strings.TrimSpace(code)
	// Remove any whitespace or special chars
	code = strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, code)
	return code
}

// ValidateInviteCode validasi format invite code
func ValidateInviteCode(code string) bool {
	if len(code) < 10 || len(code) > 30 {
		return false
	}
	// Must contain alphanumeric only
	for _, c := range code {
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}
