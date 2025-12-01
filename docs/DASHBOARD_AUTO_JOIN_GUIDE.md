# 🎨 Dashboard Auto-Join Feature - User Guide

## ✅ FITUR BARU: Toggle Auto-Join di Dashboard!

Fitur Auto-Join sekarang dapat diaktifkan/dinonaktifkan langsung dari dashboard web tanpa perlu API manual!

---

## 📍 Lokasi di Dashboard

**Tabel Akun → Kolom "Auto-Join"**

Kolom baru telah ditambahkan di tabel daftar akun dengan checkbox toggle:

```
┌───────────────────────────────────────────────────────────────┐
│ Label  │ MSISDN │ Status │ Limit │ Auto-Join │ Aksi          │
├────────┼────────┼────────┼───────┼───────────┼───────────────┤
│ Akun 1 │ 628... │ online │  100  │ ☑ ON      │ [QR][Connect] │
│ Akun 2 │ 628... │ online │  100  │ ☐ OFF     │ [QR][Connect] │
└───────────────────────────────────────────────────────────────┘
```

---

## 🎯 Cara Menggunakan

### **1. Aktifkan Auto-Join**

1. Buka dashboard: `http://localhost:9724`
2. Scroll ke section **"Daftar Akun"**
3. Lihat kolom **"Auto-Join"** 
4. **Klik checkbox** di baris akun yang ingin diaktifkan
5. Label akan berubah menjadi **"ON"** dengan warna hijau ✅

**Status ON:** Auto-join aktif, bot akan otomatis join grup dari link yang masuk!

### **2. Nonaktifkan Auto-Join**

1. **Klik checkbox lagi** untuk uncheck
2. Label akan berubah menjadi **"OFF"** dengan warna abu-abu

**Status OFF:** Auto-join tidak aktif, link grup akan diabaikan.

---

## 🎨 Indicator Visual

### **Status Tampilan:**

| Checkbox | Label | Warna | Arti |
|----------|-------|-------|------|
| ☑ | **ON** | 🟢 Hijau (#7bd88f) | Auto-join aktif |
| ☐ | **OFF** | ⚪ Abu-abu (#9aa0aa) | Auto-join tidak aktif |
| ☐ | **...** | ⚪ Abu-abu | Loading... |
| ☐ | **N/A** | 🔴 Merah (#ff6b6b) | Error / tidak tersedia |

---

## ⚙️ Fitur Auto-Join

Saat **Auto-Join = ON**, bot akan:

✅ **Detect** link WhatsApp di pesan masuk  
✅ **Extract** invite code otomatis  
✅ **Join** grup secara otomatis  
✅ **Log** semua aktivitas  

### **Default Settings:**
- ✅ Daily limit: **20 grup/hari**
- ✅ Rate limiting: **3 detik** antar join
- ✅ Preview before join: **Enabled**
- ✅ Blacklist: **Empty** (join all)

---

## 🔧 Advanced Settings (Opsional)

Untuk konfigurasi lanjutan (whitelist, blacklist, dll), gunakan API:

```bash
curl -X PUT http://localhost:9724/api/accounts/ACCOUNT_ID/autojoin/settings \
  -H "Content-Type: application/json" \
  -d '{
    "enabled": true,
    "daily_limit": 15,
    "preview_before_join": true,
    "whitelist_contacts": [],
    "blacklist_keywords": ["judi", "forex", "mlm"]
  }'
```

---

## 🐛 Troubleshooting

### **Problem: Checkbox tidak muncul**
**Solution:** Refresh halaman (F5)

### **Problem: Status menunjukkan "N/A"**
**Solution:** 
- Pastikan server running
- Check console browser (F12) untuk error
- Pastikan akun sudah dibuat di database

### **Problem: Toggle tidak berfungsi**
**Solution:**
- Check network tab (F12) untuk melihat API response
- Pastikan endpoint `/api/accounts/{id}/autojoin/enable` accessible
- Check server logs: `tail -f server.log | grep autojoin`

### **Problem: Status kembali ke OFF setelah refresh**
**Solution:**
- Settings disimpan di database
- Jika masih OFF, kemungkinan ada error saat save
- Check API response dengan browser dev tools

---

## 📊 Monitoring

### **1. Via Dashboard**
- Status realtime di kolom Auto-Join
- Checkbox reflects current state

### **2. Via API**
```bash
# Check current status
curl http://localhost:9724/api/accounts/ACCOUNT_ID/autojoin/settings

# View join logs
curl http://localhost:9724/api/accounts/ACCOUNT_ID/autojoin/logs
```

### **3. Via Server Logs**
```bash
tail -f server.log | grep autojoin
```

**Log format:**
```
[autojoin] detected 1 group link(s) from 628xxx in account abc-123
[autojoin] ✅ successfully joined group: 123@g.us (code: ABC123)
```

---

## 💡 Tips & Best Practices

### **Tip 1: Start Conservative**
Mulai dengan 1 akun dulu untuk testing, lalu scale up

### **Tip 2: Monitor Daily**
Check logs setiap hari untuk ensure tidak ada spam groups

### **Tip 3: Use Blacklist**
Tambahkan kata-kata spam umum di blacklist:
- "judi", "forex", "binary"
- "mlm", "investment", "crypto"
- "18+", "adult", "sex"

### **Tip 4: Set Reasonable Limit**
Daily limit 10-20 grup sudah cukup untuk most cases

### **Tip 5: Enable Preview**
Aktifkan preview untuk lihat info grup sebelum join

---

## 🎯 Use Case Examples

### **Example 1: Personal Use**
```
✅ Enable auto-join untuk 1 akun personal
✅ Daily limit: 10
✅ No filters (join all)
```

### **Example 2: Business Use**
```
✅ Enable auto-join untuk multiple accounts
✅ Daily limit: 20 per account
✅ Blacklist: spam keywords
✅ Monitor logs daily
```

### **Example 3: Selective Mode**
```
✅ Enable auto-join
✅ Daily limit: 5
✅ Whitelist: trusted contacts only
✅ Preview enabled
```

---

## 🔄 Integration Flow

```
User Action → Dashboard
     ↓
Checkbox Toggle
     ↓
JavaScript Event Handler
     ↓
API Call: POST /api/accounts/{id}/autojoin/enable
     ↓
Database Update
     ↓
UI Update (ON/OFF label)
     ↓
Auto-Join Active! 🎉
```

---

## 📝 Technical Details

### **Frontend:**
- Pure JavaScript (no frameworks)
- Event delegation on `#accounts-tbody`
- Async/await for API calls
- Real-time UI updates

### **Backend:**
- Go HTTP handlers
- SQLite database
- RESTful API
- Transaction-safe updates

### **Database:**
```sql
-- Settings table
auto_join_settings (
    account_id PRIMARY KEY,
    enabled INTEGER,
    daily_limit INTEGER,
    ...
)
```

---

## ✅ Feature Summary

| Feature | Status | Description |
|---------|--------|-------------|
| Dashboard Toggle | ✅ | Checkbox di tabel akun |
| Real-time Status | ✅ | ON/OFF indicator |
| Auto-refresh | ✅ | Load status on page load |
| Visual Feedback | ✅ | Color-coded labels |
| Error Handling | ✅ | N/A for errors |
| Loading State | ✅ | "..." while loading |
| API Integration | ✅ | Full CRUD operations |

---

## 🎊 SELAMAT!

Anda sekarang dapat mengelola Auto-Join langsung dari dashboard!

**Tidak perlu lagi menggunakan curl atau API manual.** 

Cukup **klik checkbox**, dan bot akan langsung bekerja! 🚀

---

## 📚 Related Documentation

- **AUTO_JOIN_QUICK_START.md** - Quick start guide
- **AUTO_JOIN_IMPLEMENTATION.md** - Full implementation details
- **FINAL_SUMMARY.md** - Feature summary

---

**Questions?** Check the documentation atau lihat server logs untuk debugging.

**Happy Auto-Joining via Dashboard! 🎨**
