package httpapi

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"promote/internal/model"
	"promote/internal/sender"
	"promote/internal/storage"
	"promote/internal/wa"
)

type API struct {
	Store   *storage.Store
	Manager *wa.Manager
	Sender  *sender.Sender
	Router  *chi.Mux
}

func NewRouter(store *storage.Store, manager *wa.Manager) *chi.Mux {
	api := &API{
		Store:   store,
		Manager: manager,
		Sender:  sender.New(store, manager),
		Router:  chi.NewRouter(),
	}
	r := api.Router
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(120 * time.Second))
	r.Use(cors)

	api.routes()
	return r
}

func (a *API) routes() {
	a.Router.Get("/api/health", a.handleHealth)
	a.Router.Get("/api/accounts", a.handleListAccounts)
	a.Router.Post("/api/accounts", a.handleCreateAccount)
	a.Router.Put("/api/accounts/{id}", a.handleUpdateAccount)
	a.Router.Delete("/api/accounts/{id}", a.handleDeleteAccount)
	a.Router.Get("/api/groups", a.handleListGroups)
	a.Router.Post("/api/groups/toggle", a.handleToggleGroup)
	a.Router.Get("/api/stats", a.handleStats)

	// Templates management
	a.Router.Get("/api/templates", a.handleListTemplates)
	a.Router.Post("/api/templates", a.handleCreateTemplate)
	a.Router.Post("/api/templates/{id}/toggle", a.handleToggleTemplate)
	a.Router.Put("/api/templates/{id}", a.handleUpdateTemplate)
	a.Router.Delete("/api/templates/{id}", a.handleDeleteTemplate)

	// Pairing & connect endpoints
	a.Router.Get("/api/accounts/{id}/pair/qr", a.handleAccountPairQR)
	a.Router.Post("/api/accounts/{id}/pair/number", a.handleAccountPairByNumber)
	a.Router.Post("/api/accounts/{id}/connect", a.handleAccountConnect)

	// Account logout
	a.Router.Post("/api/accounts/{id}/logout", a.handleAccountLogout)

	// Refresh groups from WhatsApp
	a.Router.Post("/api/accounts/{id}/groups/refresh", a.handleRefreshGroups)

	// Group participants & CSV export
	a.Router.Get("/api/accounts/{id}/groups/{gid}/participants", a.handleGroupParticipants)
	a.Router.Get("/api/accounts/{id}/groups/{gid}/participants.csv", a.handleGroupParticipantsCSV)

	// Send test (manual trigger) endpoint
	a.Router.Post("/api/send/test", a.handleSendTest)

	// Log streaming (SSE)
	a.Router.Get("/api/logs/stream", a.handleLogsStream)

	// Uploads (multipart) endpoint and static serving
	a.Router.Post("/api/upload", a.handleUpload)
	a.Router.Handle("/uploads/*", http.StripPrefix("/uploads/", http.FileServer(http.Dir("uploads"))))

	// Favicon to avoid 404 noise
	a.Router.Get("/favicon.ico", a.handleFavicon)

	// Dashboard
	a.Router.Get("/", a.handleIndex)
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"time": time.Now().Format(time.RFC3339),
	})
}

type createAccountReq struct {
	Label      string `json:"label"`
	Msisdn     string `json:"msisdn"`
	DailyLimit int    `json:"daily_limit"`
	Enabled    *bool  `json:"enabled"`
}

func (a *API) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var req createAccountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Label == "" {
		writeErr(w, http.StatusBadRequest, "label required")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	id, err := a.Store.CreateAccount(req.Label, req.Msisdn, enabled, req.DailyLimit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (a *API) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	list, err := a.Store.ListAccounts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// Update & Delete Account
type updateAccountReq struct {
	Label      string `json:"label"`
	Msisdn     string `json:"msisdn"`
	DailyLimit int    `json:"daily_limit"`
	Enabled    *bool  `json:"enabled"`
}

func (a *API) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	exists, err := a.Store.AccountExists(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var req updateAccountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if err := a.Store.UpdateAccount(id, req.Label, req.Msisdn, enabled, req.DailyLimit); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": 1})
}

func (a *API) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	exists, err := a.Store.AccountExists(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	// Best-effort: logout & drop client cache untuk mencegah sesi menggantung/terdeteksi nomor lain
	_ = a.Manager.Logout(id)
	a.Manager.DropAccount(id)

	if err := a.Store.DeleteAccount(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": 1})
}

func (a *API) handleListGroups(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account_id")
	list, err := a.Store.ListGroups(accountID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

type toggleGroupReq struct {
	GroupID string `json:"group_id"`
	Enabled bool   `json:"enabled"`
}

func (a *API) handleToggleGroup(w http.ResponseWriter, r *http.Request) {
	var req toggleGroupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.GroupID == "" {
		writeErr(w, http.StatusBadRequest, "group_id required")
		return
	}
	n, err := a.Store.ToggleGroup(req.GroupID, req.Enabled)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n == 0 {
		writeErr(w, http.StatusNotFound, "group not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": n})
}

func (a *API) handleStats(w http.ResponseWriter, r *http.Request) {
	total, success, failed, err := a.Store.StatsToday()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{
		"total":   total,
		"success": success,
		"failed":  failed,
	})
}

// Favicon: return 204 to eliminate browser 404 noise
func (a *API) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/x-icon")
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleAccountPairQR(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	exists, err := a.Store.AccountExists(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	png, _, err := a.Manager.StartPairing(ctx, id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = a.Store.UpdateAccountStatus(id, model.StatusPairing, "", nil)
	w.Header().Set("Content-Type", "image/png")
	// Hindari caching QR kadaluarsa oleh browser/proxy
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

// Pair via phone number (if supported by whatsmeow)
type pairByNumberReq struct {
	Msisdn string `json:"msisdn"`
}

func (a *API) handleAccountPairByNumber(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	exists, err := a.Store.AccountExists(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var req pairByNumberReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Msisdn == "" {
		writeErr(w, http.StatusBadRequest, "msisdn required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	code, err := a.Manager.RequestPairingCode(ctx, id, req.Msisdn)
	if err != nil {
		if errors.Is(err, wa.ErrPairingByNumberUnsupported) {
			writeErr(w, http.StatusNotImplemented, "Pairing via nomor tidak didukung oleh whatsmeow saat ini. Gunakan QR.")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if code == "" {
		writeErr(w, http.StatusBadRequest, "pairing code kosong")
		return
	}
	_ = a.Store.UpdateAccountStatus(id, model.StatusPairing, "", &req.Msisdn)
	writeJSON(w, http.StatusOK, map[string]any{"code": code})
}

func (a *API) handleAccountConnect(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	exists, err := a.Store.AccountExists(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if err := a.Manager.ConnectIfPaired(id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = a.Store.UpdateAccountStatus(id, model.StatusOnline, "", nil)
	writeJSON(w, http.StatusOK, map[string]any{"status": "online"})
}

// Logout akun WA: putus sesi dan hapus client dari cache manager
func (a *API) handleAccountLogout(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	exists, err := a.Store.AccountExists(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if err := a.Manager.Logout(id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.Manager.DropAccount(id)
	writeJSON(w, http.StatusOK, map[string]any{"status": "logged_out"})
}

func (a *API) handleRefreshGroups(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	exists, err := a.Store.AccountExists(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	n, err := a.Manager.FetchAndSyncGroups(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"refreshed": n})
}

// Group participants JSON
func (a *API) handleGroupParticipants(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	gid := chi.URLParam(r, "gid")
	exists, err := a.Store.AccountExists(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	parts, err := a.Manager.GetGroupParticipants(ctx, id, gid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, parts)
}

// Group participants CSV export
func (a *API) handleGroupParticipantsCSV(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	gid := chi.URLParam(r, "gid")
	exists, err := a.Store.AccountExists(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	parts, err := a.Manager.GetGroupParticipants(ctx, id, gid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Disposition", "attachment; filename=\"participants.csv\"")
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{"number", "jid", "is_admin", "is_superadmin"})
	for _, p := range parts {
		_ = cw.Write([]string{
			p.Number,
			p.JID,
			func() string {
				if p.IsAdmin {
					return "true"
				}
				return "false"
			}(),
			func() string {
				if p.IsSuperAdmin {
					return "true"
				}
				return "false"
			}(),
		})
	}
}

// Send test API
type sendTestReq struct {
	AccountID   string   `json:"account_id"`
	GroupID     string   `json:"group_id"`
	Text        string   `json:"text"`
	ImageURLs   []string `json:"image_urls"`
	VideoURLs   []string `json:"video_urls"`
	AudioURLs   []string `json:"audio_urls"`
	StickerURLs []string `json:"sticker_urls"`
	DocURLs     []string `json:"doc_urls"`
}

func (a *API) handleSendTest(w http.ResponseWriter, r *http.Request) {
	var req sendTestReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.AccountID == "" || req.GroupID == "" {
		writeErr(w, http.StatusBadRequest, "account_id and group_id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	content := sender.MessageContent{
		Text:        req.Text,
		ImageURLs:   req.ImageURLs,
		VideoURLs:   req.VideoURLs,
		AudioURLs:   req.AudioURLs,
		StickerURLs: req.StickerURLs,
		DocURLs:     req.DocURLs,
	}
	if err := a.Sender.SendToGroup(ctx, req.AccountID, req.GroupID, content); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "sent"})
}

func (a *API) handleLogsStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	lastID := int64(0)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// kick off stream
	_, _ = w.Write([]byte(":ok\n\n"))
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			rows, err := a.Store.DB.Query(`SELECT id, ts, account_id, group_id, campaign_id, status, COALESCE(error,''), message_preview, attempt, scheduled_for
				FROM logs WHERE id > ? ORDER BY id ASC LIMIT 100`, lastID)
			if err != nil {
				// keep trying
				continue
			}
			for rows.Next() {
				var (
					id                                             int64
					ts                                             time.Time
					accountID, groupID, campaignID, status, errMsg string
					preview                                        string
					attempt                                        int
					scheduled                                      sql.NullTime
				)
				if err := rows.Scan(&id, &ts, &accountID, &groupID, &campaignID, &status, &errMsg, &preview, &attempt, &scheduled); err != nil {
					continue
				}
				if id > lastID {
					lastID = id
				}
				payload := map[string]any{
					"id":              id,
					"ts":              ts.Format(time.RFC3339),
					"account_id":      accountID,
					"group_id":        groupID,
					"campaign_id":     campaignID,
					"status":          status,
					"error":           errMsg,
					"message_preview": preview,
					"attempt":         attempt,
					"scheduled_for": func() string {
						if scheduled.Valid {
							return scheduled.Time.Format(time.RFC3339)
						}
						return ""
					}(),
				}
				b, err := json.Marshal(payload)
				if err != nil {
					continue
				}
				_, _ = w.Write([]byte("data: "))
				_, _ = w.Write(b)
				_, _ = w.Write([]byte("\n\n"))
				flusher.Flush()
			}
			rows.Close()
		}
	}
}

/********** Templates (Global) Management **********/

type upsertTemplateReq struct {
	Name        string   `json:"name"`
	Text        string   `json:"text"`
	ImageURLs   []string `json:"image_urls"`
	VideoURLs   []string `json:"video_urls"`
	AudioURLs   []string `json:"audio_urls"`
	StickerURLs []string `json:"sticker_urls"`
	DocURLs     []string `json:"doc_urls"`
	Enabled     bool     `json:"enabled"`
}

func (a *API) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	rows, err := a.Store.DB.Query(`SELECT id,name,COALESCE(text,''),COALESCE(images_json,''),COALESCE(videos_json,''),COALESCE(audio_json,''),COALESCE(stickers_json,''),COALESCE(docs_json,''),enabled,created_at,updated_at
		FROM templates ORDER BY created_at DESC`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var (
			id, name, text, imgJSON, vidJSON, audJSON, stJSON, docJSON string
			enabledInt                                                 int
			created, updated                                           time.Time
		)
		if err := rows.Scan(&id, &name, &text, &imgJSON, &vidJSON, &audJSON, &stJSON, &docJSON, &enabledInt, &created, &updated); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, map[string]any{
			"id":           id,
			"name":         name,
			"text":         text,
			"image_urls":   parseJSONArray(imgJSON),
			"video_urls":   parseJSONArray(vidJSON),
			"audio_urls":   parseJSONArray(audJSON),
			"sticker_urls": parseJSONArray(stJSON),
			"doc_urls":     parseJSONArray(docJSON),
			"enabled":      enabledInt == 1,
			"created_at":   created.Format(time.RFC3339),
			"updated_at":   updated.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	var req upsertTemplateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	id := uuid.NewString()
	_, err := a.Store.DB.Exec(`INSERT INTO templates (id,name,text,images_json,videos_json,audio_json,stickers_json,docs_json,enabled,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		id, req.Name, req.Text,
		toJSONArray(req.ImageURLs),
		toJSONArray(req.VideoURLs),
		toJSONArray(req.AudioURLs),
		toJSONArray(req.StickerURLs),
		toJSONArray(req.DocURLs),
		btoi(req.Enabled),
	)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (a *API) handleToggleTemplate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	res, err := a.Store.DB.Exec(`UPDATE templates SET enabled=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, btoi(body.Enabled), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, http.StatusNotFound, "template not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": 1})
}

func parseJSONArray(s string) []string {
	var arr []string
	if strings.TrimSpace(s) == "" {
		return arr
	}
	_ = json.Unmarshal([]byte(s), &arr)
	return arr
}
func toJSONArray(arr []string) string {
	b, _ := json.Marshal(arr)
	return string(b)
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

/********** Templates Update/Delete **********/

// Update template: full update of name, text, media arrays, and enabled flag.
func (a *API) handleUpdateTemplate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req upsertTemplateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	// Normalize enabled default true if omitted
	enabled := req.Enabled
	// Run update
	res, err := a.Store.DB.Exec(`UPDATE templates
		SET name=?, text=?, images_json=?, videos_json=?, audio_json=?, stickers_json=?, docs_json=?, enabled=?, updated_at=CURRENT_TIMESTAMP
		WHERE id=?`,
		req.Name, req.Text,
		toJSONArray(req.ImageURLs),
		toJSONArray(req.VideoURLs),
		toJSONArray(req.AudioURLs),
		toJSONArray(req.StickerURLs),
		toJSONArray(req.DocURLs),
		btoi(enabled),
		id,
	)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, http.StatusNotFound, "template not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": 1})
}

// Delete template by ID.
func (a *API) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	res, err := a.Store.DB.Exec(`DELETE FROM templates WHERE id=?`, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, http.StatusNotFound, "template not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": 1})
}

/********** End Templates Management **********/

// Upload file (multipart) for images, videos, audio/voice, stickers (webp), documents.
func (a *API) handleUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(50 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse multipart failed")
		return
	}
	kind := strings.TrimSpace(r.FormValue("kind"))
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "file missing")
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	switch kind {
	case "image":
		if ext == "" {
			ext = ".jpg"
		}
	case "video":
		if ext == "" {
			ext = ".mp4"
		}
	case "audio":
		if ext == "" {
			ext = ".mp3"
		}
	case "sticker":
		// WA sticker recommended: .webp
		ext = ".webp"
	case "doc":
		if ext == "" {
			ext = ".pdf"
		}
	default:
		writeErr(w, http.StatusBadRequest, "invalid kind")
		return
	}

	if err := os.MkdirAll("uploads", 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir uploads failed")
		return
	}
	fname := uuid.NewString() + ext
	path := filepath.Join("uploads", fname)

	out, err := os.Create(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "save file failed")
		return
	}
	defer out.Close()
	if _, err := io.Copy(out, file); err != nil {
		writeErr(w, http.StatusInternalServerError, "write file failed")
		return
	}

	mime := "application/octet-stream"
	switch kind {
	case "image":
		m := strings.TrimPrefix(ext, ".")
		if m == "jpg" || m == "jpeg" || m == "png" || m == "webp" {
			mime = "image/" + m
		}
	case "video":
		m := strings.TrimPrefix(ext, ".")
		if m == "mp4" || m == "mov" || m == "mkv" {
			mime = "video/" + m
		}
	case "audio":
		m := strings.TrimPrefix(ext, ".")
		if m == "mp3" || m == "ogg" || m == "wav" || m == "m4a" {
			mime = "audio/" + m
		}
	case "sticker":
		mime = "image/webp"
	case "doc":
		// keep default
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"url":      "/uploads/" + fname,
		"mimetype": mime,
	})
}

// Dashboard

func (a *API) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(dashboardHTML()))
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(v); err != nil {
		log.Println("writeJSON err:", err)
	}
}

func dashboardHTML() string {
	return `<!doctype html>
<html lang="id">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Promote WA Dashboard</title>
<style>
:root{--bg:#0f1115;--panel:#12151d;--border:#2a2f3a;--text:#e6e6e6;--muted:#9aa0aa;--primary:#2b6cb0;--secondary:#394050;--danger:#b83b3b;--ok:#7bd88f;--err:#ff6b6b}
*{box-sizing:border-box}
html,body{height:100%}
body{font-family:system-ui,-apple-system,Segoe UI,Roboto,Arial,sans-serif;background:var(--bg);color:var(--text);margin:0;-webkit-font-smoothing:antialiased;-moz-osx-font-smoothing:grayscale}
header{padding:12px 16px;background:#151821;border-bottom:1px solid var(--border);position:sticky;top:0;z-index:10}
h1{font-size:18px;margin:0}
main{padding:16px;max-width:1200px;margin:0 auto}
section{margin:16px 0;padding:12px;border:1px solid var(--border);border-radius:10px;background:var(--panel)}
.row{display:flex;gap:12px;flex-wrap:wrap;align-items:center}
button{background:var(--primary);border:0;color:#fff;padding:9px 14px;border-radius:8px;cursor:pointer;transition:background .15s ease,opacity .15s ease}
button:hover{opacity:.95}
button.secondary{background:var(--secondary)}
button.danger{background:var(--danger)}
input,select,textarea{background:#0d0f14;border:1px solid var(--border);color:var(--text);border-radius:8px;padding:9px 12px}
table{width:100%;border-collapse:collapse;font-size:14px;display:block;overflow-x:auto}
thead{background:#0f131b;position:sticky;top:0}
th,td{padding:10px 8px;border-bottom:1px solid var(--border);white-space:nowrap}
.ok{color:var(--ok)}.err{color:var(--err)}
img.qr{width:220px;height:220px;border:1px solid var(--border);border-radius:10px}
small.mono{font-family:ui-monospace,Menlo,Consolas,monospace;color:var(--muted)}
@media (max-width:720px){
  h1{font-size:16px}
  .row{gap:10px}
  button{padding:8px 12px}
  input,select,textarea{width:100%}
}
</style>
</head>
<body>
<header><h1>Promote WA Dashboard</h1></header>
<main>
<section id="health">
  <div class="row"><strong>Status server:</strong><span id="health-status" class="ok">menunggu...</span><small class="mono" id="health-time"></small></div>
</section>

<section id="account-create">
  <h3>Akun WhatsApp</h3>
  <div class="row">
    <input id="acc-label" placeholder="Label akun (mis. Nomor A)">
    <input id="acc-msisdn" placeholder="MSISDN (opsional)">
    <input id="acc-limit" type="number" min="1" value="100" style="width:120px">
    <label><input type="checkbox" id="acc-enabled" checked> Aktif</label>
    <button id="acc-create">Tambah Akun</button>
    <button id="acc-save" class="secondary" disabled>Simpan Perubahan</button>
  </div>
  <small class="mono">Klik 'Edit' pada baris akun untuk mengisi form ini lalu 'Simpan Perubahan'.</small>
</section>

<section id="accounts">
  <h3>Daftar Akun</h3>
  <table>
    <thead><tr><th>Label</th><th>MSISDN</th><th>Status</th><th>Limit</th><th>Aksi</th></tr></thead>
    <tbody id="accounts-tbody"></tbody>
  </table>
  <div class="row" style="margin-top:8px">
    <div>
      <h4>QR Pairing</h4>
      <img id="qr-img" class="qr" alt="QR akan muncul di sini">
    </div>
    <div>
      <h4>Pair via Nomor</h4>
      <div class="row">
        <select id="pair-account"></select>
        <input id="pair-msisdn" placeholder="MSISDN (62...)" style="width:200px">
        <button id="btn-pair-number" class="secondary">Minta Kode</button>
      </div>
      <div>Kode: <strong id="pair-code">-</strong></div>
      <small class="mono">Jika tidak didukung oleh library, gunakan QR pairing.</small>
    </div>
  </div>
</section>

<section id="groups">
  <h3>Grup (per Akun)</h3>
  <div class="row">
    <select id="groups-account"></select>
    <button id="btn-refresh" class="secondary">Refresh dari WhatsApp</button>
  </div>
  <table style="margin-top:8px">
    <thead><tr><th>Nama Grup</th><th>Enabled</th><th>Terakhir Kirim</th><th>Risk</th><th>ID</th><th>Aksi</th></tr></thead>
    <tbody id="groups-tbody"></tbody>
  </table>
</section>

<section id="send-test">
  <h3>Kirim Uji</h3>
  <div class="row">
    <label for="send-account">Akun</label>
    <select id="send-account"></select>
    <label for="send-group-id">Group JID</label>
    <input id="send-group-id" placeholder="mis. 12345-67890@g.us" style="width:280px">
  </div>
  <div class="row" style="margin-top:8px">
    <label for="send-text">Teks Promo</label>
    <textarea id="send-text" placeholder="Gunakan {group_name} untuk personalisasi" rows="3" style="width:100%"></textarea>
  </div>
  <div class="row" style="margin-top:8px">
    <label for="send-file-image">Gambar</label>
    <input type="file" id="send-file-image" accept="image/*" multiple>
    <label for="send-file-video">Video</label>
    <input type="file" id="send-file-video" accept="video/*" multiple>
    <label for="send-file-audio">Audio/Voice</label>
    <input type="file" id="send-file-audio" accept="audio/*" multiple>
  </div>
  <div class="row" style="margin-top:8px">
    <label for="send-file-sticker">Sticker (webp)</label>
    <input type="file" id="send-file-sticker" accept="image/webp" multiple>
    <label for="send-file-doc">Dokumen</label>
    <input type="file" id="send-file-doc" multiple>
    <button id="btn-send-test">Kirim Uji</button>
  </div>
  <small class="mono">Petunjuk upload: pilih file di input sesuai jenisnya. Sistem akan mengunggah ke /uploads lalu mengirim dengan delay natural antar bagian konten.</small>
</section>

<section id="participants">
  <h3>Anggota Grup</h3>
  <div class="row">
    <small class="mono" id="participants-info">Pilih akun dan klik tombol "Anggota" pada baris grup untuk melihat daftar.</small>
  </div>
  <table style="margin-top:8px">
    <thead><tr><th>Nomor</th><th>JID</th><th>Admin</th><th>SuperAdmin</th></tr></thead>
    <tbody id="participants-tbody"></tbody>
  </table>
</section>

<section id="groups-by-number">
  <h3>Grup per Nomor</h3>
  <div class="row">
    <button id="btn-load-all-groups" class="secondary">Muat Semua Grup per Nomor</button>
  </div>
  <div id="groups-container"></div>
</section>

<section id="stats">
  <h3>Statistik Hari Ini</h3>
  <div class="row">
    <div>Total: <span id="s-total">0</span></div>
    <div class="ok">Sukses: <span id="s-success">0</span></div>
    <div class="err">Gagal: <span id="s-failed">0</span></div>
  </div>
</section>

<section id="logs">
  <h3>Log Aktivitas</h3>
  <table>
    <thead><tr><th>Waktu</th><th>Akun</th><th>Grup</th><th>Status</th><th>Preview</th><th>Error</th></tr></thead>
    <tbody id="logs-tbody"></tbody>
  </table>
</section>

<section id="templates">
  <h3>Template Global</h3>
  <div class="row">
    <label for="tpl-name">Nama Template</label>
    <input id="tpl-name" placeholder="Nama template" style="width:200px">
    <label for="tpl-text">Teks (opsional)</label>
    <textarea id="tpl-text" placeholder="Teks (opsional)" rows="4" style="width:420px"></textarea>
    <button id="tpl-create">Tambah Template</button>
    <button id="tpl-save" class="secondary" disabled>Simpan Perubahan</button>
  </div>
  <div class="row" style="margin-top:8px">
    <label for="file-image">Gambar</label>
    <input type="file" id="file-image" accept="image/*" multiple>
    <label for="file-video">Video</label>
    <input type="file" id="file-video" accept="video/*" multiple>
    <label for="file-audio">Audio/Voice</label>
    <input type="file" id="file-audio" accept="audio/*" multiple>
    <label for="file-sticker">Sticker (webp)</label>
    <input type="file" id="file-sticker" accept="image/webp" multiple>
    <label for="file-doc">Dokumen</label>
    <input type="file" id="file-doc" multiple>
  </div>
  <small class="mono">Petunjuk upload: semua file diunggah ke /uploads dan disimpan dalam template ini. Saat broadcast, sistem merotasi template aktif secara acak.</small>
  <table style="margin-top:8px">
    <thead><tr><th>Nama</th><th>Aktif</th><th>Images</th><th>Videos</th><th>Audio</th><th>Stickers</th><th>Docs</th><th>Aksi</th></tr></thead>
    <tbody id="tpl-tbody"></tbody>
  </table>
</section>
</main>

<script>
var $ = function(s){ return document.querySelector(s); };
var api = function(p,opt){ opt=opt||{}; var h=opt.headers||{}; h['Content-Type']='application/json'; opt.headers=h; return fetch(p,opt); };

function escapeHtml(s){
  s = (s==null ? '' : String(s));
  return s.replace(/[&<>"']/g, function(c){
    return ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]);
  });
}

async function pollHealth(){
  try{
    var r = await api('/api/health'); var j = await r.json();
    $('#health-status').textContent = j.ok ? 'ONLINE' : 'OFFLINE';
    $('#health-status').className = j.ok ? 'ok' : 'err';
    $('#health-time').textContent = j.time || '';
  }catch(e){
    $('#health-status').textContent = 'OFFLINE';
    $('#health-status').className = 'err';
  }
}

function rowAccount(a){
  var tr = document.createElement('tr');
  tr.innerHTML =
    '<td>'+escapeHtml(a.label)+'</td>'+
    '<td>'+(a.msisdn?escapeHtml(a.msisdn):'-')+'</td>'+
    '<td><span class="'+(a.status==='online'?'ok':'err')+'">'+escapeHtml(a.status)+'</span></td>'+
    '<td>'+a.daily_limit+'</td>'+
    '<td>'+
      '<button data-act="qr" data-id="'+a.id+'">QR</button> '+
      '<button data-act="connect" data-id="'+a.id+'" class="secondary">Connect</button> '+
      '<button data-act="logout" data-id="'+a.id+'" class="secondary">Logout</button> '+
      '<button data-act="refresh" data-id="'+a.id+'" class="secondary">Refresh Grup</button> '+
      '<button data-act="edit" data-id="'+a.id+'">Edit</button> '+
      '<button data-act="delete" data-id="'+a.id+'" class="danger">Delete</button>'+
    '</td>';
  return tr;
}

var accById = {};
var editingAccountId = null;
var tplById = {};
var editingTemplateId = null;

async function loadAccounts(){
  var r = await api('/api/accounts'); var list = await r.json();
  accById = {};
  list.forEach(function(a){ accById[a.id] = a; });
  var tb = $('#accounts-tbody'); tb.innerHTML = '';
  var sel = $('#groups-account'); sel.innerHTML = '';
  var sel2 = $('#send-account'); if (sel2) sel2.innerHTML = '';
  var sel3 = $('#pair-account'); if (sel3) sel3.innerHTML = '';
  list.forEach(function(a){
    tb.appendChild(rowAccount(a));
    var opt = document.createElement('option'); opt.value = a.id; opt.textContent = a.label + ' ('+(a.msisdn||'-')+')';
    sel.appendChild(opt);
    if (sel2) {
      var opt2 = document.createElement('option'); opt2.value = a.id; opt2.textContent = a.label + ' ('+(a.msisdn||'-')+')';
      sel2.appendChild(opt2);
    }
    if (sel3) {
      var opt3 = document.createElement('option'); opt3.value = a.id; opt3.textContent = a.label + ' ('+(a.msisdn||'-')+')';
      sel3.appendChild(opt3);
    }
  });
}

async function createAccount(){
  var label = $('#acc-label').value.trim();
  var msisdn = $('#acc-msisdn').value.trim();
  var daily = parseInt($('#acc-limit').value||'100',10);
  var enabled = !!($('#acc-enabled') && $('#acc-enabled').checked);
  if(!label){ alert('Label wajib'); return; }
  var r = await api('/api/accounts',{method:'POST',body:JSON.stringify({label:label,msisdn:msisdn,daily_limit:daily,enabled:enabled})});
  if(!r.ok){ var t = await r.text(); alert('Gagal: '+t); return; }
  $('#acc-label').value = ''; $('#acc-msisdn').value = ''; $('#acc-limit').value = '100'; if ($('#acc-enabled')) $('#acc-enabled').checked = true;
  await loadAccounts();
}

var qrTimer = null;

async function showQR(id){
  $('#qr-img').src = '/api/accounts/'+id+'/pair/qr?ts='+(Date.now());
}

function startQRRefresh(id){
  stopQRRefresh();
  showQR(id);
  qrTimer = setInterval(function(){ showQR(id); }, 25000);
}

function stopQRRefresh(){
  if (qrTimer) { clearInterval(qrTimer); qrTimer = null; }
}

async function connectAcc(id){
  stopQRRefresh();
  var r = await api('/api/accounts/'+id+'/connect',{method:'POST'});
  if(!r.ok){ var t=await r.text(); alert('Connect gagal: '+t); }
  await loadAccounts();
}
async function logoutAcc(id){
  stopQRRefresh();
  var r = await api('/api/accounts/'+id+'/logout',{method:'POST'});
  if(!r.ok){ var t=await r.text(); alert('Logout gagal: '+t); }
  await loadAccounts();
}

async function pairByNumber(){
  var acc = $('#pair-account') ? $('#pair-account').value : '';
  var msisdn = $('#pair-msisdn') ? $('#pair-msisdn').value.trim() : '';
  if(!acc || !msisdn){
    alert('Pilih akun dan isi MSISDN');
    return;
  }
  stopQRRefresh();
  api('/api/accounts/'+encodeURIComponent(acc)+'/pair/number', {
    method: 'POST',
    body: JSON.stringify({msisdn: msisdn})
  }).then(function(r){
    if(r.status === 501){
      return r.json().then(function(j){ throw new Error((j && j.error) ? j.error : 'Tidak didukung, gunakan QR'); });
    }
    if(!r.ok) return r.text().then(function(t){ throw new Error(t); });
    return r.json();
  }).then(function(j){
    var el = document.getElementById('pair-code'); if (el) el.textContent = j.code || '-';
    alert('Kode pairing: '+(j.code||'-')+'\nMasukkan kode ini di WhatsApp: Link dengan nomor');
  }).catch(function(err){
    alert('Gagal minta kode: '+err.message);
  });
}

async function refreshGroups(){
  var id = $('#groups-account').value;
  if(!id){ alert('Pilih akun'); return; }
  var r = await api('/api/accounts/'+id+'/groups/refresh',{method:'POST'});
  if(!r.ok){ var t=await r.text(); alert('Refresh gagal: '+t); return; }
  await loadGroups();
}

function rowGroup(g){
  var tr = document.createElement('tr');
  tr.innerHTML =
    '<td>'+escapeHtml(g.name||'-')+'</td>'+
    '<td><input type="checkbox" data-id="'+g.id+'" class="g-toggle" '+(g.enabled?'checked':'')+'></td>'+
    '<td>'+(g.last_sent_at? new Date(g.last_sent_at).toLocaleString():'-')+'</td>'+
    '<td>'+g.risk_score+'</td>'+
    '<td><small class="mono">'+g.id+'</small></td>'+
    '<td>'+
      '<button class="secondary" data-act="members" data-id="'+g.id+'">Anggota</button> '+
      '<button class="secondary" data-act="export" data-id="'+g.id+'">Export CSV</button>'+
    '</td>';
  return tr;
}

async function loadGroups(){
  var id = $('#groups-account').value;
  if(!id){ $('#groups-tbody').innerHTML=''; return; }
  var r = await api('/api/groups?account_id='+encodeURIComponent(id));
  var list = await r.json();
  var tb = $('#groups-tbody'); tb.innerHTML = '';
  list.forEach(function(g){ tb.appendChild(rowGroup(g)); });
}

async function toggleGroup(id, enabled){
  var r = await api('/api/groups/toggle',{method:'POST',body:JSON.stringify({group_id:id,enabled:enabled})});
  if(!r.ok){ var t=await r.text(); alert('Toggle gagal: '+t); }
}

function renderParticipants(list){
  var tb = document.getElementById('participants-tbody'); if(!tb) return;
  tb.innerHTML = '';
  if (!Array.isArray(list) || list.length === 0) {
    tb.innerHTML = '<tr><td colspan="4"><small class="mono">Tidak ada anggota terdeteksi atau akun belum connect.</small></td></tr>';
    return;
  }
  list.forEach(function(p){
    var tr = document.createElement('tr');
    tr.innerHTML =
      '<td>'+escapeHtml(p.number||'')+'</td>'+
      '<td><small class="mono">'+escapeHtml(p.jid||'')+'</small></td>'+
      '<td>'+(p.is_admin?'Ya':'-')+'</td>'+
      '<td>'+(p.is_superadmin?'Ya':'-')+'</td>';
    tb.appendChild(tr);
  });
  var info = document.getElementById('participants-info');
  if (info) info.textContent = 'Total anggota: '+list.length;
}

async function loadParticipants(gid){
  try{
    var acc = document.getElementById('groups-account') ? document.getElementById('groups-account').value : '';
    if(!acc){ alert('Pilih akun'); return; }
    var r = await api('/api/accounts/'+encodeURIComponent(acc)+'/groups/'+encodeURIComponent(gid)+'/participants');
    if(!r.ok){ throw new Error(await r.text()); }
    var list = await r.json();
    renderParticipants(list);
    // scroll ke section participants
    var section = document.getElementById('participants');
    if(section){ window.scrollTo({ top: section.offsetTop - 60, behavior: 'smooth' }); }
  }catch(e){
    alert('Gagal muat anggota: '+e.message);
  }
}

function exportParticipantsCSV(gid){
  var acc = document.getElementById('groups-account') ? document.getElementById('groups-account').value : '';
  if(!acc){ alert('Pilih akun'); return; }
  var url = '/api/accounts/'+encodeURIComponent(acc)+'/groups/'+encodeURIComponent(gid)+'/participants.csv';
  window.open(url, '_blank');
}

async function loadStats(){
  var r = await api('/api/stats'); var j = await r.json();
  $('#s-total').textContent = j.total||0;
  $('#s-success').textContent = j.success||0;
  $('#s-failed').textContent = j.failed||0;
}

async function sendTest(){
  var acc = $('#send-account') ? $('#send-account').value : '';
  var gid = $('#send-group-id') ? $('#send-group-id').value.trim() : '';
  var text = $('#send-text') ? $('#send-text').value : '';
  if(!acc || !gid){
    alert('Pilih akun dan isi Group JID');
    return;
  }
  // Upload langsung dari file input, tanpa perlu URL manual
  async function upload(kind, file){
    var fd = new FormData(); fd.append('kind', kind); fd.append('file', file);
    var r = await fetch('/api/upload', { method:'POST', body: fd });
    if(!r.ok){ throw new Error(await r.text()); }
    var j = await r.json(); return j.url;
  }
  async function collect(kind, inputId){
    var el = document.getElementById(inputId);
    var urls = [];
    if(el && el.files){
      for(var i=0;i<el.files.length;i++){
        urls.push(await upload(kind, el.files[i]));
      }
    }
    return urls;
  }
  try{
    var images = await collect('image','send-file-image');
    var videos = await collect('video','send-file-video');
    var audios = await collect('audio','send-file-audio');
    var stickers = await collect('sticker','send-file-sticker');
    var docs = await collect('doc','send-file-doc');
    var r = await api('/api/send/test', {
      method:'POST',
      body: JSON.stringify({account_id: acc, group_id: gid, text: text, image_urls: images, video_urls: videos, audio_urls: audios, sticker_urls: stickers, doc_urls: docs})
    });
    if(!r.ok){ throw new Error(await r.text()); }
    await r.json();
    alert('Kirim uji diproses');
  }catch(err){
    alert('Gagal kirim: '+err.message);
  }
}

function bindEvents(){
  $('#acc-create').addEventListener('click', createAccount);
  var btnSave = document.getElementById('acc-save');
  if (btnSave) btnSave.addEventListener('click', saveAccount);
  $('#accounts-tbody').addEventListener('click', function(e){
    var btn = e.target.closest('button'); if(!btn) return;
    var id = btn.getAttribute('data-id');
    var act = btn.getAttribute('data-act');
    if(act==='qr') startQRRefresh(id);
    else if(act==='connect') connectAcc(id);
    else if(act==='logout') logoutAcc(id);
    else if(act==='refresh'){ $('#groups-account').value = id; refreshGroups(); }
    else if(act==='edit'){ startEditAccount(id); }
    else if(act==='delete'){ deleteAccount(id); }
  });
  $('#btn-refresh').addEventListener('click', refreshGroups);
  $('#groups-account').addEventListener('change', loadGroups);
  $('#groups-tbody').addEventListener('change', function(e){
    var cb = e.target.closest('.g-toggle'); if(!cb) return;
    toggleGroup(cb.getAttribute('data-id'), cb.checked);
  });
  $('#groups-tbody').addEventListener('click', function(e){
    var btn = e.target.closest('button'); if(!btn) return;
    var id = btn.getAttribute('data-id');
    var act = btn.getAttribute('data-act');
    if (!id || !act) return;
    if (act === 'members') {
      loadParticipants(id);
    } else if (act === 'export') {
      exportParticipantsCSV(id);
    }
  });
  var groupsContainer = document.getElementById('groups-container');
  if (groupsContainer) {
    groupsContainer.addEventListener('change', function(e){
      var cb = e.target.closest('.g-toggle'); if(!cb) return;
      toggleGroup(cb.getAttribute('data-id'), cb.checked);
    });
    groupsContainer.addEventListener('click', async function(e){
      var btn = e.target.closest('button'); if(!btn) return;
      var id = btn.getAttribute('data-id');
      var act = btn.getAttribute('data-act');
      if(!id || !act) return;
      if(act==='connect'){
        try { await connectAcc(id); } catch(_){}
        await refreshOneAccount(id);
        await reloadOneAccountGroups(id);
      }else if(act==='logout'){
        try { await logoutAcc(id); } catch(_){}
        await reloadOneAccountGroups(id);
      }else if(act==='refresh'){
        await refreshOneAccount(id);
        await reloadOneAccountGroups(id);
      }
    });
  }
  var btnAll = document.getElementById('btn-load-all-groups');
  if (btnAll) btnAll.addEventListener('click', loadGroupsByNumber);
  var btnSend = document.getElementById('btn-send-test');
  if (btnSend) btnSend.addEventListener('click', sendTest);
  var btnPair = document.getElementById('btn-pair-number');
  if (btnPair) btnPair.addEventListener('click', pairByNumber);
  var btnTpl = document.getElementById('tpl-create');
  if (btnTpl) btnTpl.addEventListener('click', createTemplate);
  var btnTplSave = document.getElementById('tpl-save');
  if (btnTplSave) btnTplSave.addEventListener('click', saveTemplate);
}

function rowLog(l){
  var tr = document.createElement('tr');
  var ts = l.ts ? new Date(l.ts).toLocaleString() : '';
  tr.innerHTML =
    '<td>'+escapeHtml(ts)+'</td>'+
    '<td>'+escapeHtml(l.account_id||'')+'</td>'+
    '<td><small class="mono">'+escapeHtml(l.group_id||'')+'</small></td>'+
    '<td>'+escapeHtml(l.status||'')+'</td>'+
    '<td>'+escapeHtml(l.message_preview||'')+'</td>'+
    '<td>'+(l.error?'<span class="err">'+escapeHtml(l.error)+'</span>':'')+'</td>';
  return tr;
}

var esLogs = null;
function logsConnect(){
  try{
    esLogs = new EventSource('/api/logs/stream');
    esLogs.onmessage = function(ev){
      try{
        var l = JSON.parse(ev.data);
        var tb = document.getElementById('logs-tbody');
        if(tb){
          var tr = rowLog(l);
          if (tb.firstChild) tb.insertBefore(tr, tb.firstChild);
          else tb.appendChild(tr);
        }
      }catch(e){}
    };
  }catch(e){}
}

function renderAccountCard(acc){
  var card = document.createElement('section');
  card.style.marginTop = '8px';
  card.innerHTML = '<div class="row"><strong>'+escapeHtml(acc.label)+'</strong> <small class="mono">('+(acc.msisdn?escapeHtml(acc.msisdn):'-')+')</small> <span class="'+(acc.status==='online'?'ok':'err')+'">'+escapeHtml(acc.status||'-')+'</span> <button class="secondary" data-act="connect" data-id="'+acc.id+'">Connect</button> <button class="secondary" data-act="logout" data-id="'+acc.id+'">Logout</button> <button class="secondary" data-act="refresh" data-id="'+acc.id+'">Refresh Grup</button></div>'+
                   '<table><thead><tr><th>Nama Grup</th><th>Enabled</th><th>Terakhir Kirim</th><th>Risk</th><th>ID</th></tr></thead><tbody><tr><td colspan="5"><small class="mono">Memuat...</small></td></tr></tbody></table>';
  card.setAttribute('data-acc-id', acc.id);
  return card;
}
function renderAccountGroups(accID, groups){
  var container = document.getElementById('groups-container');
  if (!container) return;
  var card = container.querySelector('section[data-acc-id="'+accID+'"]');
  if (!card) return;
  var tb = card.querySelector('tbody');
  if (!tb) return;
  if (!groups || groups.length === 0) {
    tb.innerHTML = '<tr><td colspan="5"><small class="mono">Tidak ada grup. Pastikan akun telah connect dan klik "Refresh Grup".</small></td></tr>';
    return;
  }
  tb.innerHTML = '';
  groups.forEach(function(g){ tb.appendChild(rowGroup(g)); });
}
async function reloadOneAccountGroups(accID){
  try{
    var rg = await api('/api/groups?account_id='+encodeURIComponent(accID));
    var glist = await rg.json();
    renderAccountGroups(accID, Array.isArray(glist)?glist:[]);
  }catch(e){
    renderAccountGroups(accID, []);
  }
}
async function refreshOneAccount(accID){
  try{
    await api('/api/accounts/'+encodeURIComponent(accID)+'/groups/refresh', { method:'POST' });
  }catch(e){}
}
async function loadGroupsByNumber(){
  var container = document.getElementById('groups-container'); if (!container) return;
  container.innerHTML = '';
  var ra = await api('/api/accounts'); var accs = await ra.json();
  accs.forEach(function(acc){
    container.appendChild(renderAccountCard(acc));
  });
  for (var i=0;i<accs.length;i++){
    try{
      var acc = accs[i];
      // upayakan connect jika belum online
      if ((acc.status||'').toLowerCase() !== 'online') {
        try { await api('/api/accounts/'+encodeURIComponent(acc.id)+'/connect', { method:'POST' }); } catch(_){}
        await new Promise(function(res){ setTimeout(res, 500); });
      }
      var rg = await api('/api/groups?account_id='+encodeURIComponent(acc.id));
      var glist = await rg.json();
      if (!Array.isArray(glist) || glist.length === 0) {
        // coba refresh dari WhatsApp jika kosong, lalu fetch ulang
        await api('/api/accounts/'+encodeURIComponent(acc.id)+'/groups/refresh', { method:'POST' });
        await new Promise(function(res){ setTimeout(res, 800); });
        rg = await api('/api/groups?account_id='+encodeURIComponent(acc.id));
        glist = await rg.json();
      }
      renderAccountGroups(acc.id, glist);
    }catch(e){
      try { renderAccountGroups(accs[i].id, []); } catch(_){}
    }
  }
}
function rowTemplate(t){
  var tr = document.createElement('tr');
  tr.innerHTML =
    '<td>'+escapeHtml(t.name)+'</td>'+
    '<td><input type="checkbox" data-id="'+t.id+'" class="tpl-toggle" '+(t.enabled?'checked':'')+'></td>'+
    '<td>'+(t.image_urls||[]).length+'</td>'+
    '<td>'+(t.video_urls||[]).length+'</td>'+
    '<td>'+(t.audio_urls||[]).length+'</td>'+
    '<td>'+(t.sticker_urls||[]).length+'</td>'+
    '<td>'+(t.doc_urls||[]).length+'</td>'+
    '<td>'+
      '<button class="secondary" data-act="edit" data-id="'+t.id+'">Edit</button> '+
      '<button class="danger" data-act="delete" data-id="'+t.id+'">Delete</button>'+
    '</td>';
  return tr;
}
async function loadTemplates(){
  var r = await api('/api/templates'); var list = await r.json();
  tplById = {};
  var tb = document.getElementById('tpl-tbody'); if (!tb) return;
  tb.innerHTML = '';
  list.forEach(function(t){ tplById[t.id] = t; tb.appendChild(rowTemplate(t)); });
  tb.addEventListener('change', function(e){
    var cb = e.target.closest('.tpl-toggle'); if(!cb) return;
    var id = cb.getAttribute('data-id');
    api('/api/templates/'+id+'/toggle', { method:'POST', body: JSON.stringify({enabled: cb.checked}) });
  });
  tb.addEventListener('click', function(e){
    var btn = e.target.closest('button'); if (!btn) return;
    var id = btn.getAttribute('data-id');
    var act = btn.getAttribute('data-act');
    if (!id || !act) return;
    if (act === 'edit') {
      startEditTemplate(id);
    } else if (act === 'delete') {
      deleteTemplate(id);
    }
  });
}
async function createTemplate(){
  var name = document.getElementById('tpl-name').value.trim();
  var txtEl = document.getElementById('tpl-text');
  var text = txtEl ? txtEl.value : '';
  if(!name){ alert('Nama template wajib'); return; }
  async function upload(kind, file){
    var fd = new FormData(); fd.append('kind', kind); fd.append('file', file);
    var r = await fetch('/api/upload', { method:'POST', body: fd });
    if(!r.ok){ throw new Error(await r.text()); }
    var j = await r.json(); return j.url;
  }
  async function collect(kind, inputId){
    var el = document.getElementById(inputId);
    var urls = [];
    if(el && el.files){
      for(var i=0;i<el.files.length;i++){
        urls.push(await upload(kind, el.files[i]));
      }
    }
    return urls;
  }
  try{
    var imgs = await collect('image','file-image');
    var vids = await collect('video','file-video');
    var auds = await collect('audio','file-audio');
    var sts  = await collect('sticker','file-sticker');
    var docs = await collect('doc','file-doc');
    var r = await api('/api/templates', { method:'POST', body: JSON.stringify({ name: name, text: text, image_urls: imgs, video_urls: vids, audio_urls: auds, sticker_urls: sts, doc_urls: docs, enabled: true }) });
    if(!r.ok){ throw new Error(await r.text()); }
    await loadTemplates();
    alert('Template dibuat');
  }catch(e){
    alert('Gagal buat template: '+e.message);
  }
}

 // ---- Template: Edit/Save/Delete ----
function startEditTemplate(id){
  var t = tplById[id];
  if (!t) { alert('Template tidak ditemukan'); return; }
  var nameEl = document.getElementById('tpl-name');
  var textEl = document.getElementById('tpl-text');
  if (nameEl) nameEl.value = t.name || '';
  if (textEl) textEl.value = t.text || '';
  var btnSave = document.getElementById('tpl-save'); if (btnSave) btnSave.disabled = false;
  editingTemplateId = id;
  var section = document.getElementById('templates');
  if (section) { window.scrollTo({ top: section.offsetTop - 60, behavior: 'smooth' }); }
}
async function saveTemplate(){
  try{
    if(!editingTemplateId){ alert('Tidak ada template yang sedang diedit'); return; }
    var t = tplById[editingTemplateId] || {};
    var name = document.getElementById('tpl-name') ? document.getElementById('tpl-name').value.trim() : '';
    var text = document.getElementById('tpl-text') ? document.getElementById('tpl-text').value : '';
    if(!name){ alert('Nama wajib'); return; }
    async function upload(kind, file){
      var fd = new FormData(); fd.append('kind', kind); fd.append('file', file);
      var r = await fetch('/api/upload', { method:'POST', body: fd });
      if(!r.ok){ throw new Error(await r.text()); }
      var j = await r.json(); return j.url;
    }
    async function collect(kind, inputId){
      var el = document.getElementById(inputId);
      var urls = [];
      if(el && el.files){
        for(var i=0;i<el.files.length;i++){
          urls.push(await upload(kind, el.files[i]));
        }
      }
      return urls;
    }
    var imgsNew = await collect('image','file-image');
    var vidsNew = await collect('video','file-video');
    var audsNew = await collect('audio','file-audio');
    var stsNew  = await collect('sticker','file-sticker');
    var docsNew = await collect('doc','file-doc');
    var body = {
      name: name,
      text: text,
      image_urls: (t.image_urls||[]).concat(imgsNew||[]),
      video_urls: (t.video_urls||[]).concat(vidsNew||[]),
      audio_urls: (t.audio_urls||[]).concat(audsNew||[]),
      sticker_urls: (t.sticker_urls||[]).concat(stsNew||[]),
      doc_urls: (t.doc_urls||[]).concat(docsNew||[]),
      enabled: !!t.enabled
    };
    var r = await api('/api/templates/'+encodeURIComponent(editingTemplateId), { method:'PUT', body: JSON.stringify(body) });
    if(!r.ok){ throw new Error(await r.text()); }
    // reset state & clear file inputs
    editingTemplateId = null;
    var btnSave = document.getElementById('tpl-save'); if (btnSave) btnSave.disabled = true;
    var ids = ['file-image','file-video','file-audio','file-sticker','file-doc'];
    for(var i=0;i<ids.length;i++){ var el = document.getElementById(ids[i]); if(el){ el.value=''; } }
    await loadTemplates();
    alert('Template diupdate');
  }catch(e){
    alert('Gagal update template: '+e.message);
  }
}
async function deleteTemplate(id){
  try{
    if(!id) return;
    if(!confirm('Hapus template ini?')) return;
    var r = await api('/api/templates/'+encodeURIComponent(id), { method:'DELETE' });
    if(!r.ok){ throw new Error(await r.text()); }
    if (editingTemplateId === id) {
      editingTemplateId = null;
      var btnSave = document.getElementById('tpl-save'); if (btnSave) btnSave.disabled = true;
    }
    await loadTemplates();
    alert('Template dihapus');
  }catch(e){
    alert('Gagal hapus template: '+e.message);
  }
}
// util lama (opsional) untuk append URL ke input text (tidak dipakai di UI sekarang)
async function uploadAndAppend(kind, targetFieldId){
  try{
    var fileEl = document.getElementById('file-'+kind);
    if(!fileEl || !fileEl.files || fileEl.files.length===0){
      alert('Pilih file untuk '+kind);
      return;
    }
    var fd = new FormData();
    fd.append('kind', kind);
    fd.append('file', fileEl.files[0]);
    var r = await fetch('/api/upload', { method:'POST', body: fd });
    if(!r.ok){
      var t = await r.text(); throw new Error(t);
    }
    var j = await r.json();
    var input = document.getElementById(targetFieldId);
    var v = (input.value||'').trim();
    input.value = v ? (v+','+j.url) : j.url;
  }catch(e){
    alert('Upload gagal: '+e.message);
  }
}
// ---- Akun: Edit & Delete ----
function startEditAccount(id){
  var a = accById[id];
  if(!a){
    alert('Akun tidak ditemukan');
    return;
  }
  var labelEl = document.getElementById('acc-label');
  var msisdnEl = document.getElementById('acc-msisdn');
  var limitEl = document.getElementById('acc-limit');
  var enabledEl = document.getElementById('acc-enabled');
  var saveBtn = document.getElementById('acc-save');

  if(labelEl) labelEl.value = a.label || '';
  if(msisdnEl) msisdnEl.value = a.msisdn || '';
  if(limitEl) limitEl.value = String(a.daily_limit || 100);
  if(enabledEl) enabledEl.checked = !!a.enabled;

  editingAccountId = id;
  if(saveBtn) saveBtn.disabled = false;

  var section = document.getElementById('account-create');
  if(section){
    window.scrollTo({ top: section.offsetTop - 60, behavior: 'smooth' });
  }
}

async function saveAccount(){
  try{
    if(!editingAccountId){
      alert('Tidak ada akun yang sedang diedit');
      return;
    }
    var label = document.getElementById('acc-label') ? document.getElementById('acc-label').value.trim() : '';
    var msisdn = document.getElementById('acc-msisdn') ? document.getElementById('acc-msisdn').value.trim() : '';
    var daily = parseInt((document.getElementById('acc-limit') ? document.getElementById('acc-limit').value : '100') || '100', 10);
    var enabled = !!(document.getElementById('acc-enabled') && document.getElementById('acc-enabled').checked);

    if(!label){
      alert('Label wajib');
      return;
    }
    var r = await api('/api/accounts/'+encodeURIComponent(editingAccountId), {
      method: 'PUT',
      body: JSON.stringify({ label: label, msisdn: msisdn, daily_limit: daily, enabled: enabled })
    });
    if(!r.ok){
      throw new Error(await r.text());
    }
    // reset state
    editingAccountId = null;
    var saveBtn = document.getElementById('acc-save'); if (saveBtn) saveBtn.disabled = true;
    alert('Akun diupdate');
    await loadAccounts();
  }catch(e){
    alert('Gagal update akun: '+e.message);
  }
}

async function deleteAccount(id){
  try{
    if(!id) return;
    if(!confirm('Hapus akun ini beserta grup terkait?')){
      return;
    }
    var r = await api('/api/accounts/'+encodeURIComponent(id), { method: 'DELETE' });
    if(!r.ok){
      throw new Error(await r.text());
    }
    if(editingAccountId === id){
      editingAccountId = null;
      var saveBtn = document.getElementById('acc-save'); if (saveBtn) saveBtn.disabled = true;
    }
    alert('Akun dihapus');
    await loadAccounts();
  }catch(e){
    alert('Gagal hapus akun: '+e.message);
  }
}
// ---- End Akun ----
async function boot(){
  bindEvents();
  await pollHealth();
  await loadAccounts();
  await loadStats();
  logsConnect();
  await loadGroupsByNumber();
  await loadTemplates();
  setInterval(pollHealth, 10000);
  setInterval(loadStats, 15000);
}

boot();
</script>
</body>
</html>`
}
