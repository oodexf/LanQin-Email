package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"
	"github.com/go-chi/chi/v5"
)

type externalSMTPTestServer struct {
	authErr, mailErr, dataErr error
	disconnect                bool
	authMechanism             string
	deliveries                atomic.Int32
	mu                        sync.Mutex
	raw                       []byte
	from, username, password  string
}

func (s *externalSMTPTestServer) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &externalSMTPTestSession{server: s, conn: c}, nil
}

type externalSMTPTestSession struct {
	server *externalSMTPTestServer
	conn   *smtp.Conn
}

func (s *externalSMTPTestSession) AuthMechanisms() []string {
	if s.server.authMechanism != "" {
		return []string{s.server.authMechanism}
	}
	return []string{sasl.Plain}
}
func (s *externalSMTPTestSession) Auth(mech string) (sasl.Server, error) {
	check := func(username, password string) error {
		s.server.mu.Lock()
		s.server.username, s.server.password = username, password
		s.server.mu.Unlock()
		return s.server.authErr
	}
	if mech == "XOAUTH2" {
		return externalSMTPTestXOAUTHServer{check: check}, nil
	}
	if mech == sasl.Login {
		return &externalSMTPTestLoginServer{check: check}, nil
	}
	return sasl.NewPlainServer(func(_, username, password string) error { return check(username, password) }), nil
}
func (s *externalSMTPTestSession) Mail(from string, _ *smtp.MailOptions) error {
	s.server.mu.Lock()
	s.server.from = from
	err := s.server.mailErr
	s.server.mu.Unlock()
	return err
}
func (s *externalSMTPTestSession) Rcpt(string, *smtp.RcptOptions) error { return nil }
func (s *externalSMTPTestSession) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.server.mu.Lock()
	s.server.raw = raw
	s.server.mu.Unlock()
	s.server.deliveries.Add(1)
	if s.server.disconnect {
		_ = s.conn.Close()
	}
	return s.server.dataErr
}
func (*externalSMTPTestSession) Reset()        {}
func (*externalSMTPTestSession) Logout() error { return nil }

func startExternalSMTPTestServer(t *testing.T, a *App, backend *externalSMTPTestServer, mode string) (string, int) {
	t.Helper()
	certServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cert := certServer.TLS.Certificates[0]
	template := *certServer.Certificate()
	template.DNSNames = append(template.DNSNames, "smtp.gmail.com", "smtp.office365.com")
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, template.PublicKey, cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	cert.Certificate = [][]byte{der}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	certServer.Close()
	a.externalSMTPRootCAsForTest = roots
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	n, _ := strconv.Atoi(port)
	server := smtp.NewServer(backend)
	server.Domain = "synthetic.example.test"
	server.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	server.ErrorLog = log.New(io.Discard, "", 0)
	if mode == "tls" {
		listener = tls.NewListener(listener, server.TLSConfig)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return host, n
}

type externalSMTPFixture struct {
	a       *App
	user    *User
	mb      *Mailbox
	router  http.Handler
	account ExternalIMAPAccount
}

func newExternalSMTPFixture(t *testing.T) *externalSMTPFixture {
	return newExternalSMTPFixtureForApp(t, newTestApp(t))
}
func newExternalSMTPFixtureForApp(t *testing.T, a *App) *externalSMTPFixture {
	t.Helper()
	stopTestWorkers(a)
	previous, roots := a.cfg, a.externalSMTPRootCAsForTest
	t.Cleanup(func() { a.cfg = previous; a.externalSMTPRootCAsForTest = roots })
	a.cfg.ExternalIMAPEnabled = true
	a.cfg.ExternalIMAPSecretKey = "synthetic-smtp-key"
	a.cfg.ExternalIMAPAllowPrivateHosts = true
	a.cfg.SMTPHost = ""
	user, _, err := a.userByEmail(t.Context(), a.cfg.AdminEmail)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := a.mailboxByAddress(t.Context(), a.cfg.AdminEmail)
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Post("/accounts", a.handleCreateExternalIMAPAccount)
	router.Post("/accounts/{id}", a.handleUpdateExternalIMAPAccount)
	router.Delete("/accounts/{id}", a.handleDeleteExternalIMAPAccount)
	router.Get("/accounts/{id}/sends", a.handleExternalSends)
	router.Post("/accounts/{id}/sends/{sendId}/resolve", a.handleResolveExternalSend)
	router.Post("/send", a.handleMailSend)
	router.Post("/schedule", a.handleScheduleSend)
	router.Get("/admin", a.handleAdminDeliveryQueue)
	router.Post("/admin/{queueType}/{id}/retry", a.handleAdminDeliveryQueueRetry)
	router.Post("/admin/{queueType}/{id}/cancel", a.handleAdminDeliveryQueueCancel)
	return &externalSMTPFixture{a: a, user: user, mb: mb, router: router}
}
func (f *externalSMTPFixture) request(t *testing.T, method, path string, body any, user *User) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if user == nil {
		user = f.user
	}
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, user))
	res := httptest.NewRecorder()
	f.router.ServeHTTP(res, req)
	return res
}
func (f *externalSMTPFixture) create(t *testing.T, host string, port int, mode, copyMode string) ExternalIMAPAccount {
	t.Helper()
	response := f.request(t, "POST", "/accounts", map[string]any{"mailboxId": f.mb.ID, "name": "Synthetic sender", "host": host, "port": 993, "tlsMode": "tls", "username": "imap@example.test", "password": "synthetic-imap-password", "sendEnabled": true, "smtpHost": host, "smtpPort": port, "smtpTlsMode": mode, "smtpUsername": "sender@example.test", "sentCopyMode": copyMode}, nil)
	if response.Code != 201 {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &f.account); err != nil {
		t.Fatal(err)
	}
	if !f.account.CanSend || f.account.SendAddress != "sender@example.test" || f.account.SMTPPasswordSet {
		t.Fatalf("account: %+v", f.account)
	}
	return f.account
}
func (f *externalSMTPFixture) enqueue(t *testing.T) string {
	t.Helper()
	response := f.request(t, "POST", "/send", map[string]any{"mailboxId": f.mb.ID, "externalAccountId": f.account.ID, "from": "forged@other.test", "fromName": "Synthetic person", "to": []string{"recipient@example.test"}, "subject": "合成发件", "text": "synthetic body"}, nil)
	if response.Code != 202 {
		t.Fatalf("send: %d %s", response.Code, response.Body.String())
	}
	var result struct {
		Queued            bool   `json:"queued"`
		QueueID           string `json:"queueId"`
		ExternalAccountID string `json:"externalAccountId"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Queued || result.QueueID == "" || result.ExternalAccountID != f.account.ID {
		t.Fatalf("queue response: %+v", result)
	}
	return result.QueueID
}
func (f *externalSMTPFixture) state(t *testing.T, id string) (status, code, copyStatus string, attempt, max int) {
	t.Helper()
	err := f.a.db.QueryRow(`SELECT status,last_error,sent_copy_status,attempt_count,max_attempts FROM send_queue WHERE id=?`, id).Scan(&status, &code, &copyStatus, &attempt, &max)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func TestExternalSMTPQueueDelivery(t *testing.T) {
	for _, mode := range []string{"tls", "starttls"} {
		for _, mechanism := range []string{sasl.Plain, sasl.Login} {
			t.Run(mode+"/"+mechanism, func(t *testing.T) {
				f := newExternalSMTPFixture(t)
				backend := &externalSMTPTestServer{authMechanism: mechanism}
				host, port := startExternalSMTPTestServer(t, f.a, backend, mode)
				f.create(t, host, port, mode, "server")
				expectedPassword := "synthetic-imap-password"
				if mechanism == sasl.Login {
					expectedPassword = "synthetic-smtp-password"
					response := f.request(t, "POST", "/accounts/"+f.account.ID, map[string]any{"smtpPassword": expectedPassword}, nil)
					if response.Code != 200 {
						t.Fatalf("save smtp credentials %d", response.Code)
					}
				}
				id := f.enqueue(t)
				if err := f.a.processDueSendQueue(t.Context()); err != nil {
					t.Fatal(err)
				}
				status, code, copyStatus, _, _ := f.state(t, id)
				if status != "delivered" || code != "" || copyStatus != "none" || backend.deliveries.Load() != 1 {
					t.Fatalf("%s %s %s deliveries=%d", status, code, copyStatus, backend.deliveries.Load())
				}
				assertExternalQueueMIME(t, f, id, false)
				backend.mu.Lock()
				raw, from, password := string(backend.raw), backend.from, backend.password
				backend.mu.Unlock()
				if from != "sender@example.test" || !strings.Contains(raw, "<sender@example.test>") || strings.Contains(raw, "forged@") || !strings.Contains(raw, "@example.test>") || password != expectedPassword {
					t.Fatal("forced sender or fallback credentials incorrect")
				}
				header, _, err := readMessageHeader([]byte(raw))
				if err != nil || !strings.HasSuffix(header.Get("Message-Id"), "@example.test>") {
					t.Fatalf("external Message-ID domain: %q %v", header.Get("Message-Id"), err)
				}
				var count int
				if err := f.a.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE mailbox_id=? AND subject=?`, f.mb.ID, "合成发件").Scan(&count); err != nil || count != 0 {
					t.Fatalf("external route stored local messages: %d %v", count, err)
				}
				response := f.request(t, "GET", "/accounts/"+f.account.ID+"/sends", nil, nil)
				var list struct {
					Items []ExternalSend `json:"items"`
				}
				_ = json.Unmarshal(response.Body.Bytes(), &list)
				if response.Code != 200 || len(list.Items) != 1 || list.Items[0].Subject != "合成发件" || len(list.Items[0].Recipients) != 1 {
					t.Fatalf("records: %d %s", response.Code, response.Body.String())
				}
			})
		}
	}
}

func TestExternalSMTPErrorOutcomes(t *testing.T) {
	cases := []struct {
		name                      string
		authErr, mailErr, dataErr error
		disconnect                bool
		status, code              string
		permanent                 bool
	}{
		{name: "auth disabled", authErr: &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 139}, Message: "SmtpClientAuthentication is disabled"}, status: "failed", code: "external_smtp_auth_disabled", permanent: true},
		{name: "auth 573", authErr: &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 3}, Message: "synthetic refusal"}, status: "failed", code: "external_smtp_auth_failed", permanent: true},
		{name: "auth failure", authErr: &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: "synthetic credential refusal"}, status: "failed", code: "external_smtp_auth_failed", permanent: true},
		{name: "mail 5xx", mailErr: &smtp.SMTPError{Code: 550, Message: "synthetic private response"}, status: "failed", code: "external_smtp_rejected", permanent: true},
		{name: "mail 4xx", mailErr: &smtp.SMTPError{Code: 451, Message: "synthetic private response"}, status: "failed", code: "external_smtp_temporary"},
		{name: "data 5xx", dataErr: &smtp.SMTPError{Code: 554, Message: "synthetic private response"}, status: "failed", code: "external_smtp_rejected", permanent: true},
		{name: "data 4xx", dataErr: &smtp.SMTPError{Code: 451, Message: "synthetic private response"}, status: "failed", code: "external_smtp_temporary"},
		{name: "data disconnected", disconnect: true, status: "unconfirmed", code: "external_smtp_unconfirmed"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := newExternalSMTPFixture(t)
			backend := &externalSMTPTestServer{authErr: tt.authErr, mailErr: tt.mailErr, dataErr: tt.dataErr, disconnect: tt.disconnect}
			host, port := startExternalSMTPTestServer(t, f.a, backend, "tls")
			f.create(t, host, port, "tls", "server")
			id := f.enqueue(t)
			if err := f.a.processDueSendQueue(t.Context()); err != nil {
				t.Fatal(err)
			}
			status, code, _, attempt, max := f.state(t, id)
			if status != tt.status || code != tt.code || (attempt >= max) != tt.permanent {
				t.Fatalf("state=%s %s attempts=%d/%d", status, code, attempt, max)
			}
			assertExternalQueueMIME(t, f, id, !tt.permanent)
			// Due time alone must never retry unconfirmed or permanent failures.
			if tt.permanent || tt.disconnect {
				before := backend.deliveries.Load()
				_, _ = f.a.db.Exec(`UPDATE send_queue SET next_attempt_at=? WHERE id=?`, f.a.now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), id)
				if err := f.a.processDueSendQueue(t.Context()); err != nil {
					t.Fatal(err)
				}
				if backend.deliveries.Load() != before {
					t.Fatal("unexpected automatic retry")
				}
			}
		})
	}
}

func TestExternalSMTPAuthorization(t *testing.T) {
	f := newExternalSMTPFixture(t)
	f.create(t, "127.0.0.1", 465, "tls", "server")
	cases := []struct {
		name, sql string
		other     bool
	}{
		{name: "other account", other: true}, {name: "disabled", sql: `UPDATE external_imap_accounts SET enabled=0 WHERE id=?`}, {name: "sending disabled", sql: `UPDATE external_imap_accounts SET send_enabled=0 WHERE id=?`}, {name: "needs reauth", sql: `UPDATE external_imap_accounts SET auth_status='needs_reauth' WHERE id=?`}, {name: "mailbox mismatch", sql: `UPDATE external_imap_accounts SET mailbox_id=(SELECT id FROM mailboxes WHERE id<>? LIMIT 1) WHERE id=?`},
	}
	// The mismatch case uses a real second owned mailbox to retain all FK checks.
	_, err := f.a.createMailbox(t.Context(), f.user.ID, f.mb.DomainID, "mismatch", "Synthetic mismatch", "synthetic-password", 0, "active", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.a.db.Exec(`UPDATE external_imap_accounts SET enabled=1,send_enabled=1,auth_status='ok',mailbox_id=? WHERE id=?`, f.mb.ID, f.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tt.sql != "" {
				if tt.name == "mailbox mismatch" {
					_, err = f.a.db.Exec(tt.sql, f.mb.ID, f.account.ID)
				} else {
					_, err = f.a.db.Exec(tt.sql, f.account.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			user := f.user
			if tt.other {
				copy := *f.user
				copy.ID = "synthetic-other-user"
				user = &copy
			}
			response := f.request(t, "POST", "/send", map[string]any{"mailboxId": f.mb.ID, "externalAccountId": f.account.ID, "to": []string{"recipient@example.test"}}, user)
			if response.Code != 403 || !strings.Contains(response.Body.String(), "external account cannot send") {
				t.Fatalf("send: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

type externalSentCopyTestClient struct {
	externalIMAPSyncTestClient
	folders   []externalIMAPRemoteFolder
	appendErr error
	appends   int
	folder    string
	raw       []byte
}

func (c *externalSentCopyTestClient) ListFolders(context.Context) ([]externalIMAPRemoteFolder, error) {
	return c.folders, nil
}
func (c *externalSentCopyTestClient) Append(_ context.Context, folder string, raw []byte) error {
	c.appends++
	c.folder = folder
	c.raw = append([]byte(nil), raw...)
	return c.appendErr
}

type externalSentCopyTestFactory struct{ client *externalSentCopyTestClient }

func (f externalSentCopyTestFactory) openExternalIMAPClient(context.Context, externalIMAPAccountRecord) (externalIMAPClient, error) {
	return f.client, nil
}

func TestExternalSentCopyRetriesNeverResendSMTP(t *testing.T) {
	for _, test := range []struct {
		name, mode string
		fail       bool
		want       string
		appends    int
	}{
		{name: "append done", mode: "append", want: "done", appends: 1},
		{name: "append fails three times", mode: "append", fail: true, want: "failed", appends: 3},
		{name: "server copy", mode: "server", want: "none"},
		{name: "no copy", mode: "none", want: "none"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newExternalSMTPFixture(t)
			backend := &externalSMTPTestServer{}
			host, port := startExternalSMTPTestServer(t, f.a, backend, "tls")
			f.create(t, host, port, "tls", test.mode)
			client := &externalSentCopyTestClient{folders: []externalIMAPRemoteFolder{{Name: "Sent"}, {Name: "special-sent", Role: "Sent", SentSpecialUse: true}}}
			if test.fail {
				client.appendErr = errors.New("synthetic failure")
			}
			f.a.externalIMAP = externalSentCopyTestFactory{client}
			id := f.enqueue(t)
			for i := 0; i < 4; i++ {
				_, _ = f.a.db.Exec(`UPDATE send_queue SET next_attempt_at=? WHERE id=?`, f.a.now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), id)
				if err := f.a.processDueSendQueue(t.Context()); err != nil {
					t.Fatal(err)
				}
				if test.fail && i < 2 {
					assertExternalQueueMIME(t, f, id, true)
				}
			}
			status, _, copyStatus, _, _ := f.state(t, id)
			if status != "delivered" || copyStatus != test.want || client.appends != test.appends || backend.deliveries.Load() != 1 {
				t.Fatalf("%s %s appends=%d smtp=%d", status, copyStatus, client.appends, backend.deliveries.Load())
			}
			assertExternalQueueMIME(t, f, id, false)
			if client.appends > 0 && (client.folder != "special-sent" || !bytes.Equal(client.raw, backend.raw)) {
				t.Fatal("special-use folder or raw MIME not preserved")
			}
		})
	}
}

func TestExternalSMTPSettingsAndDeletion(t *testing.T) {
	f := newExternalSMTPFixture(t)
	f.create(t, "127.0.0.1", 465, "tls", "server")
	id := f.enqueue(t)
	for _, tt := range []struct {
		name string
		body map[string]any
		want string
	}{
		{name: "plain", body: map[string]any{"smtpTlsMode": "plain"}, want: "external smtp requires tls"},
		{name: "missing endpoint", body: map[string]any{"smtpHost": ""}, want: "external smtp settings are incomplete"},
		{name: "invalid address", body: map[string]any{"smtpUsername": "non-email", "username": "non-email"}, want: "external send address is invalid"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := f.request(t, "POST", "/accounts/"+f.account.ID, tt.body, nil)
			if res.Code != 400 || !strings.Contains(res.Body.String(), tt.want) {
				t.Fatalf("%d %s", res.Code, res.Body.String())
			}
		})
	}
	f.a.cfg.ExternalIMAPAllowPrivateHosts = false
	res := f.request(t, "POST", "/accounts/"+f.account.ID, map[string]any{"host": "8.8.8.8", "smtpHost": "127.0.0.1"}, nil)
	if res.Code != 400 || !strings.Contains(res.Body.String(), "private or local") {
		t.Fatalf("private host: %d %s", res.Code, res.Body.String())
	}
	f.a.cfg.ExternalIMAPAllowPrivateHosts = true
	res = f.request(t, "POST", "/accounts/"+f.account.ID, map[string]any{"smtpPassword": "synthetic-smtp-password"}, nil)
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"smtpPasswordSet":true`) || strings.Contains(res.Body.String(), "ciphertext") || strings.Contains(res.Body.String(), "synthetic-smtp-password") {
		t.Fatalf("password response: %d", res.Code)
	}
	res = f.request(t, "POST", "/accounts/"+f.account.ID, map[string]any{"smtpPassword": ""}, nil)
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"smtpPasswordSet":true`) {
		t.Fatal("empty password did not retain secret")
	}
	// Deleting both queued and retryable failed rows is atomic with account deletion.
	second := f.enqueue(t)
	_, _ = f.a.db.Exec(`UPDATE send_queue SET status='failed',attempt_count=1 WHERE id=?`, second)
	res = f.request(t, "DELETE", "/accounts/"+f.account.ID, nil, nil)
	if res.Code != 200 {
		t.Fatalf("delete: %d %s", res.Code, res.Body.String())
	}
	for _, queueID := range []string{id, second} {
		status, code, _, attempt, max := f.state(t, queueID)
		if status != "failed" || code != "external account removed" || attempt != max {
			t.Fatalf("removed state: %s %s %d/%d", status, code, attempt, max)
		}
	}
	// Cleared permanent failures cannot be retried, even if a relay is configured.
	f.a.cfg.SMTPHost = "127.0.0.1"
	f.a.cfg.SMTPPort = "1"
	res = f.request(t, "POST", "/admin/send/"+id+"/retry", nil, nil)
	if res.Code != 409 {
		t.Fatalf("admin retry %d %s", res.Code, res.Body.String())
	}
	if err := f.a.processDueSendQueue(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, code, _, attempt, max := f.state(t, id)
	if status != "failed" || code != "external account removed" || attempt != max {
		t.Fatalf("missing account state: %s %s %d/%d", status, code, attempt, max)
	}
}

func TestExternalOAuthSMTPEndpointsFixed(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f := newExternalMailOAuthFixture(t)
			account := connectExternalSMTPProvider(t, f, provider)
			host, port, mode := externalOAuthSMTPEndpoint(provider)
			if account.SMTPHost != host || account.SMTPPort != port || account.SMTPTLSMode != mode || account.SentCopyMode != "server" {
				t.Fatalf("callback defaults: %+v", account.ExternalIMAPAccount)
			}
			router := chi.NewRouter()
			router.Post("/accounts/{id}", f.a.handleUpdateExternalIMAPAccount)
			f.router = router
			for _, body := range []map[string]any{{"smtpHost": "attacker.example.test"}, {"smtpPort": 2525}, {"smtpTlsMode": "plain"}, {"smtpUsername": "attacker@example.test"}, {"smtpPassword": "synthetic-other"}} {
				res := f.request(t, "POST", "/accounts/"+account.ID, body, nil, nil)
				if res.Code != 400 || !strings.Contains(res.Body.String(), "oauth account endpoints cannot be changed") {
					t.Fatalf("%d %s", res.Code, res.Body.String())
				}
			}
		})
	}
}

func connectExternalSMTPProvider(t *testing.T, f *externalMailOAuthFixture, provider string) externalIMAPAccountRecord {
	t.Helper()
	res := f.request(t, "POST", "/start/"+provider, map[string]any{"mailboxId": f.mailbox.ID}, nil, nil)
	if res.Code != 200 {
		t.Fatalf("start %d %s", res.Code, res.Body.String())
	}
	var result struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &result)
	link, err := url.Parse(result.URL)
	if err != nil {
		t.Fatal(err)
	}
	f.nonce, f.challenge = link.Query().Get("nonce"), link.Query().Get("code_challenge")
	res = f.request(t, "GET", "/callback/"+provider+"?state="+url.QueryEscape(link.Query().Get("state"))+"&code=synthetic-code", nil, res.Result().Cookies()[0], nil)
	if res.Code != 302 || !strings.HasSuffix(res.Header().Get("Location"), "externalMail=connected") {
		t.Fatalf("callback %d %s", res.Code, res.Header().Get("Location"))
	}
	var id string
	if err := f.a.db.QueryRow(`SELECT id FROM external_imap_accounts WHERE user_id=? AND oauth_provider=?`, f.user.ID, provider).Scan(&id); err != nil {
		t.Fatal(err)
	}
	account, err := f.a.externalIMAPAccountForUser(t.Context(), f.user.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func TestExternalScheduledSendRechecksSender(t *testing.T) {
	f := newExternalSMTPFixture(t)
	f.create(t, "127.0.0.1", 465, "tls", "server")
	res := f.request(t, "POST", "/schedule", map[string]any{"mailboxId": f.mb.ID, "externalAccountId": f.account.ID, "to": []string{"recipient@example.test"}, "sendAt": f.a.now().Add(time.Hour).Format(time.RFC3339Nano)}, nil)
	if res.Code != 201 {
		t.Fatalf("schedule: %d %s", res.Code, res.Body.String())
	}
	var item ScheduledSend
	_ = json.Unmarshal(res.Body.Bytes(), &item)
	var payload string
	if err := f.a.db.QueryRow(`SELECT payload_json FROM scheduled_sends WHERE id=?`, item.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var stored scheduledSendPayload
	_ = json.Unmarshal([]byte(payload), &stored)
	if stored.ExternalAccountID != f.account.ID || item.ExternalAccountID != f.account.ID {
		t.Fatal("schedule lost external account")
	}
	_, _ = f.a.db.Exec(`UPDATE external_imap_accounts SET enabled=0 WHERE id=?`, f.account.ID)
	f.a.processScheduledSend(t.Context(), item.ID, f.mb.ID, "", payload)
	var status, message string
	if err := f.a.db.QueryRow(`SELECT status,error FROM scheduled_sends WHERE id=?`, item.ID).Scan(&status, &message); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || message != "external account cannot send" {
		t.Fatalf("%s %s", status, message)
	}
}

func TestExternalSendResolveOwnershipAndStatus(t *testing.T) {
	f := newExternalSMTPFixture(t)
	f.create(t, "127.0.0.1", 465, "tls", "append")
	for _, action := range []string{"delivered", "retry"} {
		t.Run(action, func(t *testing.T) {
			id := f.enqueue(t)
			_, _ = f.a.db.Exec(`UPDATE send_queue SET status='unconfirmed',last_error='external_smtp_unconfirmed' WHERE id=?`, id)
			other := *f.user
			other.ID = "synthetic-other-user"
			if res := f.request(t, "GET", "/accounts/"+f.account.ID+"/sends", nil, &other); res.Code != 404 {
				t.Fatalf("other list %d", res.Code)
			}
			path := "/accounts/" + f.account.ID + "/sends/" + id + "/resolve"
			if res := f.request(t, "POST", path, map[string]any{"action": action}, &other); res.Code != 404 {
				t.Fatalf("other resolve %d", res.Code)
			}
			if res := f.request(t, "POST", "/admin/send/"+id+"/retry", nil, nil); res.Code != 409 {
				t.Fatalf("admin retry ambiguous %d", res.Code)
			}
			admin := f.request(t, "GET", "/admin?queueType=send&status=unconfirmed", nil, nil)
			if admin.Code != 200 || !strings.Contains(admin.Body.String(), `"route":"external"`) || !strings.Contains(admin.Body.String(), `"status":"unconfirmed"`) {
				t.Fatalf("admin %d %s", admin.Code, admin.Body.String())
			}
			assertExternalQueueMIME(t, f, id, true)
			if res := f.request(t, "POST", path, map[string]any{"action": action}, nil); res.Code != 200 {
				t.Fatalf("resolve %d %s", res.Code, res.Body.String())
			}
			assertExternalQueueMIME(t, f, id, true) // retry and pending APPEND both need MIME
			want := "queued"
			if action == "delivered" {
				want = "delivered"
			}
			status, code, _, attempt, _ := f.state(t, id)
			if status != want || code != "" || attempt != 0 {
				t.Fatalf("resolved: %s %s %d", status, code, attempt)
			}
			if res := f.request(t, "POST", path, map[string]any{"action": action}, nil); res.Code != 409 {
				t.Fatalf("non ambiguous %d", res.Code)
			}
		})
	}
}

func TestExternalStaleLeaseDoesNotResend(t *testing.T) {
	f := newExternalSMTPFixture(t)
	f.create(t, "127.0.0.1", 465, "tls", "server")
	id := f.enqueue(t)
	old := f.a.now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	_, _ = f.a.db.Exec(`UPDATE send_queue SET status='sending',attempt_count=max_attempts,lease_token='synthetic',lease_until=?,updated_at=? WHERE id=?`, old, old, id)
	if err := f.a.processDueSendQueue(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, code, _, _, _ := f.state(t, id)
	if status != "unconfirmed" || code != "external_smtp_unconfirmed" {
		t.Fatalf("%s %s", status, code)
	}
}

func TestExternalSMTPQuitFailureIsDelivered(t *testing.T) {
	// A small protocol peer is needed to break QUIT specifically; all other SMTP
	// outcomes above use go-smtp's real server and real TLS verification.
	f := newExternalSMTPFixture(t)
	certServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cert := certServer.TLS.Certificates[0]
	roots := x509.NewCertPool()
	roots.AddCert(certServer.Certificate())
	certServer.Close()
	f.a.externalSMTPRootCAsForTest = roots
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		done <- externalSMTPQuitPeer(conn)
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	n, _ := strconv.Atoi(port)
	f.create(t, host, n, "tls", "server")
	id := f.enqueue(t)
	if err := f.a.processDueSendQueue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	status, code, _, _, _ := f.state(t, id)
	if status != "delivered" || code != "" {
		t.Fatalf("%s %s", status, code)
	}
}

type externalSMTPTestLoginServer struct {
	step     int
	username string
	check    func(string, string) error
}

func (s *externalSMTPTestLoginServer) Next(response []byte) ([]byte, bool, error) {
	if s.step == 0 {
		if response == nil {
			s.step = 1
			return []byte("Username:"), false, nil
		}
		s.username = string(response)
		s.step = 2
		return []byte("Password:"), false, nil
	}
	if s.step == 1 {
		s.username = string(response)
		s.step = 2
		return []byte("Password:"), false, nil
	}
	return nil, true, s.check(s.username, string(response))
}

func externalSMTPQuitPeer(conn net.Conn) error {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	peer := textproto.NewConn(conn)
	if err := peer.PrintfLine("220 synthetic SMTP"); err != nil {
		return err
	}
	for {
		line, err := peer.ReadLine()
		if err != nil {
			return err
		}
		switch {
		case strings.HasPrefix(line, "EHLO"):
			if _, err = fmt.Fprint(conn, "250-synthetic\r\n250 AUTH PLAIN\r\n"); err != nil {
				return err
			}
		case strings.HasPrefix(line, "AUTH"):
			if err := peer.PrintfLine("235 authenticated"); err != nil {
				return err
			}
		case strings.HasPrefix(line, "MAIL"), strings.HasPrefix(line, "RCPT"):
			if err := peer.PrintfLine("250 OK"); err != nil {
				return err
			}

		case line == "DATA":
			if err := peer.PrintfLine("354 send"); err != nil {
				return err
			}
			if _, err := io.Copy(io.Discard, peer.DotReader()); err != nil {
				return err
			}
			if err := peer.PrintfLine("250 accepted"); err != nil {
				return err
			}
			return nil // close immediately after definite acceptance, before QUIT
		default:
			return fmt.Errorf("unexpected synthetic command")
		}
	}
}

type externalSMTPTestXOAUTHServer struct{ check func(string, string) error }

func (s externalSMTPTestXOAUTHServer) Next(response []byte) ([]byte, bool, error) {
	parts := strings.Split(string(response), "\x01")
	if len(parts) < 2 {
		return nil, true, errors.New("synthetic invalid xoauth2")
	}
	return nil, true, s.check(strings.TrimPrefix(parts[0], "user="), strings.TrimPrefix(parts[1], "auth=Bearer "))
}

func TestExternalSMTPXOAUTHAndInvalidGrant(t *testing.T) {
	for _, invalidGrant := range []bool{false, true} {
		t.Run(strconv.FormatBool(invalidGrant), func(t *testing.T) {
			f := newExternalMailOAuthFixture(t)
			account := f.connect(t)
			backend := &externalSMTPTestServer{authMechanism: "XOAUTH2"}
			host, port := startExternalSMTPTestServer(t, f.a, backend, "tls")
			f.a.externalSMTPDialForTest = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(host, strconv.Itoa(port)))
			}
			_, err := f.a.db.Exec(`UPDATE external_imap_accounts SET send_enabled=1 WHERE id=?`, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if invalidGrant {
				_, err = f.a.db.Exec(`UPDATE external_imap_accounts SET oauth_expiry=? WHERE id=?`, f.a.now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), account.ID)
				if err != nil {
					t.Fatal(err)
				}
				f.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(400)
					_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"synthetic private response"}`))
				}
			}
			result, err := f.a.sendMailNow(t.Context(), f.user, f.mailbox, mailComposeInput{ExternalAccountID: account.ID, To: []string{"recipient@example.test"}, Text: "synthetic OAuth body"})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.a.processDueSendQueue(t.Context()); err != nil {
				t.Fatal(err)
			}
			check := &externalSMTPFixture{a: f.a}
			status, code, _, attempt, max := check.state(t, result.ID)
			if invalidGrant {
				if status != "failed" || code != "external_reauthorization_required" || attempt != max || backend.deliveries.Load() != 0 {
					t.Fatalf("invalid grant %s %s %d/%d", status, code, attempt, max)
				}
			} else {
				if status != "delivered" || backend.deliveries.Load() != 1 {
					t.Fatalf("OAuth send %s %s", status, code)
				}
				backend.mu.Lock()
				username, token := backend.username, backend.password
				backend.mu.Unlock()
				if username != account.OAuthEmail || token != f.access {
					t.Fatal("XOAUTH2 identity or token incorrect")
				}
			}
		})
	}
}

func TestExternalSMTPLiveRoutesSharedAccessAndRate(t *testing.T) {
	f := newExternalSMTPFixture(t)
	server := httptest.NewServer(f.a.Router())
	defer server.Close()
	admin := &testClient{t: t, server: server}
	if status := admin.do("POST", "/api/auth/login", map[string]string{"email": f.a.cfg.AdminEmail, "password": f.a.cfg.AdminPassword}, nil); status != 200 {
		t.Fatal("login", status)
	}
	domainID := mustDefaultDomainID(t, f.a)
	ownerMB := createTestMailbox(t, admin, domainID, "smtp-owner", "Synthetic owner", "Password123!", nil)
	viewerMB := createTestMailbox(t, admin, domainID, "smtp-viewer", "Synthetic viewer", "Password123!", nil)
	owner := &testClient{t: t, server: server}
	viewer := &testClient{t: t, server: server}
	for _, pair := range []struct {
		client *testClient
		email  string
	}{{owner, ownerMB.Address}, {viewer, viewerMB.Address}} {
		if status := pair.client.do("POST", "/api/auth/login", map[string]string{"email": pair.email, "password": "Password123!"}, nil); status != 200 {
			t.Fatal("user login", status)
		}
	}
	now := f.a.now().UTC().Format(time.RFC3339Nano)
	if _, err := f.a.db.Exec(`INSERT INTO mailbox_shares(id,mailbox_id,owner_user_id,shared_with_user_id,scope,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, newID("share"), ownerMB.ID, ownerMB.UserID, viewerMB.UserID, "all", now, now); err != nil {
		t.Fatal(err)
	}
	var account ExternalIMAPAccount
	settings := map[string]any{"mailboxId": ownerMB.ID, "host": "127.0.0.1", "username": "sender@example.test", "password": "synthetic-password", "sendEnabled": true, "smtpHost": "127.0.0.1", "smtpPort": 465, "smtpTlsMode": "tls"}
	if status := owner.do("POST", "/api/me/external-imap-accounts", settings, &account); status != 201 {
		t.Fatal("create account", status)
	}
	// A viewer with a live share still cannot select the owner's external account,
	// either from that shared mailbox or from their own mailbox.
	for _, mailboxID := range []string{ownerMB.ID, viewerMB.ID} {
		payload := map[string]any{"mailboxId": mailboxID, "externalAccountId": account.ID, "to": []string{"recipient@example.test"}, "text": "synthetic"}
		if status := viewer.do("POST", "/api/mail/send", payload, nil); status != 403 {
			t.Fatal("shared/cross-account send", status)
		}
	}
	if status := viewer.do("GET", "/api/me/external-imap-accounts/"+account.ID+"/sends", nil, nil); status != 404 {
		t.Fatal("other records", status)
	}
	unauth := &testClient{t: t, server: server}
	if status := unauth.do("GET", "/api/me/external-imap-accounts/"+account.ID+"/sends", nil, nil); status != 401 {
		t.Fatal("unauthenticated records", status)
	}
	payload := map[string]any{"mailboxId": ownerMB.ID, "externalAccountId": account.ID, "to": []string{"recipient@example.test"}, "text": "synthetic"}
	if status := owner.do("POST", "/api/mail/send", payload, nil); status != 202 {
		t.Fatal("external send", status)
	}
	var events, recipients int
	if err := f.a.db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(recipients),0) FROM smtp_send_events WHERE user_id=?`, ownerMB.UserID).Scan(&events, &recipients); err != nil || events != 1 || recipients != 1 {
		t.Fatalf("SMTP quota events=%d recipients=%d err=%v", events, recipients, err)
	}
	f.a.cfg.ExternalIMAPEnabled = false
	if status := owner.do("GET", "/api/me/external-imap-accounts/"+account.ID+"/sends", nil, nil); status != 403 {
		t.Fatal("disabled external route", status)
	}
	if status := owner.do("POST", "/api/mail/send", payload, nil); status != 403 {
		t.Fatal("disabled external send", status)
	}
}

func TestExternalSMTPRecipientAndAttachmentLimits(t *testing.T) {
	f := newExternalSMTPFixture(t)
	f.create(t, "127.0.0.1", 465, "tls", "server")
	recipients := make([]string, maxRecipientsPerMessage+1)
	for i := range recipients {
		recipients[i] = fmt.Sprintf("synthetic-%d@example.test", i)
	}
	res := f.request(t, "POST", "/send", map[string]any{"externalAccountId": f.account.ID, "mailboxId": f.mb.ID, "to": recipients}, nil)
	if res.Code != 400 || !strings.Contains(res.Body.String(), "too many recipients") {
		t.Fatalf("recipient cap %d %s", res.Code, res.Body.String())
	}
	attachments := make([]AttachmentInput, maxAttachmentsPerMessage+1)
	res = f.request(t, "POST", "/send", map[string]any{"externalAccountId": f.account.ID, "mailboxId": f.mb.ID, "to": []string{"recipient@example.test"}, "attachments": attachments}, nil)
	if res.Code != 400 || !strings.Contains(res.Body.String(), "too many attachments") {
		t.Fatalf("attachment cap %d %s", res.Code, res.Body.String())
	}
}

func TestExternalSMTPUntrustedTLSDoesNotAuthenticate(t *testing.T) {
	f := newExternalSMTPFixture(t)
	backend := &externalSMTPTestServer{}
	host, port := startExternalSMTPTestServer(t, f.a, backend, "tls")
	f.create(t, host, port, "tls", "server")
	f.a.externalSMTPRootCAsForTest = x509.NewCertPool() // no trust for the synthetic CA
	id := f.enqueue(t)
	if err := f.a.processDueSendQueue(t.Context()); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	username := backend.username
	backend.mu.Unlock()
	status, code, _, attempt, max := f.state(t, id)
	if username != "" || backend.deliveries.Load() != 0 || status != "failed" || code != "external_smtp_temporary" || attempt >= max {
		t.Fatalf("TLS failure leaked auth or failed classification: %s %s", status, code)
	}
}

func TestExternalSMTPNetworkFailureIsRetryable(t *testing.T) {
	f := newExternalSMTPFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	n, _ := strconv.Atoi(port)
	_ = listener.Close()
	f.create(t, host, n, "tls", "server")
	id := f.enqueue(t)
	if err := f.a.processDueSendQueue(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, code, _, attempt, max := f.state(t, id)
	if status != "failed" || code != "external_smtp_temporary" || attempt != 1 || attempt >= max {
		t.Fatalf("%s %s %d/%d", status, code, attempt, max)
	}
}

func TestExternalSentCopyFolderFallbackAndMissing(t *testing.T) {
	for _, tt := range []struct {
		name    string
		folders []externalIMAPRemoteFolder
		want    string
	}{
		{name: "Chinese sent", folders: []externalIMAPRemoteFolder{{Name: "已发送"}}, want: "已发送"},
		{name: "missing sent", folders: []externalIMAPRemoteFolder{{Name: "Inbox"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newExternalSMTPFixture(t)
			client := &externalSentCopyTestClient{folders: tt.folders}
			f.a.externalIMAP = externalSentCopyTestFactory{client}
			err := f.a.appendExternalSentCopy(t.Context(), externalIMAPAccountRecord{}, []byte("Subject: synthetic\r\n\r\nbody"))
			if tt.want == "" {
				if err == nil || client.appends != 0 {
					t.Fatal("missing sent folder accepted")
				}
			} else if err != nil || client.folder != tt.want || client.appends != 1 {
				t.Fatalf("fallback %q %v", client.folder, err)
			}
		})
	}
}

func TestExternalSentCopyInterruptedThirdAttemptStops(t *testing.T) {
	f := newExternalSMTPFixture(t)
	f.create(t, "127.0.0.1", 465, "tls", "append")
	id := f.enqueue(t)
	old := f.a.now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	_, err := f.a.db.Exec(`UPDATE send_queue SET status='delivered',sent_copy_status='pending',sent_copy_attempt_count=3,lease_token='interrupted',lease_until=?,next_attempt_at=? WHERE id=?`, old, old, id)
	if err != nil {
		t.Fatal(err)
	}
	client := &externalSentCopyTestClient{folders: []externalIMAPRemoteFolder{{Name: "Sent"}}}
	f.a.externalIMAP = externalSentCopyTestFactory{client}
	if err := f.a.processPendingExternalSentCopies(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, _, copyStatus, _, _ := f.state(t, id)
	if status != "delivered" || copyStatus != "failed" || client.appends != 0 {
		t.Fatalf("third append retried %s %s", status, copyStatus)
	}
}

func TestExternalUnconfirmedBackupStateIsPreserved(t *testing.T) {
	f := newExternalSMTPFixture(t)
	f.create(t, "127.0.0.1", 465, "tls", "server")
	id := f.enqueue(t)
	_, err := f.a.db.Exec(`UPDATE send_queue SET status='unconfirmed' WHERE id=?`, id)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.a.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	counts, err := pauseNativeWork(t.Context(), tx, "send_queue", []string{"queued", "sending", "failed", "pending"}, []string{"delivered", "canceled", "unconfirmed"}, "canceled", 100)
	if err != nil || len(counts) != 0 {
		t.Fatalf("backup unconfirmed state %v %v", counts, err)
	}
	var status string
	if err := tx.QueryRow(`SELECT status FROM send_queue WHERE id=?`, id).Scan(&status); err != nil || status != "unconfirmed" {
		t.Fatalf("backup changed ambiguity %s %v", status, err)
	}
}

func TestExternalSMTPTemporaryFailureRetriesExternalRoute(t *testing.T) {
	f := newExternalSMTPFixture(t)
	backend := &externalSMTPTestServer{mailErr: &smtp.SMTPError{Code: 451, Message: "synthetic temporary refusal"}}
	host, port := startExternalSMTPTestServer(t, f.a, backend, "tls")
	f.create(t, host, port, "tls", "server")
	id := f.enqueue(t)
	if err := f.a.processDueSendQueue(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, code, _, attempt, max := f.state(t, id)
	if status != "failed" || code != "external_smtp_temporary" || attempt != 1 || attempt >= max {
		t.Fatalf("first attempt %s %s", status, code)
	}
	backend.mu.Lock()
	backend.mailErr = nil
	backend.mu.Unlock()
	if _, err := f.a.db.Exec(`UPDATE send_queue SET next_attempt_at=? WHERE id=?`, f.a.now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), id); err != nil {
		t.Fatal(err)
	}
	if err := f.a.processDueSendQueue(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, code, _, attempt, _ = f.state(t, id)
	if status != "delivered" || code != "" || attempt != 2 || backend.deliveries.Load() != 1 {
		t.Fatalf("external retry %s %s attempts=%d smtp=%d", status, code, attempt, backend.deliveries.Load())
	}
}

func assertExternalQueueMIME(t *testing.T, f *externalSMTPFixture, id string, retained bool) {
	t.Helper()
	var raw string
	if err := f.a.db.QueryRow(`SELECT mime_base64 FROM send_queue WHERE id=?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if (raw != "") != retained {
		t.Fatalf("MIME retention = %v, want %v", raw != "", retained)
	}
}

func TestExternalSMTPAuthClassification(t *testing.T) {
	for _, tt := range []struct {
		code          smtp.EnhancedCode
		message, want string
	}{
		{smtp.EnhancedCode{5, 7, 139}, "synthetic", "external_smtp_auth_disabled"},
		{smtp.EnhancedCode{5, 7, 3}, "synthetic", "external_smtp_auth_failed"},
		{smtp.EnhancedCode{}, "5.7.3 synthetic", "external_smtp_auth_failed"},
		{smtp.EnhancedCode{}, "5.7.30 synthetic", "external_smtp_auth_failed"},
		{smtp.EnhancedCode{}, "5.7.139 synthetic", "external_smtp_auth_failed"},
		{smtp.EnhancedCode{}, "SMTPCLIENTAUTHENTICATION IS DISABLED", "external_smtp_auth_disabled"},
	} {
		got := classifyExternalSMTPError(&smtp.SMTPError{Code: 535, EnhancedCode: tt.code, Message: tt.message}, true, false)
		if got.code != tt.want {
			t.Errorf("%v %q: %s", tt.code, tt.message, got.code)
		}
	}
}

func TestExternalSendSubject(t *testing.T) {
	raw := []byte("Subject: =?UTF-8?B?5ZCI5oiQ?=\r\n\r\nbody")
	if got := externalSendSubject("external", raw); got != "合成" {
		t.Fatal(got)
	}
	subject := "合\t\x01" + strings.Repeat("成", 301)
	got := externalSendSubject("external", []byte("Subject: "+subject+"\r\n\r\nbody"))
	if len([]rune(got)) != 300 || strings.ContainsAny(got, "\t\x01") {
		t.Fatal("subject was not bounded or cleaned")
	}
	if got := externalSendSubject("relay", raw); got != "" {
		t.Fatal(got)
	}
}

func TestExternalResolveDeliveredWithoutAppendClearsMIME(t *testing.T) {
	f := newExternalSMTPFixture(t)
	f.create(t, "127.0.0.1", 465, "tls", "server")
	id := f.enqueue(t)
	if _, err := f.a.db.Exec(`UPDATE send_queue SET status='unconfirmed' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	res := f.request(t, "POST", "/accounts/"+f.account.ID+"/sends/"+id+"/resolve", map[string]any{"action": "delivered"}, nil)
	if res.Code != 200 {
		t.Fatal(res.Code, res.Body.String())
	}
	assertExternalQueueMIME(t, f, id, false)
}

func TestExternalTerminalQueueCannotRetryClearedMIME(t *testing.T) {
	f := newExternalSMTPFixture(t)
	f.create(t, "127.0.0.1", 465, "tls", "server")
	id := f.enqueue(t)
	item, err := f.a.claimSendQueueItem(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	f.a.markSendQueueFailed(t.Context(), item, &externalSMTPError{code: "external_smtp_auth_failed", permanent: true})
	assertExternalQueueMIME(t, f, id, false)
	res := f.request(t, "POST", "/admin/send/"+id+"/retry", nil, nil)
	if res.Code != 409 {
		t.Fatal("terminal MIME must be recomposed", res.Code)
	}
	id = f.enqueue(t)
	// The compose fixture creates a fresh Message-ID, so this is a new queue item.
	res = f.request(t, "POST", "/admin/send/"+id+"/cancel", nil, nil)
	if res.Code != 200 {
		t.Fatal(res.Code)
	}
	assertExternalQueueMIME(t, f, id, false)
}

func TestExternalBackupCanceledMIMECleared(t *testing.T) {
	f := newExternalSMTPFixture(t)
	f.create(t, "127.0.0.1", 465, "tls", "server")
	id := f.enqueue(t)
	tx, err := f.a.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = pauseNativeWork(t.Context(), tx, "send_queue", []string{"queued", "sending", "failed", "pending"}, []string{"delivered", "canceled", "unconfirmed"}, "canceled", 100)
	if err != nil {
		t.Fatal(err)
	}
	var raw, status string
	if err := tx.QueryRow(`SELECT status,mime_base64 FROM send_queue WHERE id=?`, id).Scan(&status, &raw); err != nil || status != "canceled" || raw != "" {
		t.Fatal("backup canceled MIME retained", err)
	}
}

func TestExternalSMTPAccountDisappearsBeforeDelivery(t *testing.T) {
	f := newExternalSMTPFixture(t)
	f.create(t, "127.0.0.1", 465, "tls", "server")
	id := f.enqueue(t)
	// Simulate deletion racing with a worker rather than the normal transactional
	// delete handler. A configured relay must never become a fallback route.
	if _, err := f.a.db.Exec(`DELETE FROM external_imap_accounts WHERE id=?`, f.account.ID); err != nil {
		t.Fatal(err)
	}
	f.a.cfg.SMTPHost = "127.0.0.1"
	f.a.cfg.SMTPPort = "1"
	if err := f.a.processDueSendQueue(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, code, _, attempt, max := f.state(t, id)
	if status != "failed" || code != "external_account_unavailable" || attempt != max {
		t.Fatal(status, code, attempt, max)
	}
	assertExternalQueueMIME(t, f, id, false)
}
