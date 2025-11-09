package scheduler

import (
	"context"
	"database/sql"
	"log"
	"math/rand"
	"time"

	"promote/internal/sender"
	"promote/internal/storage"
	"promote/internal/wa"
)

// Scheduler menjalankan broadcast terjadwal anti-spam:
// - Jendela waktu aman (WIB): 00:45–02:30, 03:00–05:30, 21:30–23:30 (opsional kecil)
// - Limit harian per akun: memakai accounts.daily_limit
// - Cooldown per grup: minimal 48 jam
// - Jitter antar grup: 45–120 detik random
// - Variasi konten: pilih template aktif secara acak via Sender
// - Risk: sender.bumpRiskAndMaybePause akan auto-disable grup berisiko
type Scheduler struct {
	Store   *storage.Store
	Manager *wa.Manager
	Sender  *sender.Sender

	loc        *time.Location
	running    bool
	stop       chan struct{}
	cooldownHr int
	// Jendela waktu dalam menit dari tengah malam (WIB)
	// Format: [startMin, endMin]
	windows [][2]int
	// Jitter antar kirim (detik)
	minDelaySec int
	maxDelaySec int
	// Risk threshold untuk filter grup
	riskThreshold int
}

// New membuat instance Scheduler dengan konfigurasi default konservatif.
func New(store *storage.Store, manager *wa.Manager, snd *sender.Sender) *Scheduler {
	loc, _ := time.LoadLocation("Asia/Jakarta")
	return &Scheduler{
		Store:         store,
		Manager:       manager,
		Sender:        snd,
		loc:           loc,
		stop:          make(chan struct{}),
		cooldownHr:    48,
		windows:       [][2]int{{45, 150}, {180, 330}, {1290, 1410}}, // 00:45–02:30, 03:00–05:30, 21:30–23:30 WIB
		minDelaySec:   45,
		maxDelaySec:   120,
		riskThreshold: 3,
	}
}

// Start menjalankan loop scheduler di goroutine.
// Panggil Stop() untuk menghentikan.
func (s *Scheduler) Start(ctx context.Context) {
	if s.running {
		return
	}
	s.running = true
	go s.loop(ctx)
}

// Stop menghentikan scheduler.
func (s *Scheduler) Stop() {
	if !s.running {
		return
	}
	close(s.stop)
	s.running = false
}

func (s *Scheduler) loop(ctx context.Context) {
	defer func() {
		s.running = false
	}()
	// Ticker utama: cek setiap 30 detik
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		case <-tick.C:
			// Jalankan satu siklus jika dalam jendela waktu aman
			now := time.Now().In(s.loc)
			if !s.inWindow(now) {
				continue
			}
			// Proses: satu kirim maksimum setiap siklus (menghindari burst)
			if err := s.processOneSend(ctx, now); err != nil {
				// Log saja dan lanjut; kesalahan akan ditangani risk handler sender
				log.Printf("[scheduler] process error: %v", err)
			}
		}
	}
}

// processOneSend memilih satu akun yang masih di bawah limit harian,
// lalu memilih satu grup yang memenuhi syarat, kemudian kirim menggunakan template acak.
// Setelah kirim, jeda random 45–120 detik agar natural.
func (s *Scheduler) processOneSend(ctx context.Context, now time.Time) error {
	// 1) Ambil akun aktif (enabled)
	accs, err := s.listEnabledAccounts()
	if err != nil {
		return err
	}
	if len(accs) == 0 {
		return nil
	}

	// Randomisasi urutan akun untuk pemerataan
	rand.Shuffle(len(accs), func(i, j int) { accs[i], accs[j] = accs[j], accs[i] })

	for _, a := range accs {
		// Pastikan akun paired & siap connect (best-effort)
		if err := s.Manager.ConnectIfPaired(a.ID); err != nil {
			// skip akun yang belum paired
			continue
		}
		// 2) Cek limit harian akun (sent hari ini)
		sentToday, err := s.countSentTodayForAccount(a.ID)
		if err != nil {
			continue
		}
		if a.DailyLimit <= 0 {
			a.DailyLimit = 100
		}
		if int(sentToday) >= a.DailyLimit {
			// limit tercapai; lanjut akun lain
			continue
		}
		// 3) Pilih grup satu yang eligible untuk dikirim sekarang
		groupID, err := s.pickOneEligibleGroup(a.ID, s.cooldownHr, s.riskThreshold)
		if err != nil {
			continue
		}
		if groupID == "" {
			// tidak ada grup eligible di akun ini saat ini, lanjut akun lain
			continue
		}
		// 4) Kirim menggunakan template acak (sender sudah tangani pacing antar bagian)
		sendCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		err = s.Sender.SendToGroupUsingRandomTemplate(sendCtx, a.ID, groupID)
		cancel()
		// Jika gagal, sender akan bump risk dan mungkin auto-disable grup
		if err != nil {
			// Setelah gagal, tetap jeda sebentar untuk naturalness
			s.sleepBetweenGroups(ctx)
			// lanjut akun lain setelah jeda
			continue
		}
		// 5) Jeda antar grup (jitter 45–120 detik)
		s.sleepBetweenGroups(ctx)
		// Satu kirim per siklus; keluar supaya tidak burst
		return nil
	}

	return nil
}

func (s *Scheduler) sleepBetweenGroups(ctx context.Context) {
	delay := s.randDelay()
	select {
	case <-time.After(delay):
	case <-ctx.Done():
	}
}

func (s *Scheduler) randDelay() time.Duration {
	min := s.minDelaySec
	max := s.maxDelaySec
	if max < min {
		max, min = min, max
	}
	// 45–120 detik random
	return time.Duration(min+rand.Intn(max-min+1)) * time.Second
}

func (s *Scheduler) inWindow(t time.Time) bool {
	// menit dari tengah malam (WIB)
	m := t.Hour()*60 + t.Minute()
	for _, w := range s.windows {
		if m >= w[0] && m <= w[1] {
			return true
		}
	}
	return false
}

type accountLite struct {
	ID         string
	DailyLimit int
}

func (s *Scheduler) listEnabledAccounts() ([]accountLite, error) {
	rows, err := s.Store.DB.Query(`SELECT id, daily_limit FROM accounts WHERE enabled=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []accountLite
	for rows.Next() {
		var a accountLite
		if err := rows.Scan(&a.ID, &a.DailyLimit); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *Scheduler) countSentTodayForAccount(accountID string) (int64, error) {
	var n int64
	err := s.Store.DB.QueryRow(`
		SELECT COALESCE(SUM(CASE WHEN status='sent' THEN 1 ELSE 0 END), 0)
		FROM logs
		WHERE account_id=? AND ts >= datetime('now','start of day') AND ts < datetime('now','start of day','+1 day')
	`, accountID).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Scheduler) pickOneEligibleGroup(accountID string, cooldownHours int, riskThreshold int) (string, error) {
	// Ambil satu grup yang:
	// - enabled=1
	// - last_sent_at lebih lama dari cooldownHours atau NULL
	// - risk_score < threshold
	// - random
	var (
		id sql.NullString
	)
	err := s.Store.DB.QueryRow(`
		SELECT id
		FROM groups
		WHERE account_id=? AND enabled=1 AND (last_sent_at IS NULL OR last_sent_at < datetime('now', ?)) AND risk_score < ?
		ORDER BY RANDOM()
		LIMIT 1
	`, accountID, "-"+itoa(cooldownHours)+" hours", riskThreshold).Scan(&id)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	if id.Valid {
		return id.String, nil
	}
	return "", nil
}

func itoa(i int) string {
	return fmtInt(i)
}

// fmtInt adalah helper sederhana untuk menghindari import strconv secara eksplisit
func fmtInt(i int) string {
	// manual convert int to string (fast path untuk nilai kecil)
	// untuk kesederhanaan, gunakan strconv jika diperlukan di masa depan
	switch {
	case i == 0:
		return "0"
	case i == 1:
		return "1"
	case i == 2:
		return "2"
	case i == 3:
		return "3"
	case i == 4:
		return "4"
	case i == 5:
		return "5"
	case i == 6:
		return "6"
	case i == 7:
		return "7"
	case i == 8:
		return "8"
	case i == 9:
		return "9"
	}
	// fallback gunakan strconv
	// Untuk menjaga dependency ringan, implementasi minimal berikut:
	sign := ""
	if i < 0 {
		sign = "-"
		i = -i
	}
	// pecah ke digit
	var digits [20]byte
	pos := len(digits)
	for i > 0 {
		pos--
		digits[pos] = byte('0' + (i % 10))
		i /= 10
	}
	return sign + string(digits[pos:])
}
