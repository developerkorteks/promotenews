package wa

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
}

var ErrPairingByNumberUnsupported = errors.New("pairing via phone number unsupported by current whatsmeow")

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
	}, nil
}

func (m *Manager) ensureClient(accountID string) (*whatsmeow.Client, error) {
	if c, ok := m.Clients[accountID]; ok {
		return c, nil
	}
	device := m.Container.NewDevice()
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
	return client.Connect()
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

// GetClient returns (or creates) a whatsmeow client for an account without connecting.
func (m *Manager) GetClient(accountID string) (*whatsmeow.Client, error) {
	return m.ensureClient(accountID)
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

	// whatsmeow currently provides GetJoinedGroups(ctx) which returns a map keyed by JID.
	// We only need group ID (string) and name/subject for display.
	gmap, err := client.GetJoinedGroups(ctx)
	if err != nil {
		return 0, err
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
