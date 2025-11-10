package scheduler

import (
	"context"
	"database/sql"
	"log"
	"math/rand"
	"os"
	"strconv"
	"strings"
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
	// Override agar inWindow selalu true (uji/ops): SCHEDULER_ALWAYS_ON=1|true|yes
	alwaysOn bool
}

// New membuat instance Scheduler dengan konfigurasi default konservatif.
func New(store *storage.Store, manager *wa.Manager, snd *sender.Sender) *Scheduler {
	// Pastikan zona waktu WIB tersedia. Jika tzdata tidak terpasang di VPS,
	// time.LoadLocation("Asia/Jakarta") bisa gagal. Fallback ke zona tetap +07:00.
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil || loc == nil {
		loc = time.FixedZone("WIB", 7*3600)
	}

	s := &Scheduler{
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
		alwaysOn:      false,
	}

	// ENV overrides (ops):
	// - SCHEDULER_ALWAYS_ON=1|true|yes  -> jalankan kapan saja (abaikan window)
	// - SCHEDULER_COOLDOWN_HOURS=int    -> override cooldown antar kirim ke grup yang sama
	// - SCHEDULER_MIN_DELAY_SEC=int     -> delay min antar grup
	// - SCHEDULER_MAX_DELAY_SEC=int     -> delay max antar grup
	// - SCHEDULER_RISK_THRESHOLD=int    -> ambang risk_score untuk filter/auto-disable
	if v := os.Getenv("SCHEDULER_ALWAYS_ON"); v != "" {
		vv := strings.ToLower(strings.TrimSpace(v))
		if vv == "1" || vv == "true" || vv == "yes" {
			s.alwaysOn = true
		}
	}
	if v := os.Getenv("SCHEDULER_COOLDOWN_HOURS"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			s.cooldownHr = n
		}
	}
	if v := os.Getenv("SCHEDULER_MIN_DELAY_SEC"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			s.minDelaySec = n
		}
	}
	if v := os.Getenv("SCHEDULER_MAX_DELAY_SEC"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			s.maxDelaySec = n
		}
	}
	if v := os.Getenv("SCHEDULER_RISK_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			s.riskThreshold = n
		}
	}

	return s
}

// Start menjalankan loop scheduler di goroutine.
// Panggil Stop() untuk menghentikan.
func (s *Scheduler) Start(ctx context.Context) {
	if s.running {
		return
	}
	s.running = true
	// Log awal untuk diagnosis: pastikan timezone & jendela waktu terbaca benar
	log.Printf("[scheduler] start: tz=%s now=%s windows=%v alwaysOn=%v cooldownHr=%d minDelay=%ds maxDelay=%ds riskThreshold=%d",
		s.loc.String(),
		time.Now().In(s.loc).Format(time.RFC3339),
		s.windows,
		s.alwaysOn,
		s.cooldownHr,
		s.minDelaySec,
		s.maxDelaySec,
		s.riskThreshold,
	)
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
			inWindow := s.inWindow(now)
			if !inWindow {
				ns, ne, dur := s.nextWindow(now)
				log.Printf("[scheduler] tick: now=%s in_window=%v next_window=%02d:%02d-%02d:%02d in=%s alwaysOn=%v",
					now.Format("2006-01-02 15:04:05"),
					inWindow,
					ns/60, ns%60, ne/60, ne%60,
					dur.String(),
					s.alwaysOn,
				)
				if !s.alwaysOn {
					continue
				}
			} else {
				log.Printf("[scheduler] tick: now=%s in_window=%v alwaysOn=%v", now.Format("2006-01-02 15:04:05"), inWindow, s.alwaysOn)
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
	log.Printf("[scheduler] accounts_enabled=%d", len(accs))
	if len(accs) == 0 {
		return nil
	}

	// Randomisasi urutan akun untuk pemerataan
	rand.Shuffle(len(accs), func(i, j int) { accs[i], accs[j] = accs[j], accs[i] })

	for _, a := range accs {
		// Pastikan akun paired & siap connect (best-effort)
		if err := s.Manager.ConnectIfPaired(a.ID); err != nil {
			// skip akun yang belum paired
			log.Printf("[scheduler] account=%s connectIfPaired=skip err=%v", a.ID, err)
			continue
		}
		// 2) Cek limit harian akun (sent hari ini)
		sentToday, err := s.countSentTodayForAccount(a.ID)
		if err != nil {
			log.Printf("[scheduler] account=%s sentToday-query-err=%v", a.ID, err)
			continue
		}
		if a.DailyLimit <= 0 {
			a.DailyLimit = 100
		}
		if int(sentToday) >= a.DailyLimit {
			// limit tercapai; lanjut akun lain
			log.Printf("[scheduler] account=%s sentToday=%d dailyLimit=%d -> skip (limit reached)", a.ID, sentToday, a.DailyLimit)
			continue
		}

		// Logging eligible groups count
		eligibleCnt, err := s.countEligibleGroups(a.ID, s.cooldownHr, s.riskThreshold)
		if err != nil {
			log.Printf("[scheduler] account=%s eligible-count-err=%v", a.ID, err)
		} else {
			log.Printf("[scheduler] account=%s eligible_groups=%d", a.ID, eligibleCnt)
		}

		// 3) Pilih grup satu yang eligible untuk dikirim sekarang
		groupID, err := s.pickOneEligibleGroup(a.ID, s.cooldownHr, s.riskThreshold)
		if err != nil {
			log.Printf("[scheduler] account=%s pick-group-err=%v", a.ID, err)
			continue
		}
		if groupID == "" {
			// tidak ada grup eligible di akun ini saat ini, lanjut akun lain
			log.Printf("[scheduler] account=%s pick-group=none", a.ID)
			continue
		}
		log.Printf("[scheduler] account=%s group=%s -> sending with random template...", a.ID, groupID)

		// 4) Kirim menggunakan template acak (sender sudah tangani pacing antar bagian)
		sendCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		err = s.Sender.SendToGroupUsingRandomTemplate(sendCtx, a.ID, groupID)
		cancel()
		// Jika gagal, sender akan bump risk dan mungkin auto-disable grup
		if err != nil {
			log.Printf("[scheduler] send failed account=%s group=%s err=%v", a.ID, groupID, err)
			// Setelah gagal, tetap jeda sebentar untuk naturalness
			s.sleepBetweenGroups(ctx)
			// lanjut akun lain setelah jeda
			continue
		}
		log.Printf("[scheduler] send success account=%s group=%s", a.ID, groupID)

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
	// Ops override: jalankan kapan saja jika diaktifkan
	if s.alwaysOn {
		return true
	}
	// menit dari tengah malam (WIB)
	m := t.Hour()*60 + t.Minute()
	for _, w := range s.windows {
		if m >= w[0] && m <= w[1] {
			return true
		}
	}
	return false
}

// nextWindow mengembalikan pasangan [startMin,endMin] untuk window berikutnya
// relatif terhadap waktu t (WIB) dan durasi menuju start window tersebut.
func (s *Scheduler) nextWindow(t time.Time) (startMin int, endMin int, until time.Duration) {
	m := t.Hour()*60 + t.Minute()
	var nextStart, nextEnd int
	found := false
	for _, w := range s.windows {
		if m <= w[0] {
			nextStart = w[0]
			nextEnd = w[1]
			found = true
			break
		}
	}
	if !found {
		// gunakan window pertama pada hari berikutnya
		nextStart = s.windows[0][0]
		nextEnd = s.windows[0][1]
	}
	// hitung durasi menuju nextStart
	// konversi menit dari tengah malam ke time.Time untuk hari ini/tomorrow
	dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, s.loc)
	nextStartTime := dayStart.Add(time.Duration(nextStart) * time.Minute)
	if nextStartTime.Before(t) {
		nextStartTime = nextStartTime.Add(24 * time.Hour)
	}
	return nextStart, nextEnd, nextStartTime.Sub(t)
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

func (s *Scheduler) countEligibleGroups(accountID string, cooldownHours int, riskThreshold int) (int64, error) {
	var n int64
	err := s.Store.DB.QueryRow(`
		SELECT COUNT(*)
		FROM groups
		WHERE account_id=? AND enabled=1 AND (last_sent_at IS NULL OR last_sent_at < datetime('now', ?)) AND risk_score < ?
	`, accountID, "-"+itoa(cooldownHours)+" hours", riskThreshold).Scan(&n)
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
