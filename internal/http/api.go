package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

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
	r.Use(middleware.Timeout(60 * time.Second))
	r.Use(cors)

	api.routes()
	return r
}

func (a *API) routes() {
	a.Router.Get("/api/health", a.handleHealth)
	a.Router.Get("/api/accounts", a.handleListAccounts)
	a.Router.Post("/api/accounts", a.handleCreateAccount)
	a.Router.Get("/api/groups", a.handleListGroups)
	a.Router.Post("/api/groups/toggle", a.handleToggleGroup)
	a.Router.Get("/api/stats", a.handleStats)
	// Pairing & connect endpoints
	a.Router.Get("/api/accounts/{id}/pair/qr", a.handleAccountPairQR)
	a.Router.Post("/api/accounts/{id}/pair/number", a.handleAccountPairByNumber)
	a.Router.Post("/api/accounts/{id}/connect", a.handleAccountConnect)
	// Refresh groups from WhatsApp
	a.Router.Post("/api/accounts/{id}/groups/refresh", a.handleRefreshGroups)
	// Send test (manual trigger) endpoint
	a.Router.Post("/api/send/test", a.handleSendTest)
	// Log streaming (SSE)
	a.Router.Get("/api/logs/stream", a.handleLogsStream)
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
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
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
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
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

// Send test API
type sendTestReq struct {
	AccountID string   `json:"account_id"`
	GroupID   string   `json:"group_id"`
	Text      string   `json:"text"`
	ImageURLs []string `json:"image_urls"`
	VideoURLs []string `json:"video_urls"`
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
		Text:      req.Text,
		ImageURLs: req.ImageURLs,
		VideoURLs: req.VideoURLs,
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
body{font-family:system-ui,-apple-system,Segoe UI,Roboto,Arial,sans-serif;background:#0f1115;color:#e6e6e6;margin:0}
header{padding:12px 16px;background:#151821;border-bottom:1px solid #2a2f3a;position:sticky;top:0;z-index:1}
h1{font-size:18px;margin:0}
main{padding:16px;max-width:1100px;margin:0 auto}
section{margin:16px 0;padding:12px;border:1px solid #2a2f3a;border-radius:8px;background:#12151d}
.row{display:flex;gap:12px;flex-wrap:wrap;align-items:center}
button{background:#2b6cb0;border:0;color:#fff;padding:8px 12px;border-radius:6px;cursor:pointer}
button.secondary{background:#394050}
input,select{background:#0d0f14;border:1px solid #2a2f3a;color:#e6e6e6;border-radius:6px;padding:8px 10px}
table{width:100%;border-collapse:collapse;font-size:14px}
th,td{padding:8px;border-bottom:1px solid #2a2f3a}
.ok{color:#7bd88f}.err{color:#ff6b6b}
img.qr{width:220px;height:220px;border:1px solid #2a2f3a;border-radius:8px}
small.mono{font-family:ui-monospace,Menlo,Consolas,monospace;color:#9aa0aa}
</style>
</head>
<body>
<header><h1>Promote WA Dashboard</h1></header>
<main>
<section id="health">
  <div class="row"><strong>Status server:</strong><span id="health-status" class="ok">menunggu...</span><small class="mono" id="health-time"></small></div>
</section>

<section id="account-create">
  <h3>Buat Akun WhatsApp</h3>
  <div class="row">
    <input id="acc-label" placeholder="Label akun (mis. Nomor A)">
    <input id="acc-msisdn" placeholder="MSISDN (opsional)">
    <input id="acc-limit" type="number" min="1" value="100" style="width:120px">
    <button id="acc-create">Tambah Akun</button>
  </div>
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
<section id="send-test">
  <h3>Kirim Uji</h3>
  <div class="row">
    <select id="send-account"></select>
    <input id="send-group-id" placeholder="Group JID (mis. 12345-67890@g.us)" style="width:280px">
  </div>
  <div class="row" style="margin-top:8px">
    <textarea id="send-text" placeholder="Teks promo (gunakan {group_name} untuk personalisasi)" rows="3" style="width:100%"></textarea>
  </div>
  <div class="row" style="margin-top:8px">
    <input id="send-images" placeholder="Image URLs (comma separated)" style="width:48%">
    <input id="send-videos" placeholder="Video URLs (comma separated)" style="width:48%">
    <button id="btn-send-test">Kirim Uji</button>
  </div>
  <small class="mono">Delay natural otomatis antar bagian konten.</small>
</section>
    <button id="btn-refresh" class="secondary">Refresh dari WhatsApp</button>
  </div>
  <table style="margin-top:8px">
    <thead><tr><th>Nama Grup</th><th>Enabled</th><th>Terakhir Kirim</th><th>Risk</th><th>ID</th></tr></thead>
    <tbody id="groups-tbody"></tbody>
  </table>
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
      '<button data-act="refresh" data-id="'+a.id+'" class="secondary">Refresh Grup</button>'+
    '</td>';
  return tr;
}

async function loadAccounts(){
  var r = await api('/api/accounts'); var list = await r.json();
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
  if(!label){ alert('Label wajib'); return; }
  var r = await api('/api/accounts',{method:'POST',body:JSON.stringify({label:label,msisdn:msisdn,daily_limit:daily,enabled:true})});
  if(!r.ok){ var t = await r.text(); alert('Gagal: '+t); return; }
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
    '<td><small class="mono">'+g.id+'</small></td>';
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

async function loadStats(){
  var r = await api('/api/stats'); var j = await r.json();
  $('#s-total').textContent = j.total||0;
  $('#s-success').textContent = j.success||0;
  $('#s-failed').textContent = j.failed||0;
}

function sendTest(){
  var acc = $('#send-account') ? $('#send-account').value : '';
  var gid = $('#send-group-id') ? $('#send-group-id').value.trim() : '';
  var text = $('#send-text') ? $('#send-text').value : '';
  var images = $('#send-images') ? $('#send-images').value.split(',').map(function(s){return s.trim();}).filter(function(s){return s.length>0;}) : [];
  var videos = $('#send-videos') ? $('#send-videos').value.split(',').map(function(s){return s.trim();}).filter(function(s){return s.length>0;}) : [];
  if(!acc || !gid){
    alert('Pilih akun dan isi Group JID');
    return;
  }
  api('/api/send/test', {
    method:'POST',
    body: JSON.stringify({account_id: acc, group_id: gid, text: text, image_urls: images, video_urls: videos})
  }).then(function(r){
    if(!r.ok) return r.text().then(function(t){ throw new Error(t); });
    return r.json();
  }).then(function(){
    alert('Kirim uji diproses');
  }).catch(function(err){
    alert('Gagal kirim: '+err.message);
  });
}

function bindEvents(){
  $('#acc-create').addEventListener('click', createAccount);
  $('#accounts-tbody').addEventListener('click', function(e){
    var btn = e.target.closest('button'); if(!btn) return;
    var id = btn.getAttribute('data-id');
    var act = btn.getAttribute('data-act');
    if(act==='qr') startQRRefresh(id);
    else if(act==='connect') connectAcc(id);
    else if(act==='refresh'){ $('#groups-account').value = id; refreshGroups(); }
  });
  $('#btn-refresh').addEventListener('click', refreshGroups);
  $('#groups-account').addEventListener('change', loadGroups);
  $('#groups-tbody').addEventListener('change', function(e){
    var cb = e.target.closest('.g-toggle'); if(!cb) return;
    toggleGroup(cb.getAttribute('data-id'), cb.checked);
  });
  var btnSend = document.getElementById('btn-send-test');
  if (btnSend) btnSend.addEventListener('click', sendTest);
  var btnPair = document.getElementById('btn-pair-number');
  if (btnPair) btnPair.addEventListener('click', pairByNumber);
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

async function boot(){
  bindEvents();
  await pollHealth();
  await loadAccounts();
  await loadStats();
  logsConnect();
  setInterval(pollHealth, 10000);
  setInterval(loadStats, 15000);
}

boot();
</script>
</body>
</html>`
}
