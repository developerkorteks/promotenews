# 🚀 Auto-Join Group - Quick Start Guide

## ⚡ 3 Langkah Mudah untuk Mulai

### **Step 1: Start Server**
```bash
./main
```

### **Step 2: Enable Auto-Join**
```bash
# Ganti YOUR_ACCOUNT_ID dengan ID akun WhatsApp Anda
curl -X POST http://localhost:9724/api/accounts/YOUR_ACCOUNT_ID/autojoin/enable \
  -H "Content-Type: application/json" \
  -d '{"enabled": true}'
```

### **Step 3: Test!**
Kirim pesan ke diri sendiri atau minta teman kirim link grup WhatsApp:
```
https://chat.whatsapp.com/ABC123XYZ
```

Bot akan otomatis join! ✅

---

## 📱 Testing Script

Untuk testing lengkap:
```bash
./tmp_rovodev_test_autojoin.sh YOUR_ACCOUNT_ID
```

---

## 🎛️ Configuration (Optional)

### Default Settings (Sudah Aman):
- ✅ Daily limit: 20 groups/day
- ✅ Rate limiting: 3 seconds between joins
- ✅ Preview before join: Enabled
- ✅ Blacklist: Empty (join all)

### Customize Settings:
```bash
curl -X PUT http://localhost:9724/api/accounts/YOUR_ACCOUNT_ID/autojoin/settings \
  -H "Content-Type: application/json" \
  -d '{
    "enabled": true,
    "daily_limit": 10,
    "preview_before_join": true,
    "blacklist_keywords": ["judi", "forex", "mlm"]
  }'
```

---

## 📊 Monitor Logs

```bash
# View recent joins
curl http://localhost:9724/api/accounts/YOUR_ACCOUNT_ID/autojoin/logs

# Filter by status
curl "http://localhost:9724/api/accounts/YOUR_ACCOUNT_ID/autojoin/logs?status=joined"

# Watch live logs
tail -f server.log | grep autojoin
```

---

## 🎯 Use Cases

### ✅ **Join All Groups (Default)**
```json
{
  "enabled": true,
  "daily_limit": 50,
  "blacklist_keywords": []
}
```

### 🛡️ **Safe Mode (Filter Spam)**
```json
{
  "enabled": true,
  "daily_limit": 10,
  "blacklist_keywords": ["judi", "forex", "binary", "investment"]
}
```

### 👥 **Trusted Contacts Only**
```json
{
  "enabled": true,
  "whitelist_contacts": ["628123456789@s.whatsapp.net"]
}
```

---

## ❓ FAQ

**Q: Apakah aman?**  
A: Ya! Ada rate limiting (3s delay), daily limit, dan blacklist protection.

**Q: Berapa grup maksimal per hari?**  
A: Default 20, max 100 (untuk keamanan dari WhatsApp spam detection).

**Q: Bisa filter grup spam?**  
A: Ya! Gunakan `blacklist_keywords` untuk skip grup dengan kata tertentu.

**Q: Apakah harus join semua link?**  
A: Tidak. Anda bisa set whitelist untuk hanya join dari kontak terpercaya.

**Q: Bagaimana cek history?**  
A: `curl http://localhost:9724/api/accounts/ACCOUNT_ID/autojoin/logs`

---

## 🎉 That's It!

Anda sekarang tidak perlu lagi join grup manual. Bot akan handle semuanya! 🚀

**Dokumentasi Lengkap**: `AUTO_JOIN_IMPLEMENTATION.md`
