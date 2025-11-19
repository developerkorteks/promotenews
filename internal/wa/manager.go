package wa

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"promote/internal/storage"
)

type Manager struct {
	Container     *sqlstore.Container
	Clients       map[string]*whatsmeow.Client
	DB            *sql.DB
	Store         *storage.Store
	DBLogger      waLog.Logger
	ClientLogger  waLog.Logger
	pairingMu     sync.Mutex
	pairingActive map[string]bool

	// Multi-session isolation: satu sqlstore container per account
	BaseDSN    string
	Containers map[string]*sqlstore.Container
}

var ErrPairingByNumberUnsupported = errors.New("pairing via phone number unsupported by current whatsmeow")

// ParticipantInfo merepresentasikan anggota grup (user) beserta atribut admin.
type ParticipantInfo struct {
	JID          string `json:"jid"`
	Number       string `json:"number"`
	IsAdmin      bool   `json:"is_admin"`
	IsSuperAdmin bool   `json:"is_superadmin"`
}

func NewManager(ctx context.Context, dsn string, store *storage.Store) (*Manager, error) {
	dbLog := waLog.Stdout("Database", "INFO", true)
	container, err := sqlstore.New(ctx, "sqlite3", dsn, dbLog)
	if err != nil {
		return nil, err
	}
	// Naikkan level log WhatsApp ke INFO untuk observabilitas pairing
	clientLog := waLog.Stdout("WhatsApp", "INFO", true)
	return &Manager{
		Container:     container,
		Clients:       make(map[string]*whatsmeow.Client),
		DB:            store.DB,
		Store:         store,
		DBLogger:      dbLog,
		ClientLogger:  clientLog,
		pairingActive: make(map[string]bool),
		BaseDSN:       dsn,
		Containers:    make(map[string]*sqlstore.Container),
	}, nil
}

// perAccountDSN menghasilkan DSN SQLite terpisah per akun untuk mengisolasi sesi device whatsmeow.
func (m *Manager) perAccountDSN(accountID string) string {
	base := m.BaseDSN
	if base == "" {
		return fmt.Sprintf("file:promote_wa_%s.db?_foreign_keys=on", accountID)
	}
	// Pisahkan query string jika ada
	var path, q string
	if i := strings.Index(base, "?"); i >= 0 {
		path = base[:i]
		q = base[i+1:]
	} else {
		path = base
	}
	if strings.HasPrefix(path, "file:") {
		fn := strings.TrimPrefix(path, "file:")
		lfn := strings.ToLower(fn)
		if strings.HasSuffix(lfn, ".db") {
			fn = strings.TrimSuffix(fn, ".db") + "_wa_" + accountID + ".db"
		} else {
			fn = fn + "_wa_" + accountID + ".db"
		}
		dsn := "file:" + fn
		if q != "" {
			dsn = dsn + "?" + q
		}
		return dsn
	}
	// Fallback: append parameter pembeda
	if q != "" {
		return fmt.Sprintf("%s&acc=%s", base, accountID)
	}
	return fmt.Sprintf("%s?acc=%s", base, accountID)
}

func (m *Manager) ensureClient(accountID string) (*whatsmeow.Client, error) {
	if c, ok := m.Clients[accountID]; ok {
		return c, nil
	}

	// Pastikan ada container sqlstore terpisah per akun (persisten dan terisolasi)
	cont := m.Containers[accountID]
	if cont == nil {
		dsn := m.perAccountDSN(accountID)
		var err error
		cont, err = sqlstore.New(context.Background(), "sqlite3", dsn, m.DBLogger)
		if err != nil {
			return nil, err
		}
		m.Containers[accountID] = cont
	}

	// Reuse device yang sudah ada di store akun ini jika tersedia, kalau tidak buat baru
	device, err := cont.GetFirstDevice(context.Background())
	if err != nil {
		return nil, err
	}
	if device == nil {
		device = cont.NewDevice()
	}
	client := whatsmeow.NewClient(device, m.ClientLogger)

	// Update account status according to events
	client.AddEventHandler(func(evt interface{}) {
		switch evt.(type) {
		case *events.Connected:
			// best effort: update msisdn if available from store ID
			var msisdn *string
			if client.Store != nil && client.Store.ID != nil && client.Store.ID.User != "" {
				v := client.Store.ID.User
				msisdn = &v
			}
			_ = m.Store.UpdateAccountStatus(accountID, "online", "", msisdn)
		case *events.LoggedOut:
			_ = m.Store.UpdateAccountStatus(accountID, "logged_out", "", nil)
		case *events.StreamReplaced:
			_ = m.Store.UpdateAccountStatus(accountID, "replaced", "", nil)
		}
	})

	m.Clients[accountID] = client
	return client, nil
}

func (m *Manager) StartPairing(ctx context.Context, accountID string) ([]byte, string, error) {
	client, err := m.ensureClient(accountID)
	if err != nil {
		return nil, "", err
	}
	if client.Store.ID != nil {
		return nil, "", fmt.Errorf("already paired")
	}

	// Pastikan Connect hanya dipanggil sekali saat pairing untuk akun ini
	m.pairingMu.Lock()
	if !m.pairingActive[accountID] {
		m.ClientLogger.Infof("pair:qr: start connect account=%s", accountID)
		m.pairingActive[accountID] = true
		go func() {
			if err := client.Connect(); err != nil {
				m.ClientLogger.Errorf("pair:qr: connect err account=%s: %v", accountID, err)
			}
		}()
	}
	m.pairingMu.Unlock()

	// Gunakan context.Background agar QR websocket tidak tertutup saat HTTP handler selesai
	qrChan, _ := client.GetQRChannel(context.Background())
	m.ClientLogger.Infof("pair:qr: waiting code account=%s", accountID)

	for {
		select {
		case item, ok := <-qrChan:
			if !ok {
				m.ClientLogger.Errorf("pair:qr: channel closed account=%s", accountID)
				return nil, "", fmt.Errorf("qr channel closed")
			}
			if item.Event == "code" && item.Code != "" {
				png, err := qrcode.Encode(item.Code, qrcode.Medium, 256)
				if err != nil {
					return nil, "", err
				}
				m.ClientLogger.Infof("pair:qr: got code len=%d account=%s", len(item.Code), accountID)
				return png, item.Code, nil
			}
		case <-ctx.Done():
			m.ClientLogger.Errorf("pair:qr: timeout/cancel account=%s: %v", accountID, ctx.Err())
			return nil, "", ctx.Err()
		}
	}
}

func (m *Manager) ConnectIfPaired(accountID string) error {
	client, err := m.ensureClient(accountID)
	if err != nil {
		return err
	}
	if client.Store.ID == nil {
		return fmt.Errorf("not paired")
	}
	m.ClientLogger.Infof("connect: account=%s", accountID)
	// Toleransi jika sudah terkoneksi: whatsmeow.Connect() kadang mengembalikan error "already connected".
	if err := client.Connect(); err != nil {
		ls := strings.ToLower(err.Error())
		if strings.Contains(ls, "already") || strings.Contains(ls, "connected") {
			// Anggap sukses; koneksi sudah aktif.
			return nil
		}
		return err
	}
	return nil
}

/*
RequestPairingCode menghasilkan pairing code berbasis nomor (Link dengan nomor) untuk akun.
Strategi:
- Pastikan Connect hanya sekali per akun saat pairing (hindari race).
- QR channel dibuat dengan context.Background agar socket tidak tertutup ketika handler HTTP selesai.
- Tunggu event awal atau delay singkat sebelum PairPhone agar koneksi siap.
- Logging rinci untuk memudahkan debugging kasus "couldn't link device".
*/
func (m *Manager) RequestPairingCode(ctx context.Context, accountID, msisdn string) (string, error) {
	client, err := m.ensureClient(accountID)
	if err != nil {
		return "", err
	}
	if client.Store.ID != nil {
		return "", fmt.Errorf("already paired")
	}
	if msisdn == "" {
		return "", fmt.Errorf("msisdn required")
	}

	// Pastikan Connect hanya dipanggil sekali saat pairing untuk akun ini
	m.pairingMu.Lock()
	if !m.pairingActive[accountID] {
		m.ClientLogger.Infof("pair:number: start connect account=%s msisdn=%s", accountID, msisdn)
		m.pairingActive[accountID] = true
		go func() {
			if err := client.Connect(); err != nil {
				m.ClientLogger.Errorf("pair:number: connect err account=%s: %v", accountID, err)
			}
		}()
	}
	m.pairingMu.Unlock()

	// Siapkan QR channel dengan background context agar websocket pairing tetap hidup
	qrChan, _ := client.GetQRChannel(context.Background())

	// Tunggu event awal atau delay singkat supaya koneksi siap sebelum PairPhone
	select {
	case <-qrChan:
		m.ClientLogger.Infof("pair:number: initial QR event account=%s", accountID)
	case <-time.After(1 * time.Second):
		m.ClientLogger.Infof("pair:number: proceed after delay account=%s", accountID)
	case <-ctx.Done():
		m.ClientLogger.Errorf("pair:number: ctx done before code account=%s: %v", accountID, ctx.Err())
		return "", ctx.Err()
	}

	code, err := client.PairPhone(ctx, msisdn, false, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		m.ClientLogger.Errorf("pair:number: PairPhone error account=%s: %v", accountID, err)
		return "", err
	}
	_ = m.Store.UpdateAccountStatus(accountID, "pairing", "", &msisdn)
	m.ClientLogger.Infof("pair:number: got code len=%d account=%s", len(code), accountID)
	return code, nil
}

/*
GetClient returns (or creates) a whatsmeow client for an account without connecting.
*/
func (m *Manager) GetClient(accountID string) (*whatsmeow.Client, error) {
	return m.ensureClient(accountID)
}

// Logout disconnects and logs out the account device session.
func (m *Manager) Logout(accountID string) error {
	c, err := m.ensureClient(accountID)
	if err != nil {
		return err
	}
	// Putuskan koneksi websocket terlebih dahulu.
	c.Disconnect()
	// Coba logout server-side; beberapa versi bisa gagal jika sudah logout.
	if err := c.Logout(context.Background()); err != nil {
		m.ClientLogger.Errorf("logout: account=%s err=%v", accountID, err)
	}
	_ = m.Store.UpdateAccountStatus(accountID, "logged_out", "", nil)
	return nil
}

// DropAccount disconnects client and removes it from manager cache.
func (m *Manager) DropAccount(accountID string) {
	if c, ok := m.Clients[accountID]; ok && c != nil {
		c.Disconnect()
		delete(m.Clients, accountID)
	}
}

// lookupMSISDN returns the msisdn stored for an account (if any).
func (m *Manager) lookupMSISDN(accountID string) string {
	var ms sql.NullString
	_ = m.DB.QueryRow(`SELECT msisdn FROM accounts WHERE id=?`, accountID).Scan(&ms)
	if ms.Valid {
		return ms.String
	}
	return ""
}

// SendText sends a plain text message to a group JID string like "12345-67890@g.us".
func (m *Manager) SendText(ctx context.Context, accountID, groupJID, text string) error {
	c, err := m.ensureClient(accountID)
	if err != nil {
		return err
	}
	if c.Store == nil || c.Store.ID == nil {
		return fmt.Errorf("account %s not paired", accountID)
	}
	jid, err := types.ParseJID(groupJID)
	if err != nil {
		return fmt.Errorf("parse JID: %w", err)
	}
	msg := &waProto.Message{Conversation: strptr(text)}
	_, err = c.SendMessage(ctx, jid, msg)
	return err
}

// strptr returns a pointer to the given string (helper for proto messages).
func strptr(s string) *string { return &s }

// FetchAndSyncGroups obtains joined groups via WhatsApp and persists into DB.
// NOTE: This depends on whatsmeow's group list API. In case of API changes,
// adapt the mapping (name/subject) accordingly.
func (m *Manager) FetchAndSyncGroups(ctx context.Context, accountID string) (int, error) {
	client, err := m.ensureClient(accountID)
	if err != nil {
		return 0, err
	}
	if client.Store == nil || client.Store.ID == nil {
		return 0, fmt.Errorf("not paired")
	}

	// Ensure the client is connected before fetching groups.
	if err := client.Connect(); err != nil {
		// tolerate "already connected" errors
		if !strings.Contains(strings.ToLower(err.Error()), "already") {
			return 0, err
		}
	}

	// Brief settle time for session
	select {
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	// Get joined groups, retry once if it fails (e.g., right after connect)
	gmap, err := client.GetJoinedGroups(ctx)
	if err != nil {
		select {
		case <-time.After(800 * time.Millisecond):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		gmap, err = client.GetJoinedGroups(ctx)
		if err != nil {
			return 0, err
		}
	}

	count := 0
	for _, info := range gmap {
		name := info.Name
		gid := info.JID.String()
		if gid == "" {
			continue
		}
		if err := m.Store.UpsertGroup(accountID, gid, name); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// GetGroupParticipants mengambil daftar anggota (user) pada sebuah grup untuk akun tertentu.
// Mengembalikan slice berisi JID, nomor (user), dan flag admin.
// Menggunakan strategi cache-first untuk response cepat, dengan fallback ke network request.
func (m *Manager) GetGroupParticipants(ctx context.Context, accountID, groupJID string) ([]ParticipantInfo, error) {
	client, err := m.ensureClient(accountID)
	if err != nil {
		return nil, err
	}
	if client.Store == nil || client.Store.ID == nil {
		return nil, fmt.Errorf("not paired")
	}

	jid, err := types.ParseJID(groupJID)
	if err != nil {
		return nil, fmt.Errorf("parse JID: %w", err)
	}

	// Strategy 1: Check database cache first (very fast, no network needed)
	// Cache valid for 24 hours (1440 minutes) - adjust as needed
	participants, err := m.getCachedParticipants(ctx, groupJID)
	if err == nil && len(participants) > 0 {
		m.ClientLogger.Infof("participants: using cache for group %s (%d members)", groupJID, len(participants))
		return participants, nil
	}

	// Strategy 2: Cache miss or expired, fetch from WhatsApp
	// Check if already connected to avoid unnecessary reconnection
	if !client.IsConnected() {
		m.ClientLogger.Infof("participants: connecting client for account %s", accountID)
		if err := client.Connect(); err != nil {
			return nil, fmt.Errorf("failed to connect: %w", err)
		}
		
		// Wait for connection to stabilize before making queries
		select {
		case <-time.After(3 * time.Second):
			m.ClientLogger.Infof("participants: connection established for account %s", accountID)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	} else {
		m.ClientLogger.Infof("participants: already connected for account %s", accountID)
	}

	// Try lightweight method dengan timeout pendek untuk response cepat
	participants, err = m.fetchAndCacheParticipants(ctx, client, jid, groupJID)
	if err != nil {
		return nil, err
	}

	return participants, nil
}

// getCachedParticipants mengambil participants dari database cache
func (m *Manager) getCachedParticipants(ctx context.Context, groupJID string) ([]ParticipantInfo, error) {
	// Cache valid for 24 hours (1440 minutes)
	cached, found, err := m.Store.GetCachedGroupParticipants(groupJID, 1440)
	if err != nil || !found {
		return nil, fmt.Errorf("cache miss or error")
	}

	// Convert to ParticipantInfo
	participants := make([]ParticipantInfo, 0, len(cached))
	for _, p := range cached {
		participants = append(participants, ParticipantInfo{
			JID:          p.JID,
			Number:       p.Number,
			IsAdmin:      p.IsAdmin,
			IsSuperAdmin: p.IsSuperAdmin,
		})
	}

	return participants, nil
}

// fetchAndCacheParticipants fetches participants from WhatsApp and caches them
func (m *Manager) fetchAndCacheParticipants(ctx context.Context, client *whatsmeow.Client, jid types.JID, groupJID string) ([]ParticipantInfo, error) {
	m.ClientLogger.Infof("participants: fetching from WhatsApp for group %s", groupJID)
	
	// Use longer timeout for initial testing (30 seconds) - can be reduced after testing
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	
	info, err := client.GetGroupInfo(ctx2, jid)
	if err != nil {
		// Check for specific errors and provide helpful messages
		errMsg := err.Error()
		if strings.Contains(strings.ToLower(errMsg), "timeout") {
			return nil, fmt.Errorf("grup tidak dapat diakses (timeout) - mungkin grup sudah tidak aktif atau Anda bukan anggota")
		} else if strings.Contains(strings.ToLower(errMsg), "not found") {
			return nil, fmt.Errorf("grup tidak ditemukan - mungkin grup sudah dihapus atau ID salah")
		} else if strings.Contains(strings.ToLower(errMsg), "forbidden") {
			return nil, fmt.Errorf("tidak ada akses ke grup - mungkin Anda sudah dikeluarkan dari grup")
		}
		return nil, fmt.Errorf("gagal mengambil info grup: %v", err)
	}
	
	// Convert to ParticipantInfo
	participants := make([]ParticipantInfo, 0, len(info.Participants))
	for _, p := range info.Participants {
		participants = append(participants, ParticipantInfo{
			JID:          p.JID.String(),
			Number:       p.JID.User,
			IsAdmin:      p.IsAdmin,
			IsSuperAdmin: p.IsSuperAdmin,
		})
	}
	
	// Cache the results for next time
	cacheData := make([]struct {
		JID          string
		Number       string
		IsAdmin      bool
		IsSuperAdmin bool
	}, len(participants))
	
	for i, p := range participants {
		cacheData[i].JID = p.JID
		cacheData[i].Number = p.Number
		cacheData[i].IsAdmin = p.IsAdmin
		cacheData[i].IsSuperAdmin = p.IsSuperAdmin
	}
	
	// Best effort cache save - don't fail if caching fails
	if err := m.Store.CacheGroupParticipants(groupJID, cacheData); err != nil {
		m.ClientLogger.Errorf("participants: failed to cache for group %s: %v", groupJID, err)
	} else {
		m.ClientLogger.Infof("participants: cached %d members for group %s", len(participants), groupJID)
	}
	
	return participants, nil
}
