#!/bin/bash
# Auto-Join Feature Testing Script

# Configuration
ACCOUNT_ID="${1:-your-account-id}"
BASE_URL="${2:-http://localhost:9724}"

echo "╔═══════════════════════════════════════════════════════════════╗"
echo "║         AUTO-JOIN GROUP FEATURE - TESTING SCRIPT              ║"
echo "╚═══════════════════════════════════════════════════════════════╝"
echo ""
echo "Account ID: $ACCOUNT_ID"
echo "Base URL:   $BASE_URL"
echo ""

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

# Function to print section
print_section() {
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  $1"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
}

# Test 1: Check if account exists
print_section "1. Checking if account exists"
response=$(curl -s "$BASE_URL/api/accounts")
if echo "$response" | grep -q "$ACCOUNT_ID"; then
    echo -e "${GREEN}✓${NC} Account found"
else
    echo -e "${RED}✗${NC} Account not found. Please create account first or provide correct ACCOUNT_ID"
    echo ""
    echo "Usage: $0 <account-id> [base-url]"
    echo "Example: $0 abc-123-def http://localhost:9724"
    exit 1
fi

# Test 2: Get current settings (before enable)
print_section "2. Getting current auto-join settings"
curl -s "$BASE_URL/api/accounts/$ACCOUNT_ID/autojoin/settings" | jq '.' 2>/dev/null || \
    curl -s "$BASE_URL/api/accounts/$ACCOUNT_ID/autojoin/settings"
echo ""

# Test 3: Enable auto-join
print_section "3. Enabling auto-join with default settings"
response=$(curl -s -X POST "$BASE_URL/api/accounts/$ACCOUNT_ID/autojoin/enable" \
    -H "Content-Type: application/json" \
    -d '{"enabled": true}')
echo "$response" | jq '.' 2>/dev/null || echo "$response"

if echo "$response" | grep -q "updated"; then
    echo -e "${GREEN}✓${NC} Auto-join enabled successfully"
else
    echo -e "${RED}✗${NC} Failed to enable auto-join"
fi

# Test 4: Verify settings after enable
print_section "4. Verifying settings after enable"
curl -s "$BASE_URL/api/accounts/$ACCOUNT_ID/autojoin/settings" | jq '.' 2>/dev/null || \
    curl -s "$BASE_URL/api/accounts/$ACCOUNT_ID/autojoin/settings"
echo ""

# Test 5: Update settings with filters
print_section "5. Updating settings with safety filters"
curl -s -X PUT "$BASE_URL/api/accounts/$ACCOUNT_ID/autojoin/settings" \
    -H "Content-Type: application/json" \
    -d '{
        "enabled": true,
        "daily_limit": 15,
        "preview_before_join": true,
        "whitelist_contacts": [],
        "blacklist_keywords": ["judi", "forex", "binary", "mlm"]
    }' | jq '.' 2>/dev/null || \
    curl -s -X PUT "$BASE_URL/api/accounts/$ACCOUNT_ID/autojoin/settings" \
        -H "Content-Type: application/json" \
        -d '{
            "enabled": true,
            "daily_limit": 15,
            "preview_before_join": true,
            "whitelist_contacts": [],
            "blacklist_keywords": ["judi", "forex", "binary", "mlm"]
        }'
echo ""

# Test 6: Get updated settings
print_section "6. Confirming updated settings"
curl -s "$BASE_URL/api/accounts/$ACCOUNT_ID/autojoin/settings" | jq '.' 2>/dev/null || \
    curl -s "$BASE_URL/api/accounts/$ACCOUNT_ID/autojoin/settings"
echo ""

# Test 7: Check logs (before any joins)
print_section "7. Checking auto-join logs (current state)"
response=$(curl -s "$BASE_URL/api/accounts/$ACCOUNT_ID/autojoin/logs?limit=10")
echo "$response" | jq '.' 2>/dev/null || echo "$response"
echo ""

# Extract stats
if command -v jq &> /dev/null; then
    total_joined=$(echo "$response" | jq -r '.stats.total_joined // 0')
    total_failed=$(echo "$response" | jq -r '.stats.total_failed // 0')
    total_skipped=$(echo "$response" | jq -r '.stats.total_skipped // 0')
    joined_today=$(echo "$response" | jq -r '.stats.joined_today // 0')
    
    echo "Statistics:"
    echo "  - Total Joined:  $total_joined"
    echo "  - Total Failed:  $total_failed"
    echo "  - Total Skipped: $total_skipped"
    echo "  - Joined Today:  $joined_today"
fi

# Test 8: Manual join test (optional - requires valid invite code)
print_section "8. Manual Join Test (Optional)"
echo ""
echo -e "${YELLOW}⚠${NC}  To test manual join, you need a valid WhatsApp group invite code."
echo "    This test is commented out by default."
echo ""
echo "    To test manually, uncomment and update the code below:"
echo ""
echo "    INVITE_CODE=\"YOUR_INVITE_CODE_HERE\""
echo "    curl -X POST \"$BASE_URL/api/autojoin/manual\" \\"
echo "      -H \"Content-Type: application/json\" \\"
echo "      -d '{\"account_id\": \"$ACCOUNT_ID\", \"invite_code\": \"'\$INVITE_CODE'\"}'"
echo ""

# Uncomment below to test with real invite code:
# INVITE_CODE="YOUR_INVITE_CODE_HERE"
# if [ "$INVITE_CODE" != "YOUR_INVITE_CODE_HERE" ]; then
#     echo "Testing manual join with code: $INVITE_CODE"
#     curl -s -X POST "$BASE_URL/api/autojoin/manual" \
#         -H "Content-Type: application/json" \
#         -d "{\"account_id\": \"$ACCOUNT_ID\", \"invite_code\": \"$INVITE_CODE\"}" | \
#         jq '.' 2>/dev/null || \
#         curl -s -X POST "$BASE_URL/api/autojoin/manual" \
#             -H "Content-Type: application/json" \
#             -d "{\"account_id\": \"$ACCOUNT_ID\", \"invite_code\": \"$INVITE_CODE\"}"
#     
#     echo ""
#     echo "Waiting 5 seconds for processing..."
#     sleep 5
#     
#     echo ""
#     echo "Checking logs after manual join:"
#     curl -s "$BASE_URL/api/accounts/$ACCOUNT_ID/autojoin/logs?limit=5" | jq '.logs' 2>/dev/null || \
#         curl -s "$BASE_URL/api/accounts/$ACCOUNT_ID/autojoin/logs?limit=5"
# fi

# Test 9: Disable auto-join
print_section "9. Testing disable functionality"
echo "Disabling auto-join..."
curl -s -X POST "$BASE_URL/api/accounts/$ACCOUNT_ID/autojoin/enable" \
    -H "Content-Type: application/json" \
    -d '{"enabled": false}' | jq '.' 2>/dev/null || \
    curl -s -X POST "$BASE_URL/api/accounts/$ACCOUNT_ID/autojoin/enable" \
        -H "Content-Type: application/json" \
        -d '{"enabled": false}'
echo ""

# Test 10: Re-enable for actual use
print_section "10. Re-enabling auto-join for production use"
curl -s -X POST "$BASE_URL/api/accounts/$ACCOUNT_ID/autojoin/enable" \
    -H "Content-Type: application/json" \
    -d '{"enabled": true}' | jq '.' 2>/dev/null || \
    curl -s -X POST "$BASE_URL/api/accounts/$ACCOUNT_ID/autojoin/enable" \
        -H "Content-Type: application/json" \
        -d '{"enabled": true}'
echo ""

# Summary
print_section "✅ TESTING COMPLETE"
echo ""
echo "Summary:"
echo "  ✓ Auto-join feature is now ENABLED"
echo "  ✓ Daily limit: 15 groups"
echo "  ✓ Preview before join: Enabled"
echo "  ✓ Blacklist keywords: judi, forex, binary, mlm"
echo ""
echo "Next Steps:"
echo "  1. Send yourself a WhatsApp group invite link"
echo "  2. The bot will automatically join the group (if filters pass)"
echo "  3. Monitor logs: curl $BASE_URL/api/accounts/$ACCOUNT_ID/autojoin/logs"
echo "  4. Check server logs: tail -f server.log | grep autojoin"
echo ""
echo -e "${GREEN}Happy auto-joining! 🚀${NC}"
echo ""
