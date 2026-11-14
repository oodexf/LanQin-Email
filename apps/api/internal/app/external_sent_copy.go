package app

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

// APPEND has a separate durable counter and lease. It only reads delivered rows;
// a failure here can never make the SMTP worker pick the message up again.
func (a *App) processPendingExternalSentCopies(ctx context.Context) error {
	now := a.now().UTC().Format(time.RFC3339Nano)
	rows, err := a.db.QueryContext(ctx, `SELECT id FROM send_queue WHERE route='external' AND status='delivered' AND sent_copy_status='pending' AND next_attempt_at<=? AND (lease_until IS NULL OR lease_until<=?) LIMIT 20`, now, now)
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
		if err := a.processExternalSentCopy(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) processExternalSentCopy(ctx context.Context, id string) error {
	now := a.now().UTC()
	token := newID("append")
	res, err := a.db.ExecContext(ctx, `UPDATE send_queue SET sent_copy_attempt_count=sent_copy_attempt_count+1,lease_owner=?,lease_token=?,lease_until=? WHERE id=? AND status='delivered' AND route='external' AND sent_copy_status='pending' AND sent_copy_attempt_count<3 AND next_attempt_at<=? AND (lease_until IS NULL OR lease_until<=?)`, a.workerID, token, now.Add(sendQueueLeaseDuration).Format(time.RFC3339Nano), id, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// An interrupted third APPEND is never attempted a fourth time.
		_, err = a.db.ExecContext(ctx, `UPDATE send_queue SET sent_copy_status='failed',mime_base64='',lease_owner='',lease_token='',lease_until=NULL,updated_at=? WHERE id=? AND status='delivered' AND sent_copy_status='pending' AND sent_copy_attempt_count>=3 AND (lease_until IS NULL OR lease_until<=?)`, now.Format(time.RFC3339Nano), id, now.Format(time.RFC3339Nano))
		return err
	}
	var accountID, userID, raw string
	var attempts int
	if err := a.db.QueryRowContext(ctx, `SELECT COALESCE(external_account_id,''),user_id,mime_base64,sent_copy_attempt_count FROM send_queue WHERE id=? AND lease_token=?`, id, token).Scan(&accountID, &userID, &raw, &attempts); err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, smtpSessionTimeout)
	defer cancel()
	account, appendErr := a.externalIMAPAccountForUser(runCtx, userID, accountID)
	if appendErr == nil && (!account.Enabled || account.AuthStatus != "ok" || !a.cfg.ExternalIMAPEnabled) {
		appendErr = errors.New("external account unavailable")
	}
	if appendErr == nil {
		mimeBytes, decodeErr := base64.StdEncoding.DecodeString(raw)
		appendErr = decodeErr
		if appendErr == nil {
			appendErr = a.appendExternalSentCopy(runCtx, account, mimeBytes)
		}
	}
	status := "done"
	if appendErr != nil {
		status = "pending"
		if attempts >= 3 {
			status = "failed"
		}
	}
	_, err = a.db.ExecContext(ctx, `UPDATE send_queue SET mime_base64=CASE WHEN ?='pending' THEN mime_base64 ELSE '' END,sent_copy_status=?,next_attempt_at=?,updated_at=?,lease_owner='',lease_token='',lease_until=NULL WHERE id=? AND status='delivered' AND lease_token=?`, status, status, now.Add(sendRetryDelay(attempts)).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), id, token)
	return err
}

func (a *App) appendExternalSentCopy(ctx context.Context, account externalIMAPAccountRecord, raw []byte) error {
	client, err := a.externalIMAP.openExternalIMAPClient(ctx, account)
	if err != nil {
		return err
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() {
		if real, ok := client.(*goExternalIMAPClient); ok {
			_ = real.client.Close()
		} else {
			_ = client.Close()
		}
	})
	defer stop()
	folders, err := client.ListFolders(ctx)
	if err != nil {
		return err
	}
	folder := ""
	for _, candidate := range folders {
		if candidate.SentSpecialUse {
			folder = candidate.Name
			break
		}
	}
	if folder == "" {
		for _, candidate := range folders {
			if strings.EqualFold(candidate.Role, "Sent") {
				folder = candidate.Name
				break
			}
		}
	}

	if folder == "" {
		for _, candidate := range folders {
			switch strings.ToLower(strings.TrimSpace(candidate.Name)) {
			case "sent", "sent items", "sent messages", "已发送", "已发送邮件", "已寄件", "寄件備份", "已傳送郵件", "[gmail]/sent mail":
				folder = candidate.Name
			}
			if folder != "" {
				break
			}
		}
	}
	if folder == "" {
		return errors.New("sent folder not found")
	}
	return client.Append(ctx, folder, raw)
}
