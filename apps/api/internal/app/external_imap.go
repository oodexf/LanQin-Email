package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/go-chi/chi/v5"
	"golang.org/x/oauth2"
)

const (
	externalIMAPStorageLocal  = "local"
	externalIMAPStorageRemote = "remote"
	externalIMAPTLS           = "tls"
	externalIMAPStartTLS      = "starttls"
	externalIMAPPlain         = "plain"
	externalIMAPMaxFetch      = 30
	externalIMAPAuthPassword  = "password"
	externalIMAPAuthOAuth2    = "oauth2"
	externalIMAPOAuthGmail    = "gmail"
	externalIMAPOAuthOutlook  = "outlook"
	externalIMAPSyncBatchSize = 10
	externalIMAPSyncMaxPerRun = 100
)

type externalIMAPClientFactory interface {
	openExternalIMAPClient(ctx context.Context, account externalIMAPAccountRecord) (externalIMAPClient, error)
}

type externalIMAPClient interface {
	Close() error
	ListFolders(ctx context.Context) ([]externalIMAPRemoteFolder, error)
	FetchSummaries(ctx context.Context, folder string, query string, cursor string, limit int) ([]externalIMAPRemoteMessage, string, int, error)
	FetchNew(ctx context.Context, folder string, afterUID uint32, limit int) ([]externalIMAPRemoteMessage, error)
	FetchRaw(ctx context.Context, folder string, uid uint32) ([]byte, externalIMAPRemoteMessage, error)
	FetchAttachments(ctx context.Context, folder string, uid uint32) ([]Attachment, error)
	FetchPart(ctx context.Context, folder string, uid uint32, partID string) ([]byte, Attachment, error)
	SetRead(ctx context.Context, folder string, uid uint32, read bool) error
	Append(ctx context.Context, folder string, raw []byte) error
}

type externalIMAPAccountRecord struct {
	ExternalIMAPAccount
	SMTPPasswordCiphertext      string
	PasswordCiphertext          string
	OAuthAccessTokenCiphertext  string
	OAuthRefreshTokenCiphertext string
	OAuthSubject                string
	OAuthGeneration             int64
	OAuthExpiry                 time.Time
}

type externalIMAPRemoteFolder struct {
	SentSpecialUse bool
	Name           string
	Role           string
	UnreadCount    int
	TotalCount     int
	UIDValidity    uint32
}

type externalIMAPRemoteMessage struct {
	UID         uint32
	UIDValidity uint32
	Folder      string
	MessageID   string
	Subject     string
	From        string
	FromName    string
	To          []string
	CC          []string
	SentAt      time.Time
	ReceivedAt  time.Time
	Snippet     string
	IsRead      bool
	SizeBytes   int64
	Raw         []byte
	Attachments []Attachment
}

type externalIMAPAttachmentPart struct {
	Attachment
	Encoding string
}

type externalIMAPPayload struct {
	ProviderPreset string `json:"providerPreset"`
	SendEnabled    *bool  `json:"sendEnabled"`
	SMTPHost       string `json:"smtpHost"`
	SMTPPort       int    `json:"smtpPort"`
	SMTPTLSMode    string `json:"smtpTlsMode"`
	SMTPUsername   string `json:"smtpUsername"`
	SMTPPassword   string `json:"smtpPassword"`
	SentCopyMode   string `json:"sentCopyMode"`
	MailboxID      string `json:"mailboxId"`
	Name           string `json:"name"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	TLSMode        string `json:"tlsMode"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	StorageMode    string `json:"storageMode"`
	SyncReadState  *bool  `json:"syncReadState"`
	Enabled        *bool  `json:"enabled"`
}

type externalIMAPOAuthStartPayload struct {
	AccountID     string `json:"accountId,omitempty"`
	MailboxID     string `json:"mailboxId"`
	Name          string `json:"name"`
	Email         string `json:"email"`
	StorageMode   string `json:"storageMode"`
	SyncReadState *bool  `json:"syncReadState"`
	Enabled       *bool  `json:"enabled"`
}

type externalIMAPOAuthState struct {
	Verifier      string `json:"verifier"`
	Nonce         string `json:"nonce"`
	Name          string `json:"name"`
	Email         string `json:"email"`
	StorageMode   string `json:"storageMode"`
	SyncReadState bool   `json:"syncReadState"`
	Enabled       bool   `json:"enabled"`
}

func (a *App) externalIMAPWorker(ctx context.Context) {
	interval := time.Duration(a.cfg.ExternalIMAPSyncSeconds) * time.Second
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.syncDueExternalIMAPAccounts(ctx)
		}
	}
}

func (a *App) syncDueExternalIMAPAccounts(ctx context.Context) {
	if !a.cfg.ExternalIMAPEnabled {
		return
	}
	rows, err := a.db.QueryContext(ctx, `SELECT id FROM external_imap_accounts WHERE enabled=1 AND auth_status='ok' AND storage_mode=? ORDER BY COALESCE(last_sync_at, created_at) ASC LIMIT 10`, externalIMAPStorageLocal)
	if err != nil {
		a.log.Warn("failed to list external imap accounts", "error", err)
		return
	}
	defer rows.Close()
	var accountIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			a.log.Warn("failed to scan external imap account", "error", err)
			return
		}
		accountIDs = append(accountIDs, id)
	}
	if err := rows.Err(); err != nil {
		a.log.Warn("failed to list external imap accounts", "error", err)
		return
	}
	// Release the result set before synchronization queries the database again.
	// SQLite uses a single connection, so retaining rows would block all DB access.
	if err := rows.Close(); err != nil {
		a.log.Warn("failed to close external imap accounts", "error", err)
		return
	}
	for _, id := range accountIDs {
		if ctx.Err() != nil {
			return
		}
		runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		_, _ = a.syncExternalIMAPAccount(runCtx, id)
		cancel()
	}
}

func (a *App) requireExternalIMAPEnabled(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.cfg.ExternalIMAPEnabled {
			respondError(w, http.StatusForbidden, "external imap is disabled")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) handleListExternalIMAPAccounts(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	mailboxID := strings.TrimSpace(r.URL.Query().Get("mailboxId"))
	args := []any{user.ID}
	where := "user_id=?"
	if mailboxID != "" {
		if _, err := a.mailboxForUserByID(r.Context(), user.ID, mailboxID); err != nil {
			respondError(w, http.StatusNotFound, "mailbox not found")
			return
		}
		where += " AND mailbox_id=?"
		args = append(args, mailboxID)
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT `+externalIMAPAccountColumns+` FROM external_imap_accounts WHERE `+where+` ORDER BY created_at DESC`, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load external accounts")
		return
	}
	defer rows.Close()
	items := []ExternalIMAPAccount{}
	for rows.Next() {
		item, err := scanExternalIMAPAccount(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan external accounts")
			return
		}
		items = append(items, item.ExternalIMAPAccount)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleCreateExternalIMAPAccount(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	var req externalIMAPPayload
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	var presetErr error
	req.ProviderPreset, presetErr = normalizeExternalMailProvider(req.ProviderPreset)
	if presetErr != nil {
		badRequest(w, presetErr)
		return
	}
	mb, err := a.mailboxForUserByID(r.Context(), user.ID, strings.TrimSpace(req.MailboxID))
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	req.MailboxID = mb.ID
	if strings.TrimSpace(req.Password) == "" {
		badRequest(w, errors.New("password is required"))
		return
	}
	normalized, err := a.normalizeExternalIMAPPayload(r.Context(), req, true)
	if err != nil {
		badRequest(w, err)
		return
	}
	ciphertext, err := a.encryptExternalIMAPPassword(normalized.Password)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	smtpCipher, err := a.externalSMTPPasswordCipher(normalized.SMTPPassword, "")
	if err != nil {
		badRequest(w, errors.New("failed to encrypt smtp password"))
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	id := newID("ximap")
	if _, err := a.db.ExecContext(r.Context(), `INSERT INTO external_imap_accounts(id,user_id,mailbox_id,name,host,port,tls_mode,username,password_ciphertext,storage_mode,sync_read_state,enabled,last_status,created_at,updated_at,smtp_host,smtp_port,smtp_tls_mode,smtp_username,smtp_password_ciphertext,send_enabled,sent_copy_mode,provider_preset)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, user.ID, normalized.MailboxID, normalized.Name, normalized.Host, normalized.Port, normalized.TLSMode, normalized.Username, ciphertext, normalized.StorageMode, boolInt(*normalized.SyncReadState), boolInt(*normalized.Enabled), "idle", now, now, normalized.SMTPHost, normalized.SMTPPort, normalized.SMTPTLSMode, normalized.SMTPUsername, smtpCipher, boolInt(*normalized.SendEnabled), normalized.SentCopyMode, normalized.ProviderPreset); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create external account")
		return
	}
	account, err := a.externalIMAPAccountForUser(r.Context(), user.ID, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load external account")
		return
	}
	respondJSON(w, http.StatusCreated, account.ExternalIMAPAccount)
}

func (a *App) handleUpdateExternalIMAPAccount(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	id := chi.URLParam(r, "id")
	current, err := a.externalIMAPAccountForUser(r.Context(), user.ID, id)
	if err != nil {
		respondError(w, http.StatusNotFound, "external account not found")
		return
	}
	req := externalIMAPPayload{ProviderPreset: current.ProviderPreset, SendEnabled: &current.SendEnabled, SMTPHost: current.SMTPHost, SMTPPort: current.SMTPPort, SMTPTLSMode: current.SMTPTLSMode, SMTPUsername: current.SMTPUsername, SentCopyMode: current.SentCopyMode, MailboxID: current.MailboxID, Name: current.Name, Host: current.Host, Port: current.Port, TLSMode: current.TLSMode, Username: current.Username, StorageMode: current.StorageMode, SyncReadState: &current.SyncReadState, Enabled: &current.Enabled}
	if current.AuthMode == externalIMAPAuthOAuth2 {
		req.ProviderPreset = current.OAuthProvider
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if _, err := normalizeExternalMailProvider(req.ProviderPreset); err != nil {
		badRequest(w, err)
		return
	}
	if current.AuthMode == externalIMAPAuthOAuth2 {
		if req.ProviderPreset != current.OAuthProvider {
			badRequest(w, errors.New("oauth account provider does not match"))
			return
		}
		if req.Host != current.Host || req.Port != current.Port || req.TLSMode != current.TLSMode || req.Username != current.Username {
			badRequest(w, errors.New("oauth account endpoints cannot be changed"))
			return
		}
		host, port, mode := externalOAuthSMTPEndpoint(current.OAuthProvider)
		if (req.SMTPHost != "" && req.SMTPHost != host) || (req.SMTPPort != 0 && req.SMTPPort != port) || (req.SMTPTLSMode != "" && req.SMTPTLSMode != mode) || req.SMTPUsername != current.SMTPUsername || req.SMTPPassword != "" {
			badRequest(w, errors.New("oauth account endpoints cannot be changed"))
			return
		}
		req.SMTPHost, req.SMTPPort, req.SMTPTLSMode = host, port, mode
		req.SMTPUsername = current.OAuthEmail
		req.Password = ""
	}
	if strings.TrimSpace(req.MailboxID) == "" {
		req.MailboxID = current.MailboxID
	}
	if _, err := a.mailboxForUserByID(r.Context(), user.ID, strings.TrimSpace(req.MailboxID)); err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	normalized, err := a.normalizeExternalIMAPPayload(r.Context(), req, false)
	if err != nil {
		badRequest(w, err)
		return
	}
	ciphertext := current.PasswordCiphertext
	if strings.TrimSpace(normalized.Password) != "" {
		ciphertext, err = a.encryptExternalIMAPPassword(normalized.Password)
		if err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	smtpCipher, err := a.externalSMTPPasswordCipher(normalized.SMTPPassword, current.SMTPPasswordCiphertext)
	if err != nil {
		badRequest(w, errors.New("failed to encrypt smtp password"))
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	_, err = a.db.ExecContext(r.Context(), `UPDATE external_imap_accounts SET mailbox_id=?,name=?,host=?,port=?,tls_mode=?,username=?,password_ciphertext=?,storage_mode=?,sync_read_state=?,enabled=?,updated_at=?,smtp_host=?,smtp_port=?,smtp_tls_mode=?,smtp_username=?,smtp_password_ciphertext=?,send_enabled=?,sent_copy_mode=?,provider_preset=? WHERE id=? AND user_id=?`,
		normalized.MailboxID, normalized.Name, normalized.Host, normalized.Port, normalized.TLSMode, normalized.Username, ciphertext, normalized.StorageMode, boolInt(*normalized.SyncReadState), boolInt(*normalized.Enabled), now, normalized.SMTPHost, normalized.SMTPPort, normalized.SMTPTLSMode, normalized.SMTPUsername, smtpCipher, boolInt(*normalized.SendEnabled), normalized.SentCopyMode, normalized.ProviderPreset, id, user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update external account")
		return
	}
	account, err := a.externalIMAPAccountForUser(r.Context(), user.ID, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load external account")
		return
	}
	respondJSON(w, http.StatusOK, account.ExternalIMAPAccount)
}

func (a *App) handleDeleteExternalIMAPAccount(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	id := chi.URLParam(r, "id")
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, 500, "failed to delete external account")
		return
	}
	defer tx.Rollback()
	// Ownership is checked inside the transaction before touching any queue rows.
	var owned string
	if err := tx.QueryRowContext(r.Context(), `SELECT id FROM external_imap_accounts WHERE id=? AND user_id=?`, id, user.ID).Scan(&owned); err != nil {
		respondError(w, 404, "external account not found")
		return
	}
	_, err = tx.ExecContext(r.Context(), `UPDATE send_queue SET status='failed',attempt_count=max_attempts,mime_base64='',last_error='external account removed',updated_at=? WHERE route='external' AND external_account_id=? AND user_id=? AND (status='queued' OR (status='failed' AND attempt_count<max_attempts))`, a.now().UTC().Format(time.RFC3339Nano), id, user.ID)
	if err != nil {
		respondError(w, 500, "failed to delete external account")
		return
	}
	if _, err = tx.ExecContext(r.Context(), `DELETE FROM external_imap_accounts WHERE id=? AND user_id=?`, id, user.ID); err != nil {
		respondError(w, 500, "failed to delete external account")
		return
	}
	if err = tx.Commit(); err != nil {
		respondError(w, 500, "failed to delete external account")
		return
	}

	respondJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleTestExternalIMAPAccount(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	account, err := a.externalIMAPAccountForUser(r.Context(), user.ID, chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusNotFound, "external account not found")
		return
	}
	if account.AuthStatus == "needs_reauth" {
		badRequest(w, errExternalMailReauth)
		return
	}
	client, err := a.externalIMAP.openExternalIMAPClient(r.Context(), account)
	if err != nil {
		a.updateExternalIMAPStatus(r.Context(), account.ID, "error", err.Error())
		respondError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return
	}
	defer client.Close()
	folders, err := client.ListFolders(r.Context())
	if err != nil {
		a.updateExternalIMAPStatus(r.Context(), account.ID, "error", err.Error())
		respondError(w, http.StatusBadRequest, "list folders failed: "+err.Error())
		return
	}
	a.updateExternalIMAPStatus(r.Context(), account.ID, "ok", "")
	respondJSON(w, http.StatusOK, map[string]any{"ok": true, "folders": len(folders)})
}

func (a *App) handleSyncExternalIMAPAccount(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	account, err := a.externalIMAPAccountForUser(r.Context(), user.ID, chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusNotFound, "external account not found")
		return
	}
	if account.StorageMode != externalIMAPStorageLocal {
		badRequest(w, errors.New("remote storage accounts do not sync into local mailbox"))
		return
	}
	run, err := a.syncExternalIMAPAccount(r.Context(), account.ID)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, run)
}

func (a *App) handleExternalIMAPSyncRuns(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	account, err := a.externalIMAPAccountForUser(r.Context(), user.ID, chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusNotFound, "external account not found")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,account_id,folder,status,imported,skipped,failed,error,started_at,finished_at FROM external_imap_sync_runs WHERE account_id=? ORDER BY started_at DESC LIMIT 20`, account.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load sync runs")
		return
	}
	defer rows.Close()
	items := []ExternalIMAPSyncRun{}
	for rows.Next() {
		run, err := scanExternalIMAPSyncRun(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan sync runs")
			return
		}
		items = append(items, run)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleSyncExternalIMAPFolder(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	account, err := a.externalIMAPAccountForUser(r.Context(), user.ID, chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusNotFound, "external account not found")
		return
	}
	if account.StorageMode != externalIMAPStorageLocal {
		badRequest(w, errors.New("remote storage accounts do not sync into local mailbox"))
		return
	}
	var req struct {
		Folder string `json:"folder"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	folder := strings.TrimSpace(req.Folder)
	if folder == "" {
		badRequest(w, errors.New("folder is required"))
		return
	}
	run, err := a.syncExternalIMAPAccountFolder(r.Context(), account.ID, folder)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, run)
}

func (a *App) handleMailExternalAccounts(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	rows, err := a.db.QueryContext(r.Context(), `SELECT `+externalIMAPAccountColumns+` FROM external_imap_accounts WHERE user_id=? AND enabled=1 ORDER BY name`, user.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load external accounts")
		return
	}
	defer rows.Close()
	items := []ExternalIMAPAccount{}
	for rows.Next() {
		item, err := scanExternalIMAPAccount(rows)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to scan external accounts")
			return
		}
		items = append(items, item.ExternalIMAPAccount)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleExternalIMAPFolders(w http.ResponseWriter, r *http.Request) {
	account, ok := a.externalIMAPAccountForMailRequest(w, r)
	if !ok {
		return
	}
	client, err := a.externalIMAP.openExternalIMAPClient(r.Context(), account)
	if err != nil {
		respondError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return
	}
	defer client.Close()
	folders, err := client.ListFolders(r.Context())
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to list folders")
		return
	}
	items := make([]ExternalIMAPFolder, 0, len(folders))
	for _, folder := range folders {
		if account.StorageMode == externalIMAPStorageLocal {
			var total, unread int
			_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(m.id),COALESCE(SUM(CASE WHEN m.is_read=0 THEN 1 ELSE 0 END),0)
				FROM external_imap_messages em JOIN messages m ON m.id=em.local_message_id
				WHERE em.account_id=? AND em.remote_folder=? AND m.mailbox_id=?`, account.ID, folder.Name, account.MailboxID).Scan(&total, &unread)
			folder.TotalCount = total
			folder.UnreadCount = unread
		}
		items = append(items, ExternalIMAPFolder{Name: folder.Name, Role: folder.Role, UnreadCount: folder.UnreadCount, TotalCount: folder.TotalCount})
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleExternalIMAPMessages(w http.ResponseWriter, r *http.Request) {
	account, ok := a.externalIMAPAccountForMailRequest(w, r)
	if !ok {
		return
	}
	folder := strings.TrimSpace(r.URL.Query().Get("folder"))
	if folder == "" {
		folder = "INBOX"
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	limit, err := mailMessagePageLimit(r)
	if err != nil {
		badRequest(w, err)
		return
	}
	if account.StorageMode == externalIMAPStorageLocal {
		access := &mailboxReadAccess{Mailbox: &Mailbox{ID: account.MailboxID, UserID: account.UserID}, Owner: true}
		a.respondMailMessageListInternal(w, r, access,
			`m.mailbox_id=? AND EXISTS (SELECT 1 FROM external_imap_messages em WHERE em.local_message_id=m.id AND em.account_id=? AND em.remote_folder=?)`,
			[]any{account.MailboxID, account.ID, folder}, true, account.ID)
		return
	}
	client, err := a.externalIMAP.openExternalIMAPClient(r.Context(), account)
	if err != nil {
		respondError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return
	}
	defer client.Close()
	remote, next, totalCount, err := client.FetchSummaries(r.Context(), folder, query, cursor, limit)
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to load remote messages")
		return
	}
	items := make([]MailMessage, 0, len(remote))
	for _, msg := range remote {
		items = append(items, externalRemoteMessageToMailMessage(account, msg, false))
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next, "totalCount": totalCount})
}

func (a *App) handleExternalIMAPMessage(w http.ResponseWriter, r *http.Request) {
	account, ok := a.externalIMAPAccountForMailRequest(w, r)
	if !ok {
		return
	}
	if account.StorageMode == externalIMAPStorageLocal {
		messageID := chi.URLParam(r, "remoteId")
		var mappedID string
		if err := a.db.QueryRowContext(r.Context(), `SELECT em.local_message_id FROM external_imap_messages em JOIN messages m ON m.id=em.local_message_id
			WHERE em.account_id=? AND em.local_message_id=? AND m.mailbox_id=? LIMIT 1`, account.ID, messageID, account.MailboxID).Scan(&mappedID); err != nil {
			respondError(w, http.StatusNotFound, "message not found")
			return
		}
		msg, err := a.messageByID(r.Context(), mappedID, true)
		if err != nil {
			respondError(w, http.StatusNotFound, "message not found")
			return
		}
		msg.ExternalAccountID = account.ID
		if len(msg.Attachments) == 0 {
			var rawPath string
			if err := a.db.QueryRowContext(r.Context(), `SELECT raw_path FROM messages WHERE id=? AND mailbox_id=?`, mappedID, account.MailboxID).Scan(&rawPath); err == nil {
				if info, err := os.Stat(rawPath); err == nil {
					msg.Attachments = []Attachment{{ID: "raw", MessageID: msg.ID, Filename: safeExternalEMLFilename(msg.Subject), ContentType: "message/rfc822", SizeBytes: info.Size(), CreatedAt: a.now().UTC()}}
					msg.HasAttachments = true
				}
			}
		}
		respondJSON(w, http.StatusOK, msg)
		return
	}
	folder, uid, ok := decodeExternalRemoteID(w, chi.URLParam(r, "remoteId"))
	if !ok {
		return
	}
	client, err := a.externalIMAP.openExternalIMAPClient(r.Context(), account)
	if err != nil {
		respondError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return
	}
	defer client.Close()
	raw, remote, err := client.FetchRaw(r.Context(), folder, uid)
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to load remote message")
		return
	}
	stored, attachments, err := a.parseMaildirMessage(raw, account.Username)
	msg := externalRemoteMessageToMailMessage(account, remote, true)
	if err == nil {
		msg.BodyText = stored.BodyText
		msg.BodyHTML = stored.BodyHTML
		msg.Snippet = stored.Snippet
		if len(attachments) > 0 {
			msg.HasAttachments = true
		}
	}
	if parts, err := client.FetchAttachments(r.Context(), folder, uid); err == nil {
		for i := range parts {
			parts[i].MessageID = msg.ID
		}
		msg.Attachments = parts
		msg.HasAttachments = len(parts) > 0
	}
	if len(msg.Attachments) == 0 {
		msg.Attachments = []Attachment{{ID: "raw", MessageID: msg.ID, Filename: safeExternalEMLFilename(msg.Subject), ContentType: "message/rfc822", SizeBytes: int64(len(raw)), CreatedAt: a.now().UTC()}}
	}
	respondJSON(w, http.StatusOK, msg)
}

func (a *App) handleExternalIMAPAttachment(w http.ResponseWriter, r *http.Request) {
	account, ok := a.externalIMAPAccountForMailRequest(w, r)
	if !ok {
		return
	}
	partID := chi.URLParam(r, "partId")
	if account.StorageMode == externalIMAPStorageLocal {
		messageID := chi.URLParam(r, "remoteId")
		var exists int
		if err := a.db.QueryRowContext(r.Context(), `SELECT 1 FROM external_imap_messages em JOIN messages m ON m.id=em.local_message_id
			WHERE em.account_id=? AND em.local_message_id=? AND m.mailbox_id=? LIMIT 1`, account.ID, messageID, account.MailboxID).Scan(&exists); err != nil {
			respondError(w, http.StatusNotFound, "message not found")
			return
		}
		if partID != "raw" {
			var filename, contentType, path string
			var size int64
			if err := a.db.QueryRowContext(r.Context(), `SELECT att.filename,att.content_type,att.storage_path,att.size_bytes
				FROM attachments att JOIN external_imap_messages em ON em.local_message_id=att.message_id
				WHERE att.id=? AND em.account_id=? AND em.local_message_id=?`, partID, account.ID, messageID).Scan(&filename, &contentType, &path, &size); err != nil {
				respondError(w, http.StatusNotFound, "attachment not found")
				return
			}
			data, err := os.ReadFile(path)
			if err != nil {
				respondError(w, http.StatusNotFound, "attachment file missing")
				return
			}
			writeAttachmentHeaders(w, contentType, filename, size)
			_, _ = w.Write(data)
			return
		}
		var rawPath, subject string
		if err := a.db.QueryRowContext(r.Context(), `SELECT m.raw_path,m.subject FROM messages m
			JOIN external_imap_messages em ON em.local_message_id=m.id
			WHERE m.id=? AND m.mailbox_id=? AND em.account_id=?`, messageID, account.MailboxID, account.ID).Scan(&rawPath, &subject); err != nil {
			respondError(w, http.StatusNotFound, "message not found")
			return
		}
		raw, err := os.ReadFile(rawPath)
		if err != nil {
			respondError(w, http.StatusNotFound, "message not found")
			return
		}
		writeAttachmentHeaders(w, "message/rfc822", safeExternalEMLFilename(subject), int64(len(raw)))
		_, _ = w.Write(raw)
		return
	}
	folder, uid, ok := decodeExternalRemoteID(w, chi.URLParam(r, "remoteId"))
	if !ok {
		return
	}
	client, err := a.externalIMAP.openExternalIMAPClient(r.Context(), account)
	if err != nil {
		respondError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return
	}
	defer client.Close()
	if partID != "raw" {
		data, att, err := client.FetchPart(r.Context(), folder, uid, partID)
		if err != nil {
			respondError(w, http.StatusNotFound, "attachment not found")
			return
		}
		writeAttachmentHeaders(w, att.ContentType, att.Filename, int64(len(data)))
		_, _ = w.Write(data)
		return
	}
	raw, remote, err := client.FetchRaw(r.Context(), folder, uid)
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to load remote message")
		return
	}
	writeAttachmentHeaders(w, "message/rfc822", safeExternalEMLFilename(remote.Subject), int64(len(raw)))
	_, _ = w.Write(raw)
}

func (a *App) handleExternalIMAPMarkRead(w http.ResponseWriter, r *http.Request) {
	account, ok := a.externalIMAPAccountForMailRequest(w, r)
	if !ok {
		return
	}
	if account.StorageMode != externalIMAPStorageLocal && !account.SyncReadState {
		badRequest(w, errors.New("read state sync is disabled"))
		return
	}
	var req struct {
		Read *bool `json:"read"`
	}
	_ = decodeJSON(r, &req)
	read := true
	if req.Read != nil {
		read = *req.Read
	}
	if account.StorageMode == externalIMAPStorageLocal {
		messageID := chi.URLParam(r, "remoteId")
		var folder string
		var uid uint32
		if err := a.db.QueryRowContext(r.Context(), `SELECT em.remote_folder,em.uid FROM external_imap_messages em JOIN messages m ON m.id=em.local_message_id
			WHERE em.account_id=? AND em.local_message_id=? AND m.mailbox_id=? LIMIT 1`, account.ID, messageID, account.MailboxID).Scan(&folder, &uid); err != nil {
			respondError(w, http.StatusNotFound, "message not found")
			return
		}
		msg, err := a.loadMessageForRequest(r, messageID, false)
		if err != nil || msg.MailboxID != account.MailboxID {
			respondError(w, http.StatusNotFound, "message not found")
			return
		}
		if err := a.updateMessageMaildirFlags(r.Context(), msg.ID, &read, nil); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to update message")
			return
		}
		modSeq, err := a.updateMessageModSeq(r.Context(), msg.ID, msg.FolderID)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to update message")
			return
		}
		if _, err := a.db.ExecContext(r.Context(), `UPDATE messages SET is_read=?, imap_modseq=CASE WHEN ? > 0 THEN ? ELSE imap_modseq END,updated_at=? WHERE id=?`, boolInt(read), modSeq, modSeq, a.now().UTC().Format(time.RFC3339Nano), msg.ID); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to update message")
			return
		}
		remoteSynced := false
		if account.SyncReadState {
			client, err := a.externalIMAP.openExternalIMAPClient(r.Context(), account)
			if err == nil {
				if err = client.SetRead(r.Context(), folder, uid, read); err == nil {
					remoteSynced = true
				}
				_ = client.Close()
			}
			if err != nil {
				a.log.Warn("failed to sync external imap read state", "account", account.ID, "error", err)
			}
		}
		respondJSON(w, http.StatusOK, map[string]any{"ok": true, "read": read, "remoteSynced": remoteSynced})
		return
	}
	folder, uid, ok := decodeExternalRemoteID(w, chi.URLParam(r, "remoteId"))
	if !ok {
		return
	}
	client, err := a.externalIMAP.openExternalIMAPClient(r.Context(), account)
	if err != nil {
		respondError(w, http.StatusBadRequest, "connection failed: "+err.Error())
		return
	}
	defer client.Close()
	if err := client.SetRead(r.Context(), folder, uid, read); err != nil {
		respondError(w, http.StatusBadRequest, "failed to update remote message")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"ok": true, "read": read})
}

func (a *App) normalizeExternalIMAPPayload(ctx context.Context, req externalIMAPPayload, create bool) (externalIMAPPayload, error) {
	var err error
	req.ProviderPreset, err = normalizeExternalMailProvider(req.ProviderPreset)
	if err != nil {
		return req, err
	}
	req.Name = strings.Join(strings.Fields(req.Name), " ")
	if req.Name == "" {
		req.Name = req.Username
	}
	if req.Name == "" {
		req.Name = req.Host
	}
	if len([]rune(req.Name)) > 80 {
		return req, errors.New("name is too long")
	}
	req.Host = strings.ToLower(strings.TrimSpace(req.Host))
	if req.Host == "" {
		return req, errors.New("host is required")
	}
	if err := a.validateExternalIMAPHost(ctx, req.Host); err != nil {
		return req, err
	}
	req.TLSMode = strings.ToLower(strings.TrimSpace(req.TLSMode))
	if req.TLSMode == "" {
		req.TLSMode = externalIMAPTLS
	}
	if req.TLSMode != externalIMAPTLS && req.TLSMode != externalIMAPStartTLS && req.TLSMode != externalIMAPPlain {
		return req, errors.New("invalid TLS mode")
	}
	if req.Port <= 0 {
		if req.TLSMode == externalIMAPTLS {
			req.Port = 993
		} else {
			req.Port = 143
		}
	}
	if req.Port <= 0 || req.Port > 65535 {
		return req, errors.New("invalid port")
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		return req, errors.New("username is required")
	}
	req.StorageMode = strings.ToLower(strings.TrimSpace(req.StorageMode))
	if req.StorageMode == "" {
		req.StorageMode = externalIMAPStorageLocal
	}
	if req.StorageMode != externalIMAPStorageLocal && req.StorageMode != externalIMAPStorageRemote {
		return req, errors.New("invalid storage mode")
	}
	if req.SyncReadState == nil {
		v := true
		req.SyncReadState = &v
	}
	if req.Enabled == nil {
		v := true
		req.Enabled = &v
	}
	if err := a.normalizeExternalSMTPPayload(ctx, &req); err != nil {
		return req, err
	}
	return req, nil
}

func (a *App) validateExternalIMAPHost(ctx context.Context, host string) error {
	if a.cfg.ExternalIMAPAllowPrivateHosts {
		return nil
	}
	if strings.EqualFold(host, "localhost") {
		return errors.New("localhost is not allowed")
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		if ip := net.ParseIP(host); ip != nil {
			ips = []net.IP{ip}
		} else {
			return fmt.Errorf("failed to resolve host: %w", err)
		}
	}
	for _, ip := range ips {
		if !isPublicUnicastIP(ip) {
			return errors.New("private or local IMAP hosts are not allowed")
		}
	}
	return nil
}

// externalMailDialer returns a dialer that re-checks the destination after DNS
// resolution.
//
// validateExternalIMAPHost resolves the hostname itself, so on its own it leaves
// a DNS rebinding window: a hostile resolver can answer with a public address
// for the check and a private one for the connection that follows. Dialer.Control
// runs once per connection attempt with the address actually being dialed, which
// is the only point where the check cannot be raced.
func (a *App) externalMailDialer() *net.Dialer {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if a.cfg.ExternalIMAPAllowPrivateHosts {
		return dialer
	}
	dialer.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if !isPublicUnicastIP(net.ParseIP(host)) {
			return errors.New("private or local IMAP hosts are not allowed")
		}
		return nil
	}
	return dialer
}

func (a *App) encryptExternalIMAPPassword(password string) (string, error) {
	key, err := a.externalIMAPKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := append(nonce, gcm.Seal(nil, nonce, []byte(password), nil)...)
	return base64.StdEncoding.EncodeToString(out), nil
}

func (a *App) decryptExternalIMAPPassword(ciphertext string) (string, error) {
	key, err := a.externalIMAPKey()
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("invalid encrypted password")
	}
	nonce, data := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, data, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (a *App) externalIMAPKey() ([]byte, error) {
	secret := strings.TrimSpace(a.cfg.ExternalIMAPSecretKey)
	if secret == "" {
		return nil, errors.New("LANQIN_EXTERNAL_IMAP_SECRET_KEY is required")
	}
	sum := sha256.Sum256([]byte(secret))
	return sum[:], nil
}

type externalIMAPOAuthProvider struct {
	TLSMode string
	Name    string
	Host    string
	Port    int
	Issuer  string // Only set by the injected provider configuration in tests.
}

func (a *App) externalIMAPOAuthConfig(provider string) (*oauth2.Config, externalIMAPOAuthProvider, error) {
	if a.externalIMAPOAuthConfigForTest != nil {
		return a.externalIMAPOAuthConfigForTest(provider)
	}
	preset, _ := externalMailProviderByID(provider)
	callback := strings.TrimRight(a.cfg.PublicBaseURL, "/") + "/api/external-imap-oauth/" + provider + "/callback"
	switch provider {
	case externalIMAPOAuthGmail:
		if a.cfg.ExternalIMAPGmailClientID == "" || a.cfg.ExternalIMAPGmailClientSecret == "" {
			return nil, externalIMAPOAuthProvider{}, errors.New("gmail oauth is not configured")
		}
		return &oauth2.Config{
			ClientID:     a.cfg.ExternalIMAPGmailClientID,
			ClientSecret: a.cfg.ExternalIMAPGmailClientSecret,
			RedirectURL:  callback,
			Scopes:       []string{"openid", "email", "profile", "https://mail.google.com/"},
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
				TokenURL: "https://oauth2.googleapis.com/token",
			},
		}, externalIMAPOAuthProvider{Name: preset.Name, Host: preset.IMAPHost, Port: preset.IMAPPort, TLSMode: preset.IMAPTLSMode}, nil
	case externalIMAPOAuthOutlook:
		if a.cfg.ExternalIMAPOutlookClientID == "" || a.cfg.ExternalIMAPOutlookClientSecret == "" {
			return nil, externalIMAPOAuthProvider{}, errors.New("outlook oauth is not configured")
		}
		return &oauth2.Config{
			ClientID:     a.cfg.ExternalIMAPOutlookClientID,
			ClientSecret: a.cfg.ExternalIMAPOutlookClientSecret,
			RedirectURL:  callback,
			Scopes:       []string{"openid", "email", "profile", "offline_access", "https://outlook.office.com/IMAP.AccessAsUser.All", "https://outlook.office.com/SMTP.Send"},
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
				TokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token",
			},
		}, externalIMAPOAuthProvider{Name: preset.Name, Host: preset.IMAPHost, Port: preset.IMAPPort, TLSMode: preset.IMAPTLSMode}, nil
	default:
		return nil, externalIMAPOAuthProvider{}, errors.New("unsupported oauth provider")
	}
}

func externalIMAPOAuthEmail(provider string, token *oauth2.Token) (string, error) {
	if token == nil {
		return "", errors.New("oauth token is missing")
	}
	idToken, _ := token.Extra("id_token").(string)
	claims, err := externalIMAPOIDCClaims(idToken)
	if err != nil {
		return "", err
	}
	fields := []string{"email"}
	if provider == externalIMAPOAuthOutlook {
		fields = []string{"email", "preferred_username", "upn"}
	}
	for _, field := range fields {
		value, _ := claims[field].(string)
		email := normalizeEmail(value)
		if email != "" && strings.Contains(email, "@") {
			return email, nil
		}
	}
	return "", errors.New("oauth provider did not return an email address")
}

func externalIMAPOIDCClaims(idToken string) (map[string]any, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return nil, errors.New("oauth provider did not return an id token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("invalid oauth id token")
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, errors.New("invalid oauth id token")
	}
	return claims, nil
}

func (a *App) externalIMAPAccountForUser(ctx context.Context, userID, id string) (externalIMAPAccountRecord, error) {
	row := a.db.QueryRowContext(ctx, `SELECT `+externalIMAPAccountColumns+` FROM external_imap_accounts WHERE id=? AND user_id=?`, id, userID)
	return scanExternalIMAPAccount(row)
}

func (a *App) externalIMAPAccountForMailRequest(w http.ResponseWriter, r *http.Request) (externalIMAPAccountRecord, bool) {
	user := currentUser(r)
	account, err := a.externalIMAPAccountForUser(r.Context(), user.ID, chi.URLParam(r, "id"))
	if err != nil || !account.Enabled {
		respondError(w, http.StatusNotFound, "external account not found")
		return externalIMAPAccountRecord{}, false
	}
	return account, true
}

const externalIMAPAccountColumns = "id,user_id,mailbox_id,name,host,port,tls_mode,username,password_ciphertext,auth_mode,oauth_provider,oauth_email,oauth_access_token_ciphertext,oauth_refresh_token_ciphertext,oauth_expiry,storage_mode,sync_read_state,enabled,last_sync_at,last_status,last_error,created_at,updated_at,auth_status,provider_preset,oauth_subject,oauth_generation,smtp_host,smtp_port,smtp_tls_mode,smtp_username,smtp_password_ciphertext,send_enabled,sent_copy_mode"

type externalIMAPScanner interface {
	Scan(dest ...any) error
}

func scanExternalIMAPAccount(row externalIMAPScanner) (externalIMAPAccountRecord, error) {
	var item externalIMAPAccountRecord
	var syncRead, enabled, sendEnabled int
	var lastSync, oauthExpiry sql.NullString
	var created, updated string
	err := row.Scan(&item.ID, &item.UserID, &item.MailboxID, &item.Name, &item.Host, &item.Port, &item.TLSMode, &item.Username, &item.PasswordCiphertext, &item.AuthMode, &item.OAuthProvider, &item.OAuthEmail, &item.OAuthAccessTokenCiphertext, &item.OAuthRefreshTokenCiphertext, &oauthExpiry, &item.StorageMode, &syncRead, &enabled, &lastSync, &item.LastStatus, &item.LastError, &created, &updated, &item.AuthStatus, &item.ProviderPreset, &item.OAuthSubject, &item.OAuthGeneration, &item.SMTPHost, &item.SMTPPort, &item.SMTPTLSMode, &item.SMTPUsername, &item.SMTPPasswordCiphertext, &sendEnabled, &item.SentCopyMode)
	if err != nil {
		return item, err
	}
	if item.AuthMode == "" {
		item.AuthMode = externalIMAPAuthPassword
	}
	if oauthExpiry.Valid {
		item.OAuthExpiry = parseTime(oauthExpiry.String)
	}
	item.OAuthConfigured = item.AuthMode == externalIMAPAuthOAuth2 && item.OAuthAccessTokenCiphertext != ""
	item.SyncReadState = syncRead != 0
	item.Enabled = enabled != 0
	item.SendEnabled = sendEnabled != 0
	item.SMTPPasswordSet = item.SMTPPasswordCiphertext != ""
	item.SendAddress = externalSendAddress(item.ExternalIMAPAccount)
	item.CanSend = externalAccountCanSend(item.ExternalIMAPAccount)
	if lastSync.Valid && strings.TrimSpace(lastSync.String) != "" {
		t := parseTime(lastSync.String)
		item.LastSyncAt = &t
	}
	item.CreatedAt = parseTime(created)
	item.UpdatedAt = parseTime(updated)
	return item, nil
}

func (a *App) updateExternalIMAPStatus(ctx context.Context, accountID, status, errText string) {
	_, _ = a.db.ExecContext(ctx, `UPDATE external_imap_accounts SET last_status=?,last_error=?,updated_at=? WHERE id=?`, status, trimExternalIMAPError(errText), a.now().UTC().Format(time.RFC3339Nano), accountID)
}

func (a *App) migrateExternalIMAP(ctx context.Context) error {
	columns := []struct {
		table string
		name  string
		sql   string
	}{
		{"external_imap_sync_runs", "folder", `ALTER TABLE external_imap_sync_runs ADD COLUMN folder TEXT NOT NULL DEFAULT ''`},
		{"external_imap_accounts", "auth_mode", `ALTER TABLE external_imap_accounts ADD COLUMN auth_mode TEXT NOT NULL DEFAULT 'password'`},
		{"external_imap_accounts", "oauth_provider", `ALTER TABLE external_imap_accounts ADD COLUMN oauth_provider TEXT NOT NULL DEFAULT ''`},
		{"external_imap_accounts", "oauth_email", `ALTER TABLE external_imap_accounts ADD COLUMN oauth_email TEXT NOT NULL DEFAULT ''`},
		{"external_imap_accounts", "oauth_access_token_ciphertext", `ALTER TABLE external_imap_accounts ADD COLUMN oauth_access_token_ciphertext TEXT NOT NULL DEFAULT ''`},
		{"external_imap_accounts", "oauth_refresh_token_ciphertext", `ALTER TABLE external_imap_accounts ADD COLUMN oauth_refresh_token_ciphertext TEXT NOT NULL DEFAULT ''`},
		{"external_imap_accounts", "oauth_expiry", `ALTER TABLE external_imap_accounts ADD COLUMN oauth_expiry TEXT`},
	}
	columns = append(columns, []struct{ table, name, sql string }{
		{"external_imap_accounts", "provider_preset", `ALTER TABLE external_imap_accounts ADD COLUMN provider_preset TEXT NOT NULL DEFAULT 'custom'`},
		{"external_imap_accounts", "auth_status", `ALTER TABLE external_imap_accounts ADD COLUMN auth_status TEXT NOT NULL DEFAULT 'ok'`},
		{"external_imap_accounts", "oauth_subject", `ALTER TABLE external_imap_accounts ADD COLUMN oauth_subject TEXT NOT NULL DEFAULT ''`},
		{"external_imap_accounts", "oauth_generation", `ALTER TABLE external_imap_accounts ADD COLUMN oauth_generation INTEGER NOT NULL DEFAULT 0`},
		{"external_imap_accounts", "smtp_host", `ALTER TABLE external_imap_accounts ADD COLUMN smtp_host TEXT NOT NULL DEFAULT ''`},
		{"external_imap_accounts", "smtp_port", `ALTER TABLE external_imap_accounts ADD COLUMN smtp_port INTEGER NOT NULL DEFAULT 0`},
		{"external_imap_accounts", "smtp_tls_mode", `ALTER TABLE external_imap_accounts ADD COLUMN smtp_tls_mode TEXT NOT NULL DEFAULT ''`},
		{"external_imap_accounts", "smtp_username", `ALTER TABLE external_imap_accounts ADD COLUMN smtp_username TEXT NOT NULL DEFAULT ''`},
		{"external_imap_accounts", "smtp_password_ciphertext", `ALTER TABLE external_imap_accounts ADD COLUMN smtp_password_ciphertext TEXT NOT NULL DEFAULT ''`},
		{"external_imap_accounts", "send_enabled", `ALTER TABLE external_imap_accounts ADD COLUMN send_enabled INTEGER NOT NULL DEFAULT 0`},
		{"external_imap_accounts", "sent_copy_mode", `ALTER TABLE external_imap_accounts ADD COLUMN sent_copy_mode TEXT NOT NULL DEFAULT 'server'`},
		{"send_queue", "sent_copy_attempt_count", `ALTER TABLE send_queue ADD COLUMN sent_copy_attempt_count INTEGER NOT NULL DEFAULT 0`},
		{"send_queue", "subject", `ALTER TABLE send_queue ADD COLUMN subject TEXT NOT NULL DEFAULT ''`},
		{"send_queue", "route", `ALTER TABLE send_queue ADD COLUMN route TEXT NOT NULL DEFAULT 'relay'`},
		{"send_queue", "external_account_id", `ALTER TABLE send_queue ADD COLUMN external_account_id TEXT`},
		{"send_queue", "sent_copy_status", `ALTER TABLE send_queue ADD COLUMN sent_copy_status TEXT NOT NULL DEFAULT 'none'`},
	}...)
	for _, column := range columns {
		if err := a.ensureTableColumn(ctx, column.table, column.name, column.sql); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) syncExternalIMAPAccount(ctx context.Context, accountID string) (ExternalIMAPSyncRun, error) {
	account, err := a.externalIMAPAccountByID(ctx, accountID)
	if err != nil {
		return ExternalIMAPSyncRun{}, err
	}
	if account.AuthStatus == "needs_reauth" {
		return ExternalIMAPSyncRun{}, errExternalMailReauth
	}
	if account.StorageMode != externalIMAPStorageLocal {
		return ExternalIMAPSyncRun{}, errors.New("account is not configured for local storage")
	}
	run := ExternalIMAPSyncRun{ID: newID("ximrun"), AccountID: account.ID, Status: "running", StartedAt: a.now().UTC()}
	_, _ = a.db.ExecContext(ctx, `INSERT INTO external_imap_sync_runs(id,account_id,folder,status,started_at) VALUES(?,?,?,?,?)`, run.ID, run.AccountID, run.Folder, run.Status, run.StartedAt.Format(time.RFC3339Nano))
	client, err := a.externalIMAP.openExternalIMAPClient(ctx, account)
	if err != nil {
		return a.finishExternalIMAPRun(ctx, run, "failed", err)
	}
	defer client.Close()
	folders, err := client.ListFolders(ctx)
	if err != nil {
		return a.finishExternalIMAPRun(ctx, run, "failed", err)
	}
	for _, folder := range folders {
		imported, skipped, failed, err := a.syncExternalIMAPFolder(ctx, account, client, folder)
		run.Imported += imported
		run.Skipped += skipped
		run.Failed += failed
		if err != nil {
			run.Failed++
			a.log.Warn("external imap folder sync failed", "account", account.ID, "folder", folder.Name, "error", err)
		}
	}
	status := "ok"
	if run.Failed > 0 {
		status = "partial"
	}
	return a.finishExternalIMAPRun(ctx, run, status, nil)
}

func (a *App) syncExternalIMAPAccountFolder(ctx context.Context, accountID, folderName string) (ExternalIMAPSyncRun, error) {
	account, err := a.externalIMAPAccountByID(ctx, accountID)
	if err != nil {
		return ExternalIMAPSyncRun{}, err
	}
	if account.AuthStatus == "needs_reauth" {
		return ExternalIMAPSyncRun{}, errExternalMailReauth
	}
	if account.StorageMode != externalIMAPStorageLocal {
		return ExternalIMAPSyncRun{}, errors.New("account is not configured for local storage")
	}
	run := ExternalIMAPSyncRun{ID: newID("ximrun"), AccountID: account.ID, Folder: folderName, Status: "running", StartedAt: a.now().UTC()}
	_, _ = a.db.ExecContext(ctx, `INSERT INTO external_imap_sync_runs(id,account_id,folder,status,started_at) VALUES(?,?,?,?,?)`, run.ID, run.AccountID, run.Folder, run.Status, run.StartedAt.Format(time.RFC3339Nano))
	client, err := a.externalIMAP.openExternalIMAPClient(ctx, account)
	if err != nil {
		return a.finishExternalIMAPRun(ctx, run, "failed", err)
	}
	defer client.Close()
	folders, err := client.ListFolders(ctx)
	if err != nil {
		return a.finishExternalIMAPRun(ctx, run, "failed", err)
	}
	var selected externalIMAPRemoteFolder
	for _, folder := range folders {
		if strings.EqualFold(folder.Name, folderName) {
			selected = folder
			break
		}
	}
	if strings.TrimSpace(selected.Name) == "" {
		return a.finishExternalIMAPRun(ctx, run, "failed", errors.New("remote folder not found"))
	}
	imported, skipped, failed, err := a.syncExternalIMAPFolder(ctx, account, client, selected)
	run.Imported = imported
	run.Skipped = skipped
	run.Failed = failed
	status := "ok"
	if failed > 0 {
		status = "partial"
	}
	if err != nil {
		status = "failed"
	}
	return a.finishExternalIMAPRun(ctx, run, status, err)
}

func (a *App) externalIMAPAccountByID(ctx context.Context, id string) (externalIMAPAccountRecord, error) {
	row := a.db.QueryRowContext(ctx, `SELECT `+externalIMAPAccountColumns+` FROM external_imap_accounts WHERE id=? AND enabled=1`, id)
	return scanExternalIMAPAccount(row)
}

func (a *App) externalIMAPAccountByIDIncludingDisabled(ctx context.Context, id string) (externalIMAPAccountRecord, error) {
	row := a.db.QueryRowContext(ctx, `SELECT `+externalIMAPAccountColumns+` FROM external_imap_accounts WHERE id=?`, id)
	return scanExternalIMAPAccount(row)
}

func (a *App) syncExternalIMAPFolder(ctx context.Context, account externalIMAPAccountRecord, client externalIMAPClient, folder externalIMAPRemoteFolder) (int, int, int, error) {
	localFolderName := externalIMAPLocalFolderName(account.ID, folder.Name)
	localFolderID, err := a.ensureFolder(ctx, account.MailboxID, localFolderName)
	if err != nil {
		return 0, 0, 0, err
	}
	state := a.loadExternalIMAPFolderState(ctx, account.ID, folder.Name)
	if folder.UIDValidity != 0 && state.UIDValidity != 0 && folder.UIDValidity != state.UIDValidity {
		if err := a.resetExternalIMAPFolder(ctx, account.ID, folder.Name, state.UIDValidity, folder.UIDValidity); err != nil {
			return 0, 0, 0, err
		}
		state.LastUID = 0
	}
	if folder.UIDValidity == 0 {
		folder.UIDValidity = state.UIDValidity
	}
	if state.LocalFolderID != "" && state.LocalFolderID != localFolderID {
		if err := a.moveExternalIMAPMessagesToFolder(ctx, account.ID, account.MailboxID, folder.Name, localFolderID); err != nil {
			return 0, 0, 0, err
		}
	}
	imported, skipped, failed := 0, 0, 0
	lastUID := state.LastUID
	processed := 0
	now := a.now().UTC().Format(time.RFC3339Nano)
	for processed < externalIMAPSyncMaxPerRun {
		batchLimit := min(externalIMAPSyncBatchSize, externalIMAPSyncMaxPerRun-processed)
		remote, err := client.FetchNew(ctx, folder.Name, lastUID, batchLimit)
		if err != nil {
			if processed == 0 {
				return 0, 0, 0, err
			}
			failed++
			break
		}
		if len(remote) == 0 {
			break
		}
		stop := false
		for _, item := range remote {
			if folder.UIDValidity == 0 {
				folder.UIDValidity = item.UIDValidity
			}
			if a.externalIMAPRemoteMessageExists(ctx, account.ID, folder.Name, item.UIDValidity, item.UID) {
				skipped++
				lastUID = item.UID
				processed++
				continue
			}
			raw := item.Raw
			if len(raw) == 0 {
				var fetched externalIMAPRemoteMessage
				raw, fetched, err = client.FetchRaw(ctx, folder.Name, item.UID)
				if err == nil {
					item = fetched
				}
			}
			if err != nil {
				failed++
				stop = true
				break
			}
			stored, attachments, err := a.parseMaildirMessage(raw, account.Username)
			if err != nil {
				failed++
				stop = true
				break
			}
			stored.MailboxID = account.MailboxID
			stored.FolderID = localFolderID
			stored.RecipientAddr = account.Username
			stored.IsRead = item.IsRead
			msgID, err := a.insertExternalIMAPMessageOnce(ctx, account, folder.Name, item, stored, attachments)
			if err != nil {
				failed++
				stop = true
				break
			}
			if msgID != "" {
				if err := a.writeStoredMessageToMaildir(ctx, msgID, stored, attachments); err != nil {
					a.log.Warn("failed to write external imap message to maildir", "message", msgID, "error", err)
				}
				if strings.EqualFold(folder.Name, "INBOX") {
					a.applyInboundControls(ctx, msgID, account.MailboxID, stored.From, stored.Subject)
				}
				imported++
			} else {
				skipped++
			}
			lastUID = item.UID
			processed++
		}
		if stop || len(remote) < batchLimit {
			break
		}
	}
	query := upsertSQL(a.cfg.DBDriver, `INSERT INTO external_imap_folder_states(account_id,remote_folder,local_folder_id,uid_validity,last_uid,last_sync_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`, `(account_id,remote_folder)`,
		`local_folder_id=excluded.local_folder_id,last_uid=CASE WHEN external_imap_folder_states.uid_validity=excluded.uid_validity THEN `+scalarMaxSQL(a.cfg.DBDriver, "last_uid", "excluded.last_uid")+` ELSE excluded.last_uid END,uid_validity=excluded.uid_validity,last_sync_at=excluded.last_sync_at,updated_at=excluded.updated_at`,
		`local_folder_id=VALUES(local_folder_id),last_uid=CASE WHEN uid_validity=VALUES(uid_validity) THEN GREATEST(last_uid,VALUES(last_uid)) ELSE VALUES(last_uid) END,uid_validity=VALUES(uid_validity),last_sync_at=VALUES(last_sync_at),updated_at=VALUES(updated_at)`)
	_, err = a.db.ExecContext(ctx, query,
		account.ID, folder.Name, localFolderID, folder.UIDValidity, lastUID, now, now, now)
	if err != nil {
		return imported, skipped, failed, err
	}
	return imported, skipped, failed, ctx.Err()
}

type externalIMAPFolderState struct {
	LocalFolderID string
	LastUID       uint32
	UIDValidity   uint32
}

func (a *App) loadExternalIMAPFolderState(ctx context.Context, accountID, folder string) externalIMAPFolderState {
	var state externalIMAPFolderState
	_ = a.db.QueryRowContext(ctx, `SELECT local_folder_id,last_uid,uid_validity FROM external_imap_folder_states WHERE account_id=? AND remote_folder=?`, accountID, folder).Scan(&state.LocalFolderID, &state.LastUID, &state.UIDValidity)
	return state
}

func externalIMAPLocalFolderName(accountID, remoteFolder string) string {
	key := strings.TrimPrefix(accountID, "ximap_")
	if len(key) > 12 {
		key = key[:12]
	}
	hash := sha256.Sum256([]byte(remoteFolder))
	return fmt.Sprintf("External %s %x", key, hash[:6])
}

func (a *App) moveExternalIMAPMessagesToFolder(ctx context.Context, accountID, mailboxID, remoteFolder, targetFolderID string) error {
	rows, err := a.db.QueryContext(ctx, `SELECT m.id FROM messages m JOIN external_imap_messages em ON em.local_message_id=m.id
		WHERE em.account_id=? AND em.remote_folder=? AND m.mailbox_id=? AND m.folder_id<>?`, accountID, remoteFolder, mailboxID, targetFolderID)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := a.moveMessageMaildir(ctx, id, targetFolderID); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) externalIMAPRemoteMessageExists(ctx context.Context, accountID, folder string, uidValidity, uid uint32) bool {
	var exists int
	_ = a.db.QueryRowContext(ctx, `SELECT 1 FROM external_imap_messages WHERE account_id=? AND remote_folder=? AND uid_validity=? AND uid=?`, accountID, folder, uidValidity, uid).Scan(&exists)
	return exists == 1
}

func (a *App) insertExternalIMAPMessageOnce(ctx context.Context, account externalIMAPAccountRecord, folder string, item externalIMAPRemoteMessage, msg storedMessage, attachments []AttachmentInput) (string, error) {
	now := a.now().UTC().Format(time.RFC3339Nano)
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	query := insertIgnoreSQL(a.cfg.DBDriver, `INSERT INTO external_imap_messages(account_id,remote_folder,uid_validity,uid,message_id,is_read,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, `(account_id,remote_folder,uid_validity,uid)`)
	res, err := tx.ExecContext(ctx, query, account.ID, folder, item.UIDValidity, item.UID, msg.MessageID, boolInt(item.IsRead), now, now)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", tx.Commit()
	}
	id, err := a.insertMessageWithDB(ctx, tx, msg, attachments)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE external_imap_messages SET local_message_id=?,updated_at=? WHERE account_id=? AND remote_folder=? AND uid_validity=? AND uid=?`, id, now, account.ID, folder, item.UIDValidity, item.UID)
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (a *App) finishExternalIMAPRun(ctx context.Context, run ExternalIMAPSyncRun, status string, err error) (ExternalIMAPSyncRun, error) {
	run.Status = status
	if err != nil {
		run.Error = trimExternalIMAPError(err.Error())
	}
	finished := a.now().UTC()
	run.FinishedAt = &finished
	_, _ = a.db.ExecContext(ctx, `UPDATE external_imap_sync_runs SET status=?,imported=?,skipped=?,failed=?,error=?,finished_at=? WHERE id=?`,
		run.Status, run.Imported, run.Skipped, run.Failed, run.Error, finished.Format(time.RFC3339Nano), run.ID)
	lastStatus := status
	if status == "failed" {
		lastStatus = "error"
	}
	_, _ = a.db.ExecContext(ctx, `UPDATE external_imap_accounts SET last_sync_at=?,last_status=?,last_error=?,updated_at=? WHERE id=?`,
		finished.Format(time.RFC3339Nano), lastStatus, run.Error, finished.Format(time.RFC3339Nano), run.AccountID)
	if run.Imported > 0 {
		// The account names the mailbox the messages landed in, so only that mailbox's
		// audience needs waking rather than every open stream.
		var mailboxID string
		if scanErr := a.db.QueryRowContext(ctx, `SELECT mailbox_id FROM external_imap_accounts WHERE id=?`, run.AccountID).Scan(&mailboxID); scanErr != nil {
			a.log.Warn("failed to resolve external IMAP sync audience", "account", run.AccountID, "error", scanErr)
			a.publishSyncEventToAll()
		} else {
			a.publishSyncEventForMailboxes(ctx, []string{mailboxID})
		}
	}
	return run, err
}

func scanExternalIMAPSyncRun(row externalIMAPScanner) (ExternalIMAPSyncRun, error) {
	var run ExternalIMAPSyncRun
	var started, finished sql.NullString
	if err := row.Scan(&run.ID, &run.AccountID, &run.Folder, &run.Status, &run.Imported, &run.Skipped, &run.Failed, &run.Error, &started, &finished); err != nil {
		return run, err
	}
	if started.Valid && strings.TrimSpace(started.String) != "" {
		run.StartedAt = parseTime(started.String)
	}
	if finished.Valid && strings.TrimSpace(finished.String) != "" {
		t := parseTime(finished.String)
		run.FinishedAt = &t
	}
	return run, nil
}

func trimExternalIMAPError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 500 {
		return value[:500]
	}
	return value
}

func normalizeExternalIMAPFolderName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "inbox":
		return "Inbox"
	case "sent", "sent items", "sent messages":
		return "Sent"
	case "drafts", "draft":
		return "Drafts"
	case "archive", "archives":
		return "Archive"
	case "spam", "junk", "junk email":
		return "Spam"
	case "trash", "deleted", "deleted items":
		return "Trash"
	default:
		folder, err := normalizeCustomFolderName(name)
		if err != nil {
			return "Imported"
		}
		return folder
	}
}

func encodeExternalRemoteID(folder string, uid uint32) string {
	return base64.RawURLEncoding.EncodeToString([]byte(folder + "\x00" + strconv.FormatUint(uint64(uid), 10)))
}

func decodeExternalRemoteID(w http.ResponseWriter, raw string) (string, uint32, bool) {
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid remote id")
		return "", 0, false
	}
	parts := strings.SplitN(string(data), "\x00", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
		respondError(w, http.StatusBadRequest, "invalid remote id")
		return "", 0, false
	}
	uid, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil || uid == 0 {
		respondError(w, http.StatusBadRequest, "invalid remote id")
		return "", 0, false
	}
	return parts[0], uint32(uid), true
}

func externalRemoteMessageToMailMessage(account externalIMAPAccountRecord, msg externalIMAPRemoteMessage, includeBody bool) MailMessage {
	id := encodeExternalRemoteID(msg.Folder, msg.UID)
	messageID := msg.MessageID
	if messageID != "" && !strings.HasPrefix(messageID, "<") {
		messageID = "<" + messageID + ">"
	}
	return MailMessage{
		ID:                id,
		MailboxID:         account.MailboxID,
		MailboxAddress:    account.Name,
		FolderID:          msg.Folder,
		Folder:            msg.Folder,
		MessageUID:        id,
		IMAPUID:           int64(msg.UID),
		MessageID:         messageID,
		Subject:           decodeMIMEHeader(msg.Subject),
		From:              msg.From,
		FromName:          msg.FromName,
		To:                msg.To,
		CC:                msg.CC,
		SentAt:            msg.SentAt,
		ReceivedAt:        msg.ReceivedAt,
		Snippet:           msg.Snippet,
		IsRead:            msg.IsRead,
		SizeBytes:         msg.SizeBytes,
		HasAttachments:    includeBody,
		ExternalAccountID: account.ID,
	}
}

func safeExternalEMLFilename(subject string) string {
	name := strings.TrimSpace(subject)
	if name == "" {
		name = "message"
	}
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`\/:*?"<>|`, r) {
			return '-'
		}
		return r
	}, name)
	if len([]rune(name)) > 80 {
		name = string([]rune(name)[:80])
	}
	return name + ".eml"
}

func externalIMAPAttachmentsFromBodyStructure(body imap.BodyStructure) []Attachment {
	parts := externalIMAPAttachmentPartsFromBodyStructure(body)
	items := make([]Attachment, 0, len(parts))
	for _, part := range parts {
		items = append(items, part.Attachment)
	}
	return items
}

func externalIMAPAttachmentPartsFromBodyStructure(body imap.BodyStructure) []externalIMAPAttachmentPart {
	now := time.Now().UTC()
	items := []externalIMAPAttachmentPart{}
	body.Walk(func(path []int, part imap.BodyStructure) bool {
		single, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			return true
		}
		filename := strings.TrimSpace(single.Filename())
		disposition := ""
		if disp := single.Disposition(); disp != nil {
			disposition = strings.ToLower(strings.TrimSpace(disp.Value))
		}
		if filename == "" && disposition != "attachment" {
			return true
		}
		if filename == "" {
			filename = "attachment"
		}
		contentType := single.MediaType()
		if contentType == "/" || contentType == "" {
			contentType = "application/octet-stream"
		}
		items = append(items, externalIMAPAttachmentPart{
			Attachment: Attachment{
				ID:          encodeExternalIMAPPartID(path),
				Filename:    filename,
				ContentType: contentType,
				SizeBytes:   int64(single.Size),
				CreatedAt:   now,
			},
			Encoding: strings.ToLower(strings.TrimSpace(single.Encoding)),
		})
		return true
	})
	return items
}

func encodeExternalIMAPPartID(path []int) string {
	parts := make([]string, 0, len(path))
	for _, n := range path {
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, ".")
}

func decodeExternalIMAPPartID(value string) ([]int, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "raw" {
		return nil, errors.New("invalid part id")
	}
	rawParts := strings.Split(value, ".")
	path := make([]int, 0, len(rawParts))
	for _, part := range rawParts {
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 {
			return nil, errors.New("invalid part id")
		}
		path = append(path, n)
	}
	return path, nil
}

func decodeExternalIMAPPartData(data []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		compact := strings.NewReplacer("\r", "", "\n", "", " ", "", "\t", "").Replace(string(data))
		decoded := make([]byte, base64.StdEncoding.DecodedLen(len(compact)))
		n, err := base64.StdEncoding.Decode(decoded, []byte(compact))
		if err != nil {
			return nil, err
		}
		return decoded[:n], nil
	case "quoted-printable":
		return io.ReadAll(quotedprintable.NewReader(strings.NewReader(string(data))))
	default:
		return data, nil
	}
}

func (a *App) openExternalIMAPClient(ctx context.Context, account externalIMAPAccountRecord) (externalIMAPClient, error) {
	if account.AuthStatus == "needs_reauth" {
		return nil, errExternalMailReauth
	}
	if err := a.validateExternalIMAPHost(ctx, account.Host); err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(account.Host, strconv.Itoa(account.Port))
	options := &imapclient.Options{
		Dialer:    a.externalMailDialer(),
		TLSConfig: &tls.Config{ServerName: account.Host, MinVersion: tls.VersionTLS12},
	}
	var c *imapclient.Client
	var err error
	switch account.TLSMode {
	case externalIMAPTLS:
		c, err = imapclient.DialTLS(addr, options)
	case externalIMAPStartTLS:
		c, err = imapclient.DialStartTLS(addr, options)
	default:
		c, err = imapclient.DialInsecure(addr, options)
	}
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if account.AuthMode == externalIMAPAuthOAuth2 {
		token, err := a.externalIMAPOAuthAccessToken(ctx, account)
		if err != nil {
			c.Close()
			return nil, err
		}
		if err := c.Authenticate(newExternalMailXOAUTH2Client(account.Username, token)); err != nil {
			c.Close()
			return nil, fmt.Errorf("oauth xoauth2 authenticate failed: %w", err)
		}
	} else {
		password, err := a.decryptExternalIMAPPassword(account.PasswordCiphertext)
		if err != nil {
			c.Close()
			return nil, err
		}
		if err := c.Login(account.Username, password).Wait(); err != nil {
			c.Close()
			return nil, err
		}
	}
	a.sendExternalIMAPID(c, account.ExternalIMAPAccount)
	return &goExternalIMAPClient{client: c}, nil
}

type goExternalIMAPClient struct {
	client *imapclient.Client
}

func (c *goExternalIMAPClient) Close() error {
	if c.client == nil {
		return nil
	}
	_ = c.client.Logout().Wait()
	return c.client.Close()
}

func (c *goExternalIMAPClient) ListFolders(ctx context.Context) ([]externalIMAPRemoteFolder, error) {
	caps := c.client.Caps()
	var options *imap.ListOptions
	if caps.Has(imap.CapIMAP4rev2) || caps.Has(imap.CapListStatus) {
		options = &imap.ListOptions{ReturnStatus: &imap.StatusOptions{NumMessages: true, NumUnseen: true, UIDValidity: true}}
	}
	if caps.Has(imap.CapSpecialUse) {
		if options == nil {
			options = &imap.ListOptions{}
		}
		options.ReturnSpecialUse = true
	}

	list, err := c.client.List("", "*", options).Collect()
	if err != nil && options != nil && options.ReturnStatus != nil && options.ReturnSpecialUse {
		// Keep SPECIAL-USE even when a server rejects LIST RETURN (STATUS ...).
		options = &imap.ListOptions{ReturnSpecialUse: true}
		list, err = c.client.List("", "*", options).Collect()
	}

	if err != nil && options != nil {
		// Some Outlook/Exchange IMAP deployments advertise extended LIST
		// capabilities but reject LIST RETURN (STATUS ...) with
		// "BAD Command Argument Error. 12". Fall back to a plain LIST and
		// fetch counts with STATUS separately.
		options = nil
		list, err = c.client.List("", "*", nil).Collect()
	}
	if err != nil {
		return nil, err
	}
	folders := []externalIMAPRemoteFolder{}
	for _, item := range list {
		if strings.TrimSpace(item.Mailbox) == "" || mailboxHasNoSelect(item.Attrs) {
			continue
		}
		f := externalIMAPRemoteFolder{Name: item.Mailbox, Role: normalizeExternalIMAPFolderName(item.Mailbox)}
		for _, attr := range item.Attrs {
			if strings.EqualFold(string(attr), `\Sent`) {
				f.Role = "Sent"
				f.SentSpecialUse = true
			}
		}
		if item.Status != nil {
			f.UIDValidity = item.Status.UIDValidity
			if item.Status.NumMessages != nil {
				f.TotalCount = int(*item.Status.NumMessages)
			}
			if item.Status.NumUnseen != nil {
				f.UnreadCount = int(*item.Status.NumUnseen)
			}
		}
		if strings.EqualFold(item.Mailbox, "INBOX") {
			f.Name = "INBOX"
		}
		folders = append(folders, f)
	}
	if options == nil || options.ReturnStatus == nil {
		statusOptions := &imap.StatusOptions{NumMessages: true, NumUnseen: true, UIDValidity: true}
		for i := range folders {
			status, err := c.client.Status(folders[i].Name, statusOptions).Wait()
			if err != nil {
				continue
			}
			folders[i].UIDValidity = status.UIDValidity
			if status.NumMessages != nil {
				folders[i].TotalCount = int(*status.NumMessages)
			}
			if status.NumUnseen != nil {
				folders[i].UnreadCount = int(*status.NumUnseen)
			}
		}
	}
	if len(folders) == 0 {
		folders = append(folders, externalIMAPRemoteFolder{Name: "INBOX", Role: "Inbox"})
	}
	return folders, nil
}

func mailboxHasNoSelect(attrs []imap.MailboxAttr) bool {
	for _, attr := range attrs {
		if strings.EqualFold(string(attr), `\Noselect`) {
			return true
		}
	}
	return false
}

func (c *goExternalIMAPClient) FetchSummaries(ctx context.Context, folder string, query string, cursor string, limit int) ([]externalIMAPRemoteMessage, string, int, error) {
	selected, err := c.client.Select(folder, nil).Wait()
	if err != nil {
		return nil, "", 0, err
	}
	if limit != 20 && limit != 30 && limit != 50 {
		limit = externalIMAPMaxFetch
	}
	if selected.NumMessages == 0 {
		return nil, "", 0, nil
	}
	maxUID := uint32(0)
	if strings.TrimSpace(cursor) != "" {
		parsed, _ := strconv.ParseUint(strings.TrimPrefix(cursor, "uid:"), 10, 32)
		maxUID = uint32(parsed)
	}
	if maxUID == 0 {
		if selected.UIDNext == 0 {
			return nil, "", 0, nil
		}
		maxUID = uint32(selected.UIDNext - 1)
	}
	query = strings.TrimSpace(query)
	totalCount := int(selected.NumMessages)
	if query != "" {
		totalData, err := c.client.UIDSearch(&imap.SearchCriteria{Text: []string{query}}, c.uidSearchAllOptions()).Wait()
		if err != nil {
			return nil, "", 0, err
		}
		totalCount = staticUIDSetCount(totalData.All)
	}
	criteria := &imap.SearchCriteria{}
	var uidRange imap.UIDSet
	uidRange.AddRange(1, imap.UID(maxUID))
	criteria.UID = []imap.UIDSet{uidRange}
	if query != "" {
		criteria.Text = []string{query}
	}
	data, err := c.client.UIDSearch(criteria, c.uidSearchAllOptions()).Wait()
	if err != nil {
		return nil, "", 0, err
	}
	uids := boundedSearchUIDs(data.All, limit+1, true)
	if len(uids) == 0 {
		return nil, "", totalCount, nil
	}
	hasMore := len(uids) > limit
	if hasMore {
		uids = uids[:limit]
	}
	var set imap.UIDSet
	set.AddNum(uids...)
	bodySection := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
	messages, err := c.client.Fetch(set, &imap.FetchOptions{UID: true, Flags: true, Envelope: true, InternalDate: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{bodySection}}).Collect()
	if err != nil {
		return nil, "", 0, err
	}
	out := []externalIMAPRemoteMessage{}
	for _, message := range messages {
		out = append(out, fetchBufferToExternalMessage(folder, selected.UIDValidity, message, nil))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID > out[j].UID })
	next := ""
	if hasMore {
		last := uint32(uids[len(uids)-1])
		if last > 1 {
			next = "uid:" + strconv.FormatUint(uint64(last-1), 10)
		}
	}
	return out, next, totalCount, nil
}

func staticUIDSetCount(numSet imap.NumSet) int {
	uidSet, ok := numSet.(imap.UIDSet)
	if !ok || uidSet.Dynamic() {
		return 0
	}
	total := uint64(0)
	for _, uidRange := range uidSet {
		if uidRange.Start == 0 || uidRange.Stop < uidRange.Start {
			continue
		}
		total += uint64(uidRange.Stop-uidRange.Start) + 1
	}
	maxInt := uint64(^uint(0) >> 1)
	if total > maxInt {
		return int(maxInt)
	}
	return int(total)
}

func (c *goExternalIMAPClient) FetchNew(ctx context.Context, folder string, afterUID uint32, limit int) ([]externalIMAPRemoteMessage, error) {
	selected, err := c.client.Select(folder, nil).Wait()
	if err != nil {
		return nil, err
	}
	if afterUID == ^uint32(0) {
		return nil, nil
	}
	if selected.NumMessages == 0 || selected.UIDNext <= imap.UID(afterUID+1) {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var searchRange imap.UIDSet
	searchRange.AddRange(imap.UID(afterUID+1), selected.UIDNext-1)
	data, err := c.client.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{searchRange}}, c.uidSearchAllOptions()).Wait()
	if err != nil {
		return nil, err
	}
	uids := boundedSearchUIDs(data.All, limit, false)
	if len(uids) == 0 {
		return nil, nil
	}
	var set imap.UIDSet
	set.AddNum(uids...)
	bodySection := &imap.FetchItemBodySection{Peek: true}
	messages, err := c.client.Fetch(set, &imap.FetchOptions{UID: true, Flags: true, Envelope: true, InternalDate: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{bodySection}}).Collect()
	if err != nil {
		return nil, err
	}
	out := []externalIMAPRemoteMessage{}
	for _, message := range messages {
		raw := message.FindBodySection(bodySection)
		out = append(out, fetchBufferToExternalMessage(folder, selected.UIDValidity, message, raw))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

func (c *goExternalIMAPClient) uidSearchAllOptions() *imap.SearchOptions {
	caps := c.client.Caps()
	if caps.Has(imap.CapIMAP4rev2) || caps.Has(imap.CapESearch) {
		return &imap.SearchOptions{ReturnAll: true}
	}
	return nil
}

// boundedSearchUIDs reads only the requested edge of a possibly large,
// range-compressed SEARCH result. SEARCH results are always static UID sets.
func boundedSearchUIDs(numSet imap.NumSet, limit int, newestFirst bool) []imap.UID {
	uidSet, ok := numSet.(imap.UIDSet)
	if !ok || uidSet.Dynamic() || limit <= 0 {
		return nil
	}
	uids := make([]imap.UID, 0, min(limit, len(uidSet)))
	appendRange := func(start, stop imap.UID, descending bool) bool {
		if start == 0 || stop == 0 {
			return false
		}
		if descending {
			for uid := stop; ; uid-- {
				uids = append(uids, uid)
				if len(uids) == limit || uid == start {
					break
				}
			}
		} else {
			for uid := start; ; uid++ {
				uids = append(uids, uid)
				if len(uids) == limit || uid == stop {
					break
				}
			}
		}
		return len(uids) == limit
	}
	if newestFirst {
		for i := len(uidSet) - 1; i >= 0; i-- {
			if appendRange(uidSet[i].Start, uidSet[i].Stop, true) {
				break
			}
		}
	} else {
		for _, uidRange := range uidSet {
			if appendRange(uidRange.Start, uidRange.Stop, false) {
				break
			}
		}
	}
	return uids
}

func (c *goExternalIMAPClient) FetchRaw(ctx context.Context, folder string, uid uint32) ([]byte, externalIMAPRemoteMessage, error) {
	selected, err := c.client.Select(folder, nil).Wait()
	if err != nil {
		return nil, externalIMAPRemoteMessage{}, err
	}
	bodySection := &imap.FetchItemBodySection{Peek: true}
	messages, err := c.client.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{UID: true, Flags: true, Envelope: true, InternalDate: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{bodySection}}).Collect()
	if err != nil {
		return nil, externalIMAPRemoteMessage{}, err
	}
	if len(messages) == 0 {
		return nil, externalIMAPRemoteMessage{}, sql.ErrNoRows
	}
	raw := messages[0].FindBodySection(bodySection)
	return raw, fetchBufferToExternalMessage(folder, selected.UIDValidity, messages[0], raw), nil
}

func (c *goExternalIMAPClient) FetchAttachments(ctx context.Context, folder string, uid uint32) ([]Attachment, error) {
	parts, err := c.fetchAttachmentParts(ctx, folder, uid)
	if err != nil {
		return nil, err
	}
	out := make([]Attachment, 0, len(parts))
	for _, part := range parts {
		out = append(out, part.Attachment)
	}
	return out, nil
}

func (c *goExternalIMAPClient) fetchAttachmentParts(ctx context.Context, folder string, uid uint32) ([]externalIMAPAttachmentPart, error) {
	if _, err := c.client.Select(folder, nil).Wait(); err != nil {
		return nil, err
	}
	messages, err := c.client.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{
		UID:           true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}).Collect()
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 || messages[0].BodyStructure == nil {
		return nil, sql.ErrNoRows
	}
	return externalIMAPAttachmentPartsFromBodyStructure(messages[0].BodyStructure), nil
}

func (c *goExternalIMAPClient) FetchPart(ctx context.Context, folder string, uid uint32, partID string) ([]byte, Attachment, error) {
	path, err := decodeExternalIMAPPartID(partID)
	if err != nil {
		return nil, Attachment{}, err
	}
	if _, err := c.client.Select(folder, nil).Wait(); err != nil {
		return nil, Attachment{}, err
	}
	attachments, err := c.fetchAttachmentParts(ctx, folder, uid)
	if err != nil {
		return nil, Attachment{}, err
	}
	var att externalIMAPAttachmentPart
	for _, item := range attachments {
		if item.ID == partID {
			att = item
			break
		}
	}
	if att.ID == "" {
		return nil, Attachment{}, sql.ErrNoRows
	}
	bodySection := &imap.FetchItemBodySection{Part: path, Peek: true}
	messages, err := c.client.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{bodySection}}).Collect()
	if err != nil {
		return nil, Attachment{}, err
	}
	if len(messages) == 0 {
		return nil, Attachment{}, sql.ErrNoRows
	}
	data := messages[0].FindBodySection(bodySection)
	if data == nil {
		return nil, Attachment{}, sql.ErrNoRows
	}
	data, err = decodeExternalIMAPPartData(data, att.Encoding)
	if err != nil {
		return nil, Attachment{}, err
	}
	att.SizeBytes = int64(len(data))
	return data, att.Attachment, nil
}

func (c *goExternalIMAPClient) SetRead(ctx context.Context, folder string, uid uint32, read bool) error {
	if _, err := c.client.Select(folder, nil).Wait(); err != nil {
		return err
	}
	op := imap.StoreFlagsDel
	if read {
		op = imap.StoreFlagsAdd
	}
	return c.client.Store(imap.UIDSetNum(imap.UID(uid)), &imap.StoreFlags{Op: op, Flags: []imap.Flag{imap.FlagSeen}, Silent: true}, nil).Close()
}

func fetchBufferToExternalMessage(folder string, uidValidity uint32, msg *imapclient.FetchMessageBuffer, raw []byte) externalIMAPRemoteMessage {
	out := externalIMAPRemoteMessage{Folder: folder, UIDValidity: uidValidity, UID: uint32(msg.UID), ReceivedAt: time.Now().UTC(), Raw: raw}
	if msg.Envelope != nil {
		out.MessageID = msg.Envelope.MessageID
		out.Subject = msg.Envelope.Subject
		out.SentAt = msg.Envelope.Date
		out.From, out.FromName = firstIMAPAddress(msg.Envelope.From)
		out.To = imapAddresses(msg.Envelope.To)
		out.CC = imapAddresses(msg.Envelope.Cc)
	}
	if out.SentAt.IsZero() {
		out.SentAt = out.ReceivedAt
	}
	if !msg.InternalDate.IsZero() {
		out.ReceivedAt = msg.InternalDate
	}
	out.SizeBytes = msg.RFC822Size
	for _, flag := range msg.Flags {
		if flag == imap.FlagSeen {
			out.IsRead = true
			break
		}
	}
	if len(raw) > 0 {
		if stored, _, err := parseExternalIMAPRawForSnippet(raw); err == nil {
			if out.Subject == "" {
				out.Subject = stored.Subject
			}
			if out.MessageID == "" {
				out.MessageID = strings.Trim(stored.MessageID, "<>")
			}
			out.Snippet = stored.Snippet
		}
	}
	return out
}

func parseExternalIMAPRawForSnippet(raw []byte) (storedMessage, []AttachmentInput, error) {
	tmp := &App{now: time.Now, policy: NewHTMLPolicy()}
	return tmp.parseMaildirMessage(raw, "")
}

func firstIMAPAddress(addrs []imap.Address) (string, string) {
	for _, addr := range addrs {
		if email := addr.Addr(); email != "" {
			return normalizeEmail(email), addr.Name
		}
	}
	return "", ""
}

func imapAddresses(addrs []imap.Address) []string {
	out := []string{}
	for _, addr := range addrs {
		if email := addr.Addr(); email != "" {
			out = append(out, normalizeEmail(email))
		}
	}
	return out
}

// Keep the mappings until all local deletions have completed. On cancellation or
// failure the next sync retries cleanup rather than losing the only file links.
func (a *App) resetExternalIMAPFolder(ctx context.Context, accountID, folder string, oldValidity, newValidity uint32) error {
	rows, err := a.db.QueryContext(ctx, `SELECT DISTINCT local_message_id FROM external_imap_messages WHERE account_id=? AND remote_folder=? AND uid_validity=? AND local_message_id<>''`, accountID, folder, oldValidity)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.deleteMessage(ctx, id) // Removes Maildir and attachments before the DB row.
		var remaining int
		if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE id=?`, id).Scan(&remaining); err != nil {
			return err
		}
		if remaining != 0 {
			return errors.New("failed to remove stale external message")
		}
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM external_imap_messages WHERE account_id=? AND remote_folder=? AND uid_validity=?`, accountID, folder, oldValidity); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE external_imap_folder_states SET uid_validity=?,last_uid=0 WHERE account_id=? AND remote_folder=? AND uid_validity=?`, newValidity, accountID, folder, oldValidity); err != nil {
		return err
	}
	return tx.Commit()
}
