package app

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

type ExternalSend struct {
	ID             string   `json:"id"`
	Status         string   `json:"status"`
	Subject        string   `json:"subject"`
	Recipients     []string `json:"recipients"`
	CreatedAt      string   `json:"createdAt"`
	UpdatedAt      string   `json:"updatedAt"`
	LastError      string   `json:"lastError"`
	SentCopyStatus string   `json:"sentCopyStatus"`
}

func (a *App) handleExternalSends(w http.ResponseWriter, r *http.Request) {
	account, err := a.externalIMAPAccountForUser(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, 404, "external account not found")
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			badRequest(w, errors.New("invalid limit"))
			return
		}
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,status,subject,recipients_json,created_at,updated_at,last_error,sent_copy_status FROM send_queue WHERE route='external' AND external_account_id=? AND user_id=? ORDER BY created_at DESC,id DESC LIMIT ?`, account.ID, currentUser(r).ID, limit)
	if err != nil {
		respondError(w, 500, "failed to load external sends")
		return
	}
	defer rows.Close()
	items := []ExternalSend{}
	for rows.Next() {
		var item ExternalSend
		var recipients string
		if err := rows.Scan(&item.ID, &item.Status, &item.Subject, &recipients, &item.CreatedAt, &item.UpdatedAt, &item.LastError, &item.SentCopyStatus); err != nil {
			respondError(w, 500, "failed to load external sends")
			return
		}
		item.Recipients = jsonDecodeSlice(recipients)
		if item.Recipients == nil {
			item.Recipients = []string{}
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		respondError(w, 500, "failed to load external sends")
		return
	}
	respondJSON(w, 200, map[string]any{"items": items})
}

func (a *App) handleResolveExternalSend(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	account, err := a.externalIMAPAccountForUser(r.Context(), user.ID, chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, 404, "external account not found")
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if req.Action != "retry" && req.Action != "delivered" {
		badRequest(w, errors.New("invalid resolve action"))
		return
	}
	id := chi.URLParam(r, "sendId")
	var status string
	if err := a.db.QueryRowContext(r.Context(), `SELECT status FROM send_queue WHERE id=? AND route='external' AND external_account_id=? AND user_id=?`, id, account.ID, user.ID).Scan(&status); err != nil {
		respondError(w, 404, "external send not found")
		return
	}
	if status != sendQueueStatusUnconfirmed {
		respondError(w, 409, "external send is not unconfirmed")
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	status = sendQueueStatusQueued
	if req.Action == "delivered" {
		status = sendQueueStatusDelivered
	}
	copyStatus := "none"
	if req.Action == "delivered" && account.SentCopyMode == "append" {
		copyStatus = "pending"
	}
	var deliveredAt any
	if req.Action == "delivered" {
		deliveredAt = now
	}
	res, err := a.db.ExecContext(r.Context(), `UPDATE send_queue SET mime_base64=CASE WHEN ?='delivered' AND ?<>'pending' THEN '' ELSE mime_base64 END,status=?,sent_copy_status=?,attempt_count=0,next_attempt_at=?,last_error='',updated_at=?,delivered_at=?,lease_owner='',lease_token='',lease_until=NULL WHERE id=? AND route='external' AND external_account_id=? AND user_id=? AND status='unconfirmed'`, status, copyStatus, status, copyStatus, now, now, deliveredAt, id, account.ID, user.ID)
	if err != nil {
		respondError(w, 500, "failed to resolve external send")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(w, 409, "external send is not unconfirmed")
		return
	}
	// Delete any leftover acceptance marker before an explicit user-requested retry.
	a.deleteSendQueueDeliveredMarker(id)
	a.recordSendAudit(r.Context(), map[bool]string{true: sendAuditDelivered, false: sendAuditRetry}[req.Action == "delivered"], status, sendAuditInput{QueueID: id, UserID: user.ID, MailboxID: account.MailboxID, Source: sendSourceWebmail})
	respondJSON(w, 200, map[string]any{"ok": true})
}
