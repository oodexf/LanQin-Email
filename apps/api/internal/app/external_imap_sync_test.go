package app

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

type externalIMAPSyncTestClient struct {
	items       []externalIMAPRemoteMessage
	fetchLimit  int
	fetchCalls  int
	rawFetches  int
	rawFetchErr error
}

func (c *externalIMAPSyncTestClient) Close() error { return nil }

func (c *externalIMAPSyncTestClient) ListFolders(context.Context) ([]externalIMAPRemoteFolder, error) {
	return nil, nil
}

func (c *externalIMAPSyncTestClient) FetchSummaries(context.Context, string, string, string, int) ([]externalIMAPRemoteMessage, string, int, error) {
	return nil, "", 0, nil
}

func (c *externalIMAPSyncTestClient) FetchNew(_ context.Context, _ string, afterUID uint32, limit int) ([]externalIMAPRemoteMessage, error) {
	c.fetchCalls++
	c.fetchLimit = limit
	items := make([]externalIMAPRemoteMessage, 0, len(c.items))
	for _, item := range c.items {
		if item.UID > afterUID {
			items = append(items, item)
		}
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (c *externalIMAPSyncTestClient) FetchRaw(context.Context, string, uint32) ([]byte, externalIMAPRemoteMessage, error) {
	c.rawFetches++
	return nil, externalIMAPRemoteMessage{}, c.rawFetchErr
}

func (c *externalIMAPSyncTestClient) FetchAttachments(context.Context, string, uint32) ([]Attachment, error) {
	return nil, nil
}

func (c *externalIMAPSyncTestClient) FetchPart(context.Context, string, uint32, string) ([]byte, Attachment, error) {
	return nil, Attachment{}, nil
}

func (c *externalIMAPSyncTestClient) SetRead(context.Context, string, uint32, bool) error {
	return nil
}

func TestExternalIMAPSyncFolderUsesBatchBodiesAndRetriesFailedUID(t *testing.T) {
	a := newTestApp(t)
	_, mailbox := defaultAdminUserAndMailbox(t, a)
	accountID := "ximap_sync_batch_test"
	now := a.now().UTC().Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`INSERT INTO external_imap_accounts(id,user_id,mailbox_id,name,host,port,tls_mode,username,password_ciphertext,storage_mode,sync_read_state,enabled,last_status,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, accountID, mailbox.UserID, mailbox.ID, "IMAP batch test", "imap.example.test", 993, externalIMAPTLS, "remote@example.test", "ciphertext", externalIMAPStorageLocal, 1, 1, "idle", now, now); err != nil {
		t.Fatal(err)
	}

	client := &externalIMAPSyncTestClient{
		items: []externalIMAPRemoteMessage{
			{
				UID: 1, UIDValidity: 7, Folder: "INBOX", IsRead: true,
				Raw: []byte("From: sender@example.test\r\nTo: remote@example.test\r\nSubject: synthetic import\r\nMessage-ID: <batch-one@example.test>\r\n\r\nbody"),
			},
			{UID: 2, UIDValidity: 7, Folder: "INBOX", IsRead: false},
		},
		rawFetchErr: errors.New("synthetic fetch failure"),
	}
	account := externalIMAPAccountRecord{ExternalIMAPAccount: ExternalIMAPAccount{
		ID: accountID, UserID: mailbox.UserID, MailboxID: mailbox.ID, Username: "remote@example.test", StorageMode: externalIMAPStorageLocal,
	}}

	imported, skipped, failed, err := a.syncExternalIMAPFolder(t.Context(), account, client, externalIMAPRemoteFolder{Name: "INBOX", UIDValidity: 7})
	if err != nil {
		t.Fatalf("sync folder: %v", err)
	}
	if imported != 1 || skipped != 0 || failed != 1 {
		t.Fatalf("sync counts = imported %d, skipped %d, failed %d", imported, skipped, failed)
	}
	if client.fetchLimit != externalIMAPSyncBatchSize || client.rawFetches != 1 {
		t.Fatalf("fetch limit=%d raw fetches=%d, want batch limit %d and one fallback", client.fetchLimit, client.rawFetches, externalIMAPSyncBatchSize)
	}
	var lastUID uint32
	if err := a.db.QueryRow(`SELECT last_uid FROM external_imap_folder_states WHERE account_id=? AND remote_folder=?`, accountID, "INBOX").Scan(&lastUID); err != nil {
		t.Fatal(err)
	}
	if lastUID != 1 {
		t.Fatalf("last UID checkpoint=%d, want successful UID 1", lastUID)
	}
}

func TestExternalIMAPUIDValidityResetCleansLocalMessagesAndReimports(t *testing.T) {
	f := newExternalMailOAuthFixture(t)
	account := f.connect(t)
	f.a.cfg.MaildirRoot = t.TempDir()
	raw := []byte("From: sender@example.test\r\nTo: person@example.test\r\nSubject: synthetic old attachment\r\nMessage-ID: <old-validity@example.test>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=synthetic\r\n\r\n--synthetic\r\nContent-Type: text/plain\r\n\r\nbody\r\n--synthetic\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=synthetic.txt\r\n\r\nsynthetic attachment\r\n--synthetic--\r\n")
	client := &externalIMAPSyncTestClient{items: []externalIMAPRemoteMessage{{UID: 100, UIDValidity: 7, Folder: "INBOX", Raw: raw}}}
	if n, _, failed, err := f.a.syncExternalIMAPFolder(t.Context(), account, client, externalIMAPRemoteFolder{Name: "INBOX", UIDValidity: 7}); err != nil || n != 1 || failed != 0 {
		t.Fatalf("initial import = %d %d %v", n, failed, err)
	}
	var oldID, rawPath, attachmentPath string
	if err := f.a.db.QueryRow(`SELECT m.id,m.raw_path,a.storage_path FROM messages m JOIN attachments a ON a.message_id=m.id JOIN external_imap_messages em ON em.local_message_id=m.id WHERE em.account_id=?`, account.ID).Scan(&oldID, &rawPath, &attachmentPath); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{rawPath, attachmentPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("imported file missing: %v", err)
		}
	}
	// An unrelated folder mapping must survive the reset.
	now := f.a.now().UTC().Format(time.RFC3339Nano)
	if _, err := f.a.db.Exec(`INSERT INTO external_imap_messages(account_id,remote_folder,uid_validity,uid,created_at,updated_at) VALUES(?,?,?,?,?,?)`, account.ID, "Archive", 7, 100, now, now); err != nil {
		t.Fatal(err)
	}
	client.items = []externalIMAPRemoteMessage{{UID: 1, UIDValidity: 8, Folder: "INBOX", Raw: []byte("From: sender@example.test\r\nTo: person@example.test\r\nSubject: synthetic new generation\r\nMessage-ID: <new-validity@example.test>\r\n\r\nnew body")}}
	if n, skipped, failed, err := f.a.syncExternalIMAPFolder(t.Context(), account, client, externalIMAPRemoteFolder{Name: "INBOX", UIDValidity: 8}); err != nil || n != 1 || skipped != 0 || failed != 0 {
		t.Fatalf("reset import = %d %d %d %v", n, skipped, failed, err)
	}
	for _, query := range []string{`SELECT COUNT(*) FROM messages WHERE id=?`, `SELECT COUNT(*) FROM attachments WHERE message_id=?`} {
		var count int
		if err := f.a.db.QueryRow(query, oldID).Scan(&count); err != nil || count != 0 {
			t.Fatal("old local message/attachment row survived", err)
		}
	}
	for _, path := range []string{rawPath, attachmentPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("old Maildir/attachment file survived: %v", err)
		}
	}
	if f.a.externalIMAPRemoteMessageExists(t.Context(), account.ID, "INBOX", 7, 100) || !f.a.externalIMAPRemoteMessageExists(t.Context(), account.ID, "INBOX", 8, 1) || !f.a.externalIMAPRemoteMessageExists(t.Context(), account.ID, "Archive", 7, 100) {
		t.Fatal("incorrect reset mapping scope")
	}
	state := f.a.loadExternalIMAPFolderState(t.Context(), account.ID, "INBOX")
	if state.UIDValidity != 8 || state.LastUID != 1 {
		t.Fatalf("checkpoint = %+v", state)
	}
	if n, _, _, err := f.a.syncExternalIMAPFolder(t.Context(), account, client, externalIMAPRemoteFolder{Name: "INBOX", UIDValidity: 8}); err != nil || n != 0 {
		t.Fatal("same validity reimported mail", err)
	}
}

func (c *externalIMAPSyncTestClient) Append(context.Context, string, []byte) error { return nil }
