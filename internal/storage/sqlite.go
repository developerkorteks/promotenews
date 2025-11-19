package storage

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"

	"promote/internal/model"
)

type Store struct {
	DB *sql.DB
}

// Open opens/initializes SQLite database with WAL and foreign keys, then migrates schema.
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		// continue; non-fatal
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON;`); err != nil {
		// continue; non-fatal
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{DB: db}, nil
}

// Close closes underlying DB.
func (s *Store) Close() error { return s.DB.Close() }

func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmts := []string{
		`PRAGMA foreign_keys = ON;`,
		`CREATE TABLE IF NOT EXISTS accounts (
			id TEXT PRIMARY KEY,
			label TEXT NOT NULL,
			msisdn TEXT,
			enabled INTEGER NOT NULL DEFAULT 1,
			daily_limit INTEGER NOT NULL DEFAULT 100,
			status TEXT NOT NULL DEFAULT 'inactive',
			last_error TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS groups (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			name TEXT,
			enabled INTEGER NOT NULL DEFAULT 0,
			last_sent_at TIMESTAMP,
			risk_score INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS campaigns (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			text TEXT,
			media_images TEXT,
			media_videos TEXT,
			enabled INTEGER NOT NULL DEFAULT 0,
			per_group_variants INTEGER NOT NULL DEFAULT 1,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS schedules (
			id TEXT PRIMARY KEY,
			campaign_id TEXT NOT NULL,
			account_id TEXT NOT NULL,
			batch_size INTEGER NOT NULL DEFAULT 50,
			start_hour INTEGER NOT NULL DEFAULT 0,
			end_hour INTEGER NOT NULL DEFAULT 24,
			min_delay_sec INTEGER NOT NULL DEFAULT 30,
			max_delay_sec INTEGER NOT NULL DEFAULT 120,
			days_mask TEXT,
			daily_limit INTEGER NOT NULL DEFAULT 100,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY(campaign_id) REFERENCES campaigns(id) ON DELETE CASCADE,
			FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			account_id TEXT,
			group_id TEXT,
			campaign_id TEXT,
			campaign_session_id TEXT,
			status TEXT,
			error TEXT,
			message_preview TEXT,
			attempt INTEGER NOT NULL DEFAULT 1,
			scheduled_for TIMESTAMP,
			FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE SET NULL,
			FOREIGN KEY(group_id) REFERENCES groups(id) ON DELETE SET NULL,
			FOREIGN KEY(campaign_id) REFERENCES campaigns(id) ON DELETE SET NULL
		);`,
		`CREATE TABLE IF NOT EXISTS templates (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			text_only TEXT,
			images_json TEXT,
			images_caption TEXT,
			videos_json TEXT,
			videos_caption TEXT,
			stickers_json TEXT,
			docs_json TEXT,
			docs_caption TEXT,
			audio_json TEXT,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE INDEX IF NOT EXISTS idx_groups_account ON groups(account_id);`,
		`CREATE INDEX IF NOT EXISTS idx_logs_campaign_ts ON logs(campaign_id, ts);`,
		`CREATE INDEX IF NOT EXISTS idx_logs_group_ts ON logs(group_id, ts);`,
		`CREATE INDEX IF NOT EXISTS idx_templates_enabled ON templates(enabled);`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			tx.Rollback()
			return err
		}
	}
	// Best-effort schema upgrade for existing installations: add audio_json if missing.
	_, _ = tx.Exec(`ALTER TABLE templates ADD COLUMN audio_json TEXT;`)
	// Add campaign_session_id column if missing
	_, _ = tx.Exec(`ALTER TABLE logs ADD COLUMN campaign_session_id TEXT;`)
	
	// Migrate from old schema to new caption-per-media schema
	_, _ = tx.Exec(`ALTER TABLE templates ADD COLUMN text_only TEXT;`)
	_, _ = tx.Exec(`ALTER TABLE templates ADD COLUMN images_caption TEXT;`)
	_, _ = tx.Exec(`ALTER TABLE templates ADD COLUMN videos_caption TEXT;`)
	_, _ = tx.Exec(`ALTER TABLE templates ADD COLUMN docs_caption TEXT;`)
	
	// Create group_participants cache table for fast retrieval
	_, _ = tx.Exec(`CREATE TABLE IF NOT EXISTS group_participants (
		group_id TEXT NOT NULL,
		jid TEXT NOT NULL,
		number TEXT NOT NULL,
		is_admin INTEGER NOT NULL DEFAULT 0,
		is_superadmin INTEGER NOT NULL DEFAULT 0,
		cached_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (group_id, jid),
		FOREIGN KEY(group_id) REFERENCES groups(id) ON DELETE CASCADE
	)`)
	_, _ = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_group_participants_group ON group_participants(group_id);`)
	_, _ = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_group_participants_cached ON group_participants(group_id, cached_at);`)
	
	// Migrate existing 'text' column to appropriate caption columns for backward compatibility
	_, _ = tx.Exec(`UPDATE templates SET 
		text_only = text,
		images_caption = CASE WHEN images_json IS NOT NULL AND images_json != '[]' THEN text ELSE NULL END,
		videos_caption = CASE WHEN videos_json IS NOT NULL AND videos_json != '[]' THEN text ELSE NULL END,
		docs_caption = CASE WHEN docs_json IS NOT NULL AND docs_json != '[]' THEN text ELSE NULL END
		WHERE text_only IS NULL`)
	
	// Remove old text column after migration (optional, commented for safety)
	// _, _ = tx.Exec(`ALTER TABLE templates DROP COLUMN text;`)
	return tx.Commit()
}

// CreateAccount inserts a new account and returns its generated ID.
func (s *Store) CreateAccount(label, msisdn string, enabled bool, dailyLimit int) (string, error) {
	if dailyLimit <= 0 {
		dailyLimit = 100
	}
	id := uuid.NewString()
	now := time.Now()
	_, err := s.DB.Exec(`INSERT INTO accounts (id,label,msisdn,enabled,daily_limit,status,last_error,created_at,updated_at)
		VALUES (?,?,?,?,?,'inactive','',?,?)`,
		id, label, msisdn, btoi(enabled), dailyLimit, now, now)
	if err != nil {
		return "", err
	}
	return id, nil
}

// ListAccounts returns all accounts ordered by created_at desc.
func (s *Store) ListAccounts() ([]model.Account, error) {
	rows, err := s.DB.Query(`SELECT id,label,msisdn,enabled,daily_limit,status,COALESCE(last_error,''),created_at,updated_at FROM accounts ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []model.Account
	for rows.Next() {
		var a model.Account
		var enabledInt int
		if err := rows.Scan(&a.ID, &a.Label, &a.Msisdn, &enabledInt, &a.DailyLimit, &a.Status, &a.LastError, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.Enabled = enabledInt == 1
		list = append(list, a)
	}
	return list, nil
}

func (s *Store) AccountExists(id string) (bool, error) {
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(1) FROM accounts WHERE id=?`, id).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) UpdateAccountStatus(id, status, lastError string, msisdnOpt *string) error {
	if msisdnOpt != nil {
		_, err := s.DB.Exec(`UPDATE accounts SET status=?, last_error=?, msisdn=COALESCE(NULLIF(?, ''), msisdn), updated_at=CURRENT_TIMESTAMP WHERE id=?`,
			status, lastError, *msisdnOpt, id)
		return err
	}
	_, err := s.DB.Exec(`UPDATE accounts SET status=?, last_error=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		status, lastError, id)
	return err
}

// UpsertGroup inserts/updates group record for an account.
func (s *Store) UpsertGroup(accountID, groupID, name string) error {
	_, err := s.DB.Exec(`
		INSERT INTO groups (id, account_id, name, enabled, created_at)
		VALUES (?,?,?,?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			account_id=excluded.account_id,
			name=COALESCE(NULLIF(excluded.name,''), groups.name)
	`, groupID, accountID, name, 0)
	return err
}

func (s *Store) ListGroups(accountID string) ([]model.Group, error) {
	var rows *sql.Rows
	var err error
	if accountID != "" {
		rows, err = s.DB.Query(`SELECT id,account_id,name,enabled,last_sent_at,risk_score,created_at FROM groups WHERE account_id=? ORDER BY name`, accountID)
	} else {
		rows, err = s.DB.Query(`SELECT id,account_id,name,enabled,last_sent_at,risk_score,created_at FROM groups ORDER BY name`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []model.Group
	for rows.Next() {
		var g model.Group
		var enabled int
		var lastSent sql.NullTime
		if err := rows.Scan(&g.ID, &g.AccountID, &g.Name, &enabled, &lastSent, &g.RiskScore, &g.CreatedAt); err != nil {
			return nil, err
		}
		g.Enabled = enabled == 1
		if lastSent.Valid {
			t := lastSent.Time
			g.LastSentAt = &t
		}
		res = append(res, g)
	}
	return res, nil
}

func (s *Store) ToggleGroup(groupID string, enabled bool) (int64, error) {
	res, err := s.DB.Exec(`UPDATE groups SET enabled=? WHERE id=?`, btoi(enabled), groupID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) StatsToday() (total, success, failed int64, err error) {
	row := s.DB.QueryRow(`
		SELECT
			COUNT(*) AS total,
			COALESCE(SUM(CASE WHEN status='sent' THEN 1 ELSE 0 END), 0) AS success,
			COALESCE(SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END), 0) AS failed
		FROM logs
		WHERE ts >= datetime('now','start of day') AND ts < datetime('now','start of day','+1 day')`)
	if err := row.Scan(&total, &success, &failed); err != nil {
		return 0, 0, 0, err
	}
	return total, success, failed, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// UpdateAccount memperbarui label, msisdn, enabled, dan daily_limit sebuah akun.
func (s *Store) UpdateAccount(id, label, msisdn string, enabled bool, dailyLimit int) error {
	if dailyLimit <= 0 {
		dailyLimit = 100
	}
	_, err := s.DB.Exec(`UPDATE accounts 
		SET label=?, msisdn=?, enabled=?, daily_limit=?, updated_at=CURRENT_TIMESTAMP 
		WHERE id=?`,
		label, msisdn, btoi(enabled), dailyLimit, id)
	return err
}

// DeleteAccount menghapus akun. Relasi groups akan ikut terhapus karena ON DELETE CASCADE.
func (s *Store) DeleteAccount(id string) error {
	_, err := s.DB.Exec(`DELETE FROM accounts WHERE id=?`, id)
	return err
}

// CacheGroupParticipants menyimpan/update daftar participants grup ke cache database
func (s *Store) CacheGroupParticipants(groupID string, participants []struct {
	JID          string
	Number       string
	IsAdmin      bool
	IsSuperAdmin bool
}) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Hapus cache lama untuk grup ini
	if _, err := tx.Exec(`DELETE FROM group_participants WHERE group_id=?`, groupID); err != nil {
		return err
	}

	// Insert participants baru
	stmt, err := tx.Prepare(`INSERT INTO group_participants (group_id, jid, number, is_admin, is_superadmin, cached_at) 
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, p := range participants {
		if _, err := stmt.Exec(groupID, p.JID, p.Number, btoi(p.IsAdmin), btoi(p.IsSuperAdmin)); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// GetCachedGroupParticipants mengambil participants dari cache jika ada dan masih valid
func (s *Store) GetCachedGroupParticipants(groupID string, maxAgeMinutes int) ([]struct {
	JID          string
	Number       string
	IsAdmin      bool
	IsSuperAdmin bool
}, bool, error) {
	// Cek apakah ada cache yang valid
	var count int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM group_participants 
		WHERE group_id=? AND cached_at > datetime('now', '-' || ? || ' minutes')`,
		groupID, maxAgeMinutes).Scan(&count)
	if err != nil || count == 0 {
		return nil, false, err
	}

	// Ambil dari cache
	rows, err := s.DB.Query(`SELECT jid, number, is_admin, is_superadmin 
		FROM group_participants WHERE group_id=? ORDER BY number`, groupID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var participants []struct {
		JID          string
		Number       string
		IsAdmin      bool
		IsSuperAdmin bool
	}

	for rows.Next() {
		var p struct {
			JID          string
			Number       string
			IsAdmin      bool
			IsSuperAdmin bool
		}
		var isAdmin, isSuperAdmin int
		if err := rows.Scan(&p.JID, &p.Number, &isAdmin, &isSuperAdmin); err != nil {
			return nil, false, err
		}
		p.IsAdmin = isAdmin == 1
		p.IsSuperAdmin = isSuperAdmin == 1
		participants = append(participants, p)
	}

	return participants, true, nil
}

// InvalidateGroupParticipantsCache menghapus cache participants untuk grup tertentu
func (s *Store) InvalidateGroupParticipantsCache(groupID string) error {
	_, err := s.DB.Exec(`DELETE FROM group_participants WHERE group_id=?`, groupID)
	return err
}
