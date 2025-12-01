# Quick Start Guide - Optimasi Participants

## 🚀 Cara Cepat Mulai Menggunakan Optimasi

### 1. Build & Run Server
```bash
# Build aplikasi
go build -o main

# Jalankan server
./main
```

Server akan otomatis:
- ✅ Membuat tabel cache `group_participants` 
- ✅ Mengaktifkan sistem caching otomatis
- ✅ Mengurangi timeout dari 120s ke 15s

### 2. Test Performa (Opsional)

**Tanpa script benchmark:**
```bash
# Request pertama (akan fetch dari WhatsApp)
time curl "http://localhost:9724/api/accounts/YOUR_ACCOUNT_ID/groups/GROUP_JID%40g.us/participants" | jq

# Request kedua (dari cache - harus jauh lebih cepat!)
time curl "http://localhost:9724/api/accounts/YOUR_ACCOUNT_ID/groups/GROUP_JID%40g.us/participants" | jq
```

**Dengan script benchmark:**
```bash
./tmp_rovodev_benchmark_participants.sh "YOUR_ACCOUNT_ID" "GROUP_JID@g.us"
```

### 3. Force Refresh (Jika Perlu Data Terbaru)

Jika ingin memaksa ambil data fresh dari WhatsApp (misalnya ada anggota baru):
```bash
curl -X POST "http://localhost:9724/api/accounts/YOUR_ACCOUNT_ID/groups/GROUP_JID%40g.us/participants/refresh"
```

## 🎯 Apa yang Berubah untuk User?

### Dari UI/Dashboard:
1. Klik tombol "Anggota" pada grup → **Langsung muncul** (bukan tunggu 2 menit)
2. Klik lagi → **Instant** dari cache
3. Jika ingin data terbaru → Manual refresh melalui API

### Dari API:
- Endpoint sama: `GET /api/accounts/{id}/groups/{gid}/participants`
- Response format sama
- Hanya lebih cepat: 1200x lebih cepat untuk request cached!

## 📊 Ekspektasi Performa

| Request ke- | Waktu Response | Sumber Data |
|-------------|----------------|-------------|
| 1 (cache miss) | ~15 detik | WhatsApp API |
| 2-N (dalam 24 jam) | <0.1 detik | Database Cache |
| Force refresh | ~15 detik | WhatsApp API (+ update cache) |

## 🔧 Konfigurasi (Opsional)

Jika ingin mengubah durasi cache, edit file `internal/wa/manager.go`:

```go
// Line ~416 di getCachedParticipants()
cached, found, err := m.Store.GetCachedGroupParticipants(groupJID, 1440) // 1440 = 24 jam

// Ubah angka 1440 ke nilai yang diinginkan (dalam menit):
// 60 = 1 jam
// 720 = 12 jam  
// 1440 = 24 jam (default)
// 4320 = 3 hari
```

Jika ingin mengubah timeout network request, edit line ~465:

```go
ctx2, cancel := context.WithTimeout(ctx, 15*time.Second) // 15 detik

// Ubah ke nilai yang diinginkan, misalnya:
ctx2, cancel := context.WithTimeout(ctx, 30*time.Second) // 30 detik
```

## 🐛 Troubleshooting

### "Cache tidak bekerja, masih lambat setiap request"
- Cek apakah database memiliki tabel `group_participants`:
  ```bash
  sqlite3 promote.db "SELECT name FROM sqlite_master WHERE type='table' AND name='group_participants';"
  ```
- Jika tidak ada, restart aplikasi untuk trigger migration

### "Error: cache miss or error"
- Ini normal untuk request pertama
- Data akan di-cache setelah fetch berhasil dari WhatsApp

### "Timeout masih terjadi"
- Pastikan akun WhatsApp terkoneksi: cek status di dashboard
- Pastikan grup masih aktif dan akun masih menjadi member
- Untuk grup sangat besar (500+ anggota), pertimbangkan naikkan timeout

## 📝 Log yang Perlu Diperhatikan

Ketika sistem bekerja dengan baik, log akan menunjukkan:

```
# Request pertama (cache miss)
INFO: participants: fetching from WhatsApp for group 120363...@g.us
INFO: participants: cached 45 members for group 120363...@g.us

# Request berikutnya (cache hit)
INFO: participants: using cache for group 120363...@g.us (45 members)
```

## ✅ Verifikasi Optimasi Berhasil

Cara cepat cek apakah optimasi berfungsi:

```bash
# 1. Test request pertama (akan lambat, fetch dari WhatsApp)
time curl -s "http://localhost:9724/api/accounts/ID/groups/GID/participants" > /dev/null
# Output: real 0m15.xxx s

# 2. Test request kedua (harus cepat, dari cache)  
time curl -s "http://localhost:9724/api/accounts/ID/groups/GID/participants" > /dev/null
# Output: real 0m0.0xx s  ← Jika ini terjadi, optimasi BERHASIL! 🎉
```

## 🎉 Selesai!

Sistem sekarang akan:
- ✅ Response cepat untuk data yang sudah pernah diakses
- ✅ Tidak perlu tunggu lama setiap kali lihat anggota grup
- ✅ Mengurangi beban ke WhatsApp API
- ✅ User experience jauh lebih baik

**Tidak perlu konfigurasi tambahan - sistem langsung bekerja otomatis!**
