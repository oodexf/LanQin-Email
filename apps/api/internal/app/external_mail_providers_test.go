package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
)

func TestExternalMailProviderRegistry(t *testing.T) {
	want := []string{"gmail", "outlook", "yahoo", "icloud", "qq", "163", "custom"}
	seen := map[string]bool{}
	for i, p := range externalMailProviders {
		if i >= len(want) || p.ID != want[i] || seen[p.ID] {
			t.Fatalf("invalid provider %s", p.ID)
		}
		seen[p.ID] = true
		if p.Name == "" || len(p.AuthMethods) == 0 || (p.SMTPTLSMode != "tls" && p.SMTPTLSMode != "starttls") {
			t.Fatal(p)
		}
		if p.ID == "custom" {
			if p.IMAPHost != "" || p.SMTPHost != "" || p.IMAPPort != 0 || p.SMTPPort != 0 {
				t.Fatal(p)
			}
		} else if p.IMAPHost == "" || p.SMTPHost == "" || p.IMAPPort < 1 || p.IMAPPort > 65535 || p.SMTPPort < 1 || p.SMTPPort > 65535 || p.IMAPTLSMode != "tls" {
			t.Fatal(p)
		}
		methods := map[string]string{"gmail": "oauth2,app_password", "outlook": "oauth2", "yahoo": "app_password", "icloud": "app_password", "qq": "app_password", "163": "app_password", "custom": "app_password"}
		if strings.Join(p.AuthMethods, ",") != methods[p.ID] {
			t.Fatal(p.AuthMethods)
		}
		oauth := false
		for _, method := range p.AuthMethods {
			if method != "oauth2" && method != "app_password" {
				t.Fatal(method)
			}
			oauth = oauth || method == "oauth2"
		}
		if oauth != (p.OAuthProvider != "") || (oauth && p.ID != p.OAuthProvider) {
			t.Fatal("OAuth mismatch", p)
		}
		mode := "append"
		if oauth {
			mode = "server"
		}
		if p.SentCopyMode != mode || p.RequiresIMAPID != (p.ID == "163") {
			t.Fatal(p)
		}
	}
	if len(seen) != len(want) {
		t.Fatal("missing providers")
	}
	a := newTestApp(t)
	stopTestWorkers(a)
	a.cfg.PublicBaseURL = "https://mail.example.test"
	a.cfg.ExternalIMAPGmailClientID = "synthetic-client"
	a.cfg.ExternalIMAPGmailClientSecret = "synthetic-secret"
	a.cfg.ExternalIMAPOutlookClientID = "synthetic-client"
	a.cfg.ExternalIMAPOutlookClientSecret = "synthetic-secret"
	for _, id := range []string{"gmail", "outlook"} {
		preset, _ := externalMailProviderByID(id)
		_, profile, err := a.externalIMAPOAuthConfig(id)
		if err != nil || profile.Host != preset.IMAPHost || profile.Port != preset.IMAPPort || profile.TLSMode != preset.IMAPTLSMode {
			t.Fatal(profile, err)
		}
		host, port, mode := externalOAuthSMTPEndpoint(id)
		if host != preset.SMTPHost || port != preset.SMTPPort || mode != preset.SMTPTLSMode {
			t.Fatal("SMTP mismatch")
		}
	}
}

func TestExternalMailProvidersAPI(t *testing.T) {
	f := newExternalSMTPFixture(t)
	server := httptest.NewServer(f.a.Router())
	defer server.Close()
	client := &testClient{t: t, server: server}
	if status := client.do("GET", "/api/me/external-mail-providers", nil, nil); status != 401 {
		t.Fatal(status)
	}
	if status := client.do("POST", "/api/auth/login", map[string]string{"email": f.a.cfg.AdminEmail, "password": f.a.cfg.AdminPassword}, nil); status != 200 {
		t.Fatal(status)
	}
	for _, tt := range []struct {
		gmailID, gmailSecret, outlookID, outlookSecret string
		gmail, outlook                                 bool
	}{
		{}, {gmailID: "synthetic-gmail-id"}, {gmailSecret: "synthetic-gmail-secret"},
		{gmailID: "synthetic-gmail-id", gmailSecret: "synthetic-gmail-secret", gmail: true},
		{outlookID: "synthetic-outlook-id"}, {outlookSecret: "synthetic-outlook-secret"},
		{outlookID: "synthetic-outlook-id", outlookSecret: "synthetic-outlook-secret", outlook: true},
	} {
		f.a.cfg.ExternalIMAPGmailClientID, f.a.cfg.ExternalIMAPGmailClientSecret = tt.gmailID, tt.gmailSecret
		f.a.cfg.ExternalIMAPOutlookClientID, f.a.cfg.ExternalIMAPOutlookClientSecret = tt.outlookID, tt.outlookSecret
		var response struct {
			Items []ExternalMailProvider `json:"items"`
		}
		if status := client.do("GET", "/api/me/external-mail-providers", nil, &response); status != 200 {
			t.Fatal(status)
		}
		if len(response.Items) != 7 {
			t.Fatal(response)
		}
		for _, p := range response.Items {
			want := (p.ID == "gmail" && tt.gmail) || (p.ID == "outlook" && tt.outlook)
			if p.OAuthConfigured != want {
				t.Fatal(p)
			}
		}
		// Check the actual serialized handler response for values and secret-bearing keys.
		rec := httptest.NewRecorder()
		f.a.handleExternalMailProviders(rec, httptest.NewRequest("GET", "/", nil))
		body := rec.Body.String()
		for _, forbidden := range []string{"synthetic-", "clientId", "clientSecret", "passwordCiphertext", "accessToken"} {
			if strings.Contains(body, forbidden) {
				t.Fatal("provider response leaked configuration", forbidden)
			}
		}
	}
	// A real authenticated user without mail.access cannot read the registry.
	mailbox := createTestMailbox(t, client, mustDefaultDomainID(t, f.a), "provider-viewer", "Synthetic viewer", "Password123!", nil)
	viewer := &testClient{t: t, server: server}
	if status := viewer.do("POST", "/api/auth/login", map[string]string{"email": mailbox.Address, "password": "Password123!"}, nil); status != 200 {
		t.Fatal(status)
	}
	if _, err := f.a.db.Exec(`UPDATE permission_groups SET permissions_json='[]' WHERE id=?`, PermissionGroupRegular); err != nil {
		t.Fatal(err)
	}
	if status := viewer.do("GET", "/api/me/external-mail-providers", nil, nil); status != 403 {
		t.Fatal("mail.access gate", status)
	}
	// The same feature gate as external account management applies.
	f.a.cfg.ExternalIMAPEnabled = false
	if status := client.do("GET", "/api/me/external-mail-providers", nil, nil); status != 403 {
		t.Fatal(status)
	}
}

func TestExternalMailProviderPayload(t *testing.T) {
	f := newExternalSMTPFixture(t)
	body := map[string]any{"mailboxId": f.mb.ID, "host": "127.0.0.1", "port": 1993, "tlsMode": "tls", "username": "synthetic@example.test", "password": "synthetic-password", "providerPreset": "unknown"}
	res := f.request(t, "POST", "/accounts", body, nil)
	if res.Code != 400 || !strings.Contains(res.Body.String(), "unknown mail provider") {
		t.Fatal(res.Code, res.Body.String())
	}
	for _, preset := range []string{"", "qq", "163", "outlook"} {
		body["providerPreset"] = preset
		res = f.request(t, "POST", "/accounts", body, nil)
		var account ExternalIMAPAccount
		if res.Code != 201 || json.Unmarshal(res.Body.Bytes(), &account) != nil {
			t.Fatal(res.Code, res.Body.String())
		}
		want := preset
		if want == "" {
			want = "custom"
		}
		mode := "append"
		if preset == "outlook" {
			mode = "server"
		}
		if account.ProviderPreset != want || account.Host != "127.0.0.1" || account.Port != 1993 || account.SentCopyMode != mode {
			t.Fatal(account)
		}
		// Partial updates preserve preset and endpoints; unknown ids never reach storage.
		res = f.request(t, "POST", "/accounts/"+account.ID, map[string]any{"name": "Updated"}, nil)
		var updated ExternalIMAPAccount
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &updated) != nil || updated.ProviderPreset != want {
			t.Fatal(res.Code, res.Body.String())
		}
		res = f.request(t, "POST", "/accounts/"+account.ID, map[string]any{"providerPreset": "unknown"}, nil)
		if res.Code != 400 || !strings.Contains(res.Body.String(), "unknown mail provider") {
			t.Fatal(res.Code, res.Body.String())
		}
	}
	oauth := newExternalMailOAuthFixture(t)
	account := oauth.connect(t)
	for _, preset := range []string{"outlook", "custom", "qq", "", "unknown"} {
		res := oauth.request(t, "PUT", "/accounts/"+account.ID, map[string]any{"providerPreset": preset}, nil, nil)
		want := "oauth account provider does not match"
		if preset == "unknown" {
			want = "unknown mail provider"
		}
		if res.Code != 400 || !strings.Contains(res.Body.String(), want) {
			t.Fatal(preset, res.Code, res.Body.String())
		}
	}
	res = oauth.request(t, "PUT", "/accounts/"+account.ID, map[string]any{"providerPreset": "gmail"}, nil, nil)
	if res.Code != 200 {
		t.Fatal(res.Code, res.Body.String())
	}
}

// A local protocol peer verifies the real beta.8 ID command and post-login wire
// order. The preset case goes through openExternalIMAPClient; hostname cases
// exercise the same post-login helper without resolving or dialing real servers.
func TestExternalMailIMAPID(t *testing.T) {
	a := newTestApp(t)
	stopTestWorkers(a)
	a.cfg.ExternalIMAPAllowPrivateHosts = true
	a.cfg.ExternalIMAPSecretKey = "synthetic-id-key"
	ciphertext, err := a.encryptExternalIMAPPassword("synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, preset, host        string
		capID, failID, open, want bool
	}{
		{name: "preset", preset: "163", capID: true, open: true, want: true},
		{name: "preset failure", preset: "163", capID: true, failID: true, open: true, want: true},
		{name: "no capability", preset: "163", open: true},
		{name: "other", preset: "qq", capID: true, open: true},
		{name: "163 host", host: "imap.163.com", capID: true, want: true},
		{name: "126 host", host: "IMAP.126.COM.", capID: true, want: true},
		{name: "yeah host", host: "imap.yeah.net", capID: true, want: true},
		{name: "other host", host: "imap.gmail.com", capID: true},
		{name: "suffix spoof", host: "imap.163.com.example.test", capID: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			ids := make(chan string, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				caps := "IMAP4rev1"
				if tt.capID {
					caps += " ID"
				}
				_, err = fmt.Fprintf(conn, "* OK [CAPABILITY %s] synthetic IMAP\r\n", caps)
				reader := bufio.NewScanner(conn)
				loggedIn := false
				for err == nil && reader.Scan() {
					line := reader.Text()
					parts := strings.SplitN(line, " ", 3)
					if len(parts) < 2 {
						err = fmt.Errorf("invalid command")
						break
					}
					tag, command := parts[0], parts[1]
					switch command {
					case "CAPABILITY":
						_, err = fmt.Fprintf(conn, "* CAPABILITY %s\r\n%s OK caps\r\n", caps, tag)
					case "LOGIN":
						loggedIn = true
						_, err = fmt.Fprintf(conn, "%s OK [CAPABILITY %s] authenticated\r\n", tag, caps)
					case "ID":
						if !loggedIn {
							err = fmt.Errorf("ID before login")
							break
						}
						select {
						case ids <- line:
						default:
							err = fmt.Errorf("duplicate ID")
							continue
						}
						if tt.failID {
							_, err = fmt.Fprintf(conn, "%s NO synthetic ID failure\r\n", tag)
						} else {
							_, err = fmt.Fprintf(conn, "* ID NIL\r\n%s OK ID\r\n", tag)
						}
					case "NOOP":
						_, err = fmt.Fprintf(conn, "%s OK noop\r\n", tag)
					case "LOGOUT":
						_, err = fmt.Fprintf(conn, "* BYE closing\r\n%s OK logout\r\n", tag)
						done <- err
						return
					default:
						err = fmt.Errorf("unexpected command %s", command)
					}
				}
				if err == nil {
					err = reader.Err()
				}
				done <- err
			}()
			host, portText, _ := net.SplitHostPort(listener.Addr().String())
			port, _ := strconv.Atoi(portText)
			account := externalIMAPAccountRecord{ExternalIMAPAccount: ExternalIMAPAccount{Host: host, Port: port, TLSMode: "plain", AuthStatus: "ok", Username: "synthetic@example.test", ProviderPreset: tt.preset}, PasswordCiphertext: ciphertext}
			var c *imapclient.Client
			var opened externalIMAPClient
			if tt.open {
				opened, err = a.openExternalIMAPClient(t.Context(), account)
				if err != nil {
					t.Fatal(err)
				}
				c = opened.(*goExternalIMAPClient).client
			} else {
				c, err = imapclient.DialInsecure(listener.Addr().String(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if err = c.Login(account.Username, "synthetic-password").Wait(); err != nil {
					t.Fatal(err)
				}
				account.Host = tt.host
				a.sendExternalIMAPID(c, account.ExternalIMAPAccount)
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatal("ID failure broke session", err)
			}
			var id string
			select {
			case id = <-ids:
			default:
			}
			if (id != "") != tt.want {
				t.Fatalf("ID=%q want=%v", id, tt.want)
			}
			if id != "" && !strings.HasSuffix(id, ` ID ("name" "LanQin" "version" "1" "vendor" "LanQin")`) {
				t.Fatalf("unexpected ID metadata %q", id)
			}
			if opened != nil {
				_ = opened.Close()
			} else {
				_ = c.Logout().Wait()
				_ = c.Close()
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
