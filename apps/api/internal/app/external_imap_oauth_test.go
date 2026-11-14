package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/oauth2"
)

func loadTestExternalMailOAuthState(t *testing.T, a *App, state string) (externalIMAPOAuthState, error) {
	t.Helper()
	var ciphertext string
	if err := a.db.QueryRowContext(t.Context(), `SELECT payload_ciphertext FROM external_mail_oauth_states WHERE token_hash=?`, hashToken(state)).Scan(&ciphertext); err != nil {
		return externalIMAPOAuthState{}, err
	}
	raw, err := a.decryptExternalIMAPPassword(ciphertext)
	if err != nil {
		return externalIMAPOAuthState{}, err
	}
	var result externalIMAPOAuthState
	err = json.Unmarshal([]byte(raw), &result)
	return result, err
}

type externalMailOAuthFixture struct {
	a                 *App
	user              *User
	mailbox           *Mailbox
	router            http.Handler
	tokenServer       *httptest.Server
	calls             atomic.Int32
	claims            map[string]any
	subject, email    string
	tokenType, access string
	tokenHandler      func(http.ResponseWriter, *http.Request)
	nonce, challenge  string
}

func newExternalMailOAuthFixture(t *testing.T) *externalMailOAuthFixture {
	t.Helper()
	return newExternalMailOAuthFixtureForApp(t, newTestApp(t))
}

func newExternalMailOAuthFixtureForApp(t *testing.T, a *App) *externalMailOAuthFixture {
	t.Helper()
	stopTestWorkers(a)
	previousConfig, previousOAuthConfig := a.cfg, a.externalIMAPOAuthConfigForTest
	t.Cleanup(func() {
		a.cfg, a.externalIMAPOAuthConfigForTest = previousConfig, previousOAuthConfig
	})
	a.cfg.ExternalIMAPEnabled = true
	a.cfg.ExternalIMAPSecretKey = "synthetic-secret"
	a.cfg.ExternalIMAPAllowPrivateHosts = true
	a.cfg.PublicBaseURL = "https://mail.example.test"
	user, _, err := a.userByEmail(t.Context(), a.cfg.AdminEmail)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := a.mailboxByAddress(t.Context(), a.cfg.AdminEmail)
	if err != nil {
		t.Fatal(err)
	}
	f := &externalMailOAuthFixture{a: a, user: user, mailbox: mb, subject: "synthetic-subject", email: "person@example.test", tokenType: "Bearer", access: "synthetic-access"}
	f.tokenServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if f.tokenHandler != nil {
			f.tokenHandler(w, r)
			return
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "synthetic-code" || oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != f.challenge || r.Form.Get("code_verifier") == "" {
			t.Error("missing/incorrect PKCE verifier or authorization code")
		}
		claims := map[string]any{"iss": "https://issuer.example.test", "aud": "synthetic-client", "exp": a.now().Add(time.Hour).Unix(), "nonce": f.nonce, "sub": f.subject, "email": f.email}
		for k, v := range f.claims {
			claims[k] = v
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": f.access, "token_type": f.tokenType, "refresh_token": "synthetic-refresh", "expires_in": 3600, "id_token": testIDToken(claims)})
	}))
	t.Cleanup(f.tokenServer.Close)
	a.externalIMAPOAuthConfigForTest = func(provider string) (*oauth2.Config, externalIMAPOAuthProvider, error) {
		return &oauth2.Config{ClientID: "synthetic-client", ClientSecret: "synthetic-client-secret", RedirectURL: a.cfg.PublicBaseURL + "/api/external-imap-oauth/" + provider + "/callback", Scopes: []string{"openid", "email", "https://mail.google.com/"}, Endpoint: oauth2.Endpoint{AuthURL: f.tokenServer.URL + "/authorize", TokenURL: f.tokenServer.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}}, externalIMAPOAuthProvider{TLSMode: externalIMAPTLS, Name: "Synthetic provider", Host: "127.0.0.1", Port: 993, Issuer: "https://issuer.example.test"}, nil
	}
	router := chi.NewRouter()
	router.Post("/start/{provider}", a.handleStartExternalIMAPOAuth)
	router.Get("/callback/{provider}", a.handleExternalIMAPOAuthCallback)
	router.Put("/accounts/{id}", a.handleUpdateExternalIMAPAccount)
	router.Post("/accounts/{id}/test", a.handleTestExternalIMAPAccount)
	router.Post("/accounts/{id}/sync", a.handleSyncExternalIMAPAccount)
	f.router = router
	return f
}

func (f *externalMailOAuthFixture) request(t *testing.T, method, path string, body any, cookie *http.Cookie, user *User) *httptest.ResponseRecorder {
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
	if cookie != nil {
		req.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, req)
	return response
}

func (f *externalMailOAuthFixture) start(t *testing.T, body map[string]any) (string, *http.Cookie) {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	if body["mailboxId"] == nil {
		body["mailboxId"] = f.mailbox.ID
	}
	response := f.request(t, "POST", "/start/gmail", body, nil, nil)
	if response.Code != 200 {
		t.Fatalf("start: %d %s", response.Code, response.Body.String())
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(out.URL)
	if err != nil {
		t.Fatal(err)
	}
	q := link.Query()
	f.nonce, f.challenge = q.Get("nonce"), q.Get("code_challenge")
	if f.nonce == "" || f.challenge == "" || q.Get("code_challenge_method") != "S256" || q.Get("prompt") != "consent" || q.Get("access_type") != "offline" {
		t.Fatal("unsafe authorization request")
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].Path != externalMailOAuthCookiePath {
		t.Fatal("unsafe binding cookie")
	}
	state := q.Get("state")
	var hash, binding, encrypted string
	if err := f.a.db.QueryRow(`SELECT token_hash,binding_hash,payload_ciphertext FROM external_mail_oauth_states WHERE token_hash=?`, hashToken(state)).Scan(&hash, &binding, &encrypted); err != nil {
		t.Fatal(err)
	}
	if hash == state || binding == cookies[0].Value || binding != hashToken(cookies[0].Value) || strings.Contains(encrypted, f.nonce) {
		t.Fatal("plaintext state/binding/payload persisted")
	}
	return state, cookies[0]
}

func (f *externalMailOAuthFixture) callback(t *testing.T, state string, cookie *http.Cookie, extra, want string) {
	t.Helper()
	response := f.request(t, "GET", "/callback/gmail?state="+url.QueryEscape(state)+"&code=synthetic-code"+extra, nil, cookie, nil)
	if response.Code != 302 || response.Header().Get("Location") != "https://mail.example.test/profile?tab=mailboxes&externalMail="+want {
		t.Fatalf("callback: status=%d redirect=%q body=%s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge >= 0 || cookies[0].Path != externalMailOAuthCookiePath {
		t.Fatal("callback did not clear binding cookie")
	}
}

func (f *externalMailOAuthFixture) connect(t *testing.T) externalIMAPAccountRecord {
	t.Helper()
	state, cookie := f.start(t, nil)
	f.callback(t, state, cookie, "", "connected")
	var id string
	if err := f.a.db.QueryRow(`SELECT id FROM external_imap_accounts WHERE user_id=?`, f.user.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	account, err := f.a.externalIMAPAccountForUser(t.Context(), f.user.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if account.OAuthSubject != f.subject || account.AuthStatus != "ok" || account.ProviderPreset != "gmail" {
		t.Fatal("OAuth identity/preset/status not persisted")
	}
	return account
}

func TestExternalMailOAuthStateSingleUseAndPKCE(t *testing.T) {
	f := newExternalMailOAuthFixture(t)
	state, cookie := f.start(t, nil)
	f.callback(t, state, cookie, "", "connected")
	f.callback(t, state, cookie, "", "invalid_state")
	if f.calls.Load() != 1 {
		t.Fatal("replay exchanged token")
	}
}

func TestExternalMailOAuthStateCleanup(t *testing.T) {
	assertExternalMailOAuthStateCleanup(t, newExternalMailOAuthFixture(t))
}

func assertExternalMailOAuthStateCleanup(t *testing.T, f *externalMailOAuthFixture) {
	t.Helper()
	now := f.a.now().UTC()
	// Use different timestamps so eviction must follow age rather than token hash.
	otherUser := newID("usr")
	if _, err := f.a.db.Exec(`INSERT INTO users(id,email,display_name,role,password_hash,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, otherUser, otherUser+"@example.test", "Synthetic", "user", "unused", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.a.db.Exec(`DELETE FROM users WHERE id=?`, otherUser) })
	hashes := map[string]string{}
	for _, name := range []string{"expired", "expires_now", "expired_used", "other_expired", "used", "other_active", "oldest", "older", "keep1", "keep2", "keep3", "keep4"} {
		userID, expiry := f.user.ID, now.Add(time.Hour)
		if strings.HasPrefix(name, "other_") {
			userID = otherUser
		}
		if strings.Contains(name, "expired") {
			expiry = now.Add(-time.Second)
		} else if name == "expires_now" {
			expiry = now
		}
		var used any
		if strings.Contains(name, "used") {
			used = now.Add(-time.Minute).Format(time.RFC3339Nano)
		}
		created := now.Add(-time.Minute)
		if name == "oldest" {
			created = now.Add(-3 * time.Minute)
		} else if name == "older" {
			created = now.Add(-2 * time.Minute)
		}
		hashes[name] = hashToken(randomToken())
		if _, err := f.a.db.Exec(`INSERT INTO external_mail_oauth_states(token_hash,user_id,binding_hash,provider,mailbox_id,payload_ciphertext,expires_at,used_at,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, hashes[name], userID, "synthetic-binding", "gmail", f.mailbox.ID, "synthetic-payload", expiry.Format(time.RFC3339Nano), used, created.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	// Pin the exact expiry boundary; no wall-clock delay is needed.
	previousNow := f.a.now
	f.a.now = func() time.Time { return now }
	t.Cleanup(func() { f.a.now = previousNow })
	f.start(t, nil)
	for name, hash := range hashes {
		want := 1
		if strings.Contains(name, "expired") || name == "expires_now" || name == "oldest" || name == "older" {
			want = 0
		}
		var count int
		if err := f.a.db.QueryRow(`SELECT COUNT(*) FROM external_mail_oauth_states WHERE token_hash=?`, hash).Scan(&count); err != nil || count != want {
			t.Fatalf("state %s: count=%d, want %d: %v", name, count, want, err)
		}
	}
	// Repeated starts remain bounded, even when creation times tie.
	for i := 0; i < 8; i++ {
		f.start(t, nil)
		var count int
		if err := f.a.db.QueryRow(`SELECT COUNT(*) FROM external_mail_oauth_states WHERE user_id=? AND used_at IS NULL AND expires_at>?`, f.user.ID, now.Format(time.RFC3339Nano)).Scan(&count); err != nil || count != 5 {
			t.Fatalf("active states=%d, want 5: %v", count, err)
		}
	}
}

func TestExternalMailOAuthConcurrentStartsStayBounded(t *testing.T) {
	assertExternalMailOAuthConcurrentStartsStayBounded(t, newExternalMailOAuthFixture(t))
}

func assertExternalMailOAuthConcurrentStartsStayBounded(t *testing.T, f *externalMailOAuthFixture) {
	t.Helper()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			<-start
			response := f.request(t, "POST", "/start/gmail", map[string]any{"mailboxId": f.mailbox.ID}, nil, nil)
			if response.Code != http.StatusOK {
				t.Errorf("start: %d %s", response.Code, response.Body.String())
			}
		})
	}
	close(start)
	wg.Wait()
	var count int
	if err := f.a.db.QueryRow(`SELECT COUNT(*) FROM external_mail_oauth_states WHERE user_id=? AND used_at IS NULL AND expires_at>?`, f.user.ID, f.a.now().UTC().Format(time.RFC3339Nano)).Scan(&count); err != nil || count != 5 {
		t.Fatalf("active states=%d, want 5: %v", count, err)
	}
}

func TestExternalMailOAuthRejectsInvalidState(t *testing.T) {
	for _, scenario := range []string{"expired", "cookie_mismatch", "missing_cookie", "provider_mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExternalMailOAuthFixture(t)
			state, cookie := f.start(t, nil)
			switch scenario {
			case "expired":
				_, err := f.a.db.Exec(`UPDATE external_mail_oauth_states SET expires_at=?`, f.a.now().Add(-time.Minute).UTC().Format(time.RFC3339Nano))
				if err != nil {
					t.Fatal(err)
				}
			case "cookie_mismatch":
				cookie.Value = "synthetic-wrong-browser"
			case "missing_cookie":
				cookie = nil
			case "provider_mismatch":
				_, err := f.a.db.Exec(`UPDATE external_mail_oauth_states SET provider='outlook'`)
				if err != nil {
					t.Fatal(err)
				}
			}
			f.callback(t, state, cookie, "", "invalid_state")
			if f.calls.Load() != 0 {
				t.Fatal("invalid state reached token endpoint")
			}
		})
	}
}

func TestExternalMailOAuthRejectsInvalidTokens(t *testing.T) {
	for _, scenario := range []string{"nonce", "aud", "issuer", "expired", "subject", "token_type", "access_token", "email"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExternalMailOAuthFixture(t)
			state, cookie := f.start(t, map[string]any{"email": "person@example.test"})
			f.claims = map[string]any{}
			want := "exchange_failed"
			switch scenario {
			case "nonce":
				f.claims["nonce"] = "wrong-nonce"
			case "aud":
				f.claims["aud"] = "other-client"
			case "issuer":
				f.claims["iss"] = "https://wrong-issuer.example.test"
			case "expired":
				f.claims["exp"] = f.a.now().Add(-time.Minute).Unix()
			case "subject":
				f.claims["sub"] = ""
			case "token_type":
				f.tokenType = "MAC"
			case "access_token":
				f.access = ""
			case "email":
				f.email = "other@example.test"
				want = "email_mismatch"
			}
			f.callback(t, state, cookie, "", want)
			var count int
			if err := f.a.db.QueryRow(`SELECT COUNT(*) FROM external_imap_accounts`).Scan(&count); err != nil || count != 0 {
				t.Fatal("invalid token created account", err)
			}
		})
	}
}

func TestExternalMailOAuthCallbackFixedResults(t *testing.T) {
	for _, scenario := range []string{"denied", "not_configured", "failed"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExternalMailOAuthFixture(t)
			state, cookie := f.start(t, nil)
			extra := ""
			switch scenario {
			case "denied":
				extra = "&error=synthetic-secret-provider-error"
			case "not_configured":
				f.a.externalIMAPOAuthConfigForTest = func(string) (*oauth2.Config, externalIMAPOAuthProvider, error) {
					return nil, externalIMAPOAuthProvider{}, errors.New("synthetic-secret-config-error")
				}
			}
			want := scenario
			if scenario == "failed" { // Fail persistence after a valid exchange instead.
				f.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
					// Simulate the target mailbox disappearing during the token exchange.
					if _, err := f.a.db.Exec(`DELETE FROM mailboxes WHERE id=?`, f.mailbox.ID); err != nil {
						t.Error(err)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic", "token_type": "Bearer", "id_token": testIDToken(map[string]any{"iss": "https://issuer.example.test", "aud": "synthetic-client", "exp": f.a.now().Add(time.Hour).Unix(), "nonce": f.nonce, "sub": f.subject, "email": f.email})})
				}
			}
			f.callback(t, state, cookie, extra, want)
		})
	}
}

func TestExternalMailOAuthCallbackRechecksUserAccess(t *testing.T) {
	f := newExternalMailOAuthFixture(t)
	state, cookie := f.start(t, nil)
	if _, err := f.a.db.Exec(`UPDATE users SET disabled=1 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	f.callback(t, state, cookie, "", "failed")
	if f.calls.Load() != 0 {
		t.Fatal("disabled user exchanged an authorization code")
	}
	var count int
	if err := f.a.db.QueryRow(`SELECT COUNT(*) FROM external_imap_accounts WHERE user_id=?`, f.user.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("disabled user created an external account: count=%d err=%v", count, err)
	}
}

func TestExternalMailOAuthReauthorizationIdentityAndGeneration(t *testing.T) {
	for _, scenario := range []string{"different_subject", "different_subject_and_email", "same_subject", "legacy_same_email", "legacy_other_email"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExternalMailOAuthFixture(t)
			account := f.connect(t)
			if _, err := f.a.db.Exec(`UPDATE external_imap_accounts SET auth_status='needs_reauth',last_error='reauthorization required',oauth_generation=7 WHERE id=?`, account.ID); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(scenario, "legacy") {
				if _, err := f.a.db.Exec(`UPDATE external_imap_accounts SET oauth_subject='' WHERE id=?`, account.ID); err != nil {
					t.Fatal(err)
				}
			}
			state, cookie := f.start(t, map[string]any{"accountId": account.ID, "email": "person@example.test"})
			f.access = "synthetic-new-access"
			want := "reauthorized"
			switch scenario {
			case "different_subject":
				f.subject = "different-subject"
				want = "account_mismatch"
			case "different_subject_and_email":
				f.subject = "different-subject"
				f.email = "different@example.test"
				want = "account_mismatch"
			case "legacy_other_email":
				f.email = "another@example.test"
				want = "account_mismatch"
			}
			f.callback(t, state, cookie, "", want)
			current, err := f.a.externalIMAPAccountForUser(t.Context(), f.user.ID, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if want == "account_mismatch" {
				if current.OAuthGeneration != 7 || current.AuthStatus != "needs_reauth" || current.OAuthAccessTokenCiphertext != account.OAuthAccessTokenCiphertext {
					t.Fatal("mismatched identity modified account")
				}
			} else {
				access, err := f.a.decryptExternalIMAPPassword(current.OAuthAccessTokenCiphertext)
				if err != nil || current.OAuthGeneration != 8 || current.AuthStatus != "ok" || current.LastError != "" || current.OAuthSubject != f.subject || access != f.access {
					t.Fatal("reauthorization did not restore account", err)
				}
			}
		})
	}
}

func TestExternalMailOAuthStartRejectsOtherUsersAccount(t *testing.T) {
	f := newExternalMailOAuthFixture(t)
	account := f.connect(t)
	for _, test := range []struct {
		user     *User
		provider string
		want     int
	}{{&User{ID: "synthetic-other-user"}, "gmail", 404}, {f.user, "outlook", 400}} {
		response := f.request(t, "POST", "/start/"+test.provider, map[string]any{"accountId": account.ID, "mailboxId": f.mailbox.ID}, nil, test.user)
		if response.Code != test.want {
			t.Fatalf("start returned %d, want %d", response.Code, test.want)
		}
	}
}

func TestExternalMailOAuthAccountEndpointsPinned(t *testing.T) {
	f := newExternalMailOAuthFixture(t)
	account := f.connect(t)
	for _, body := range []map[string]any{{"host": "attacker.example.test"}, {"port": 143}, {"tlsMode": "plain"}, {"username": "attacker@example.test"}} {
		response := f.request(t, "PUT", "/accounts/"+account.ID, body, nil, nil)
		if response.Code != 400 || !strings.Contains(response.Body.String(), "oauth account endpoints cannot be changed") {
			t.Fatalf("endpoint edit accepted: %d %s", response.Code, response.Body.String())
		}
	}
	response := f.request(t, "PUT", "/accounts/"+account.ID, map[string]any{"name": "Renamed", "password": "ignored-synthetic-password"}, nil, nil)
	if response.Code != 200 {
		t.Fatalf("name edit: %d %s", response.Code, response.Body.String())
	}
	current, err := f.a.externalIMAPAccountForUser(t.Context(), f.user.ID, account.ID)
	if err != nil || current.Name != "Renamed" || current.Host != account.Host || current.PasswordCiphertext != account.PasswordCiphertext {
		t.Fatal("incorrect OAuth account edit", err)
	}
}

func TestExternalMailOAuthRefreshSingleflightAndPreservesRefreshToken(t *testing.T) {
	f := newExternalMailOAuthFixture(t)
	account := f.connect(t)
	if _, err := f.a.db.Exec(`UPDATE external_imap_accounts SET oauth_expiry=? WHERE id=?`, f.a.now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), account.ID); err != nil {
		t.Fatal(err)
	}
	f.calls.Store(0)
	f.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "synthetic-refresh" {
			t.Error("incorrect refresh request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-refreshed", "token_type": "Bearer", "expires_in": 3600})
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			<-start
			token, err := f.a.externalIMAPOAuthAccessToken(t.Context(), account)
			if err != nil || token != "synthetic-refreshed" {
				t.Errorf("refresh = %q, %v", token, err)
			}
		})
	}
	close(start)
	wg.Wait()
	if f.calls.Load() != 1 {
		t.Fatalf("token endpoint called %d times", f.calls.Load())
	}
	current, err := f.a.externalIMAPAccountForUser(t.Context(), f.user.ID, account.ID)
	if err != nil || current.OAuthRefreshTokenCiphertext != account.OAuthRefreshTokenCiphertext {
		t.Fatal("refresh token lost", err)
	}
}

func TestExternalMailOAuthDisabledAccountRefresh(t *testing.T) {
	for _, expiry := range []time.Duration{-time.Hour, time.Minute} {
		t.Run(expiry.String(), func(t *testing.T) {
			f := newExternalMailOAuthFixture(t)
			account := f.connect(t)
			if _, err := f.a.db.Exec(`UPDATE external_imap_accounts SET enabled=0,oauth_expiry=? WHERE id=?`, f.a.now().Add(expiry).UTC().Format(time.RFC3339Nano), account.ID); err != nil {
				t.Fatal(err)
			}
			account, err := f.a.externalIMAPAccountForUser(t.Context(), f.user.ID, account.ID)
			if err != nil || account.Enabled {
				t.Fatal("disabled account not available to owner", err)
			}
			if _, err := f.a.externalIMAPAccountByID(t.Context(), account.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("worker lookup accepted disabled account", err)
			}
			f.calls.Store(0)
			f.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
				if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "synthetic-refresh" {
					t.Error("incorrect refresh request")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-refreshed", "token_type": "Bearer", "expires_in": 3600})
			}
			if token, err := f.a.externalIMAPOAuthAccessToken(t.Context(), account); err != nil || token != "synthetic-refreshed" {
				t.Fatalf("disabled account refresh = %q, %v", token, err)
			}
			current, err := f.a.externalIMAPAccountForUser(t.Context(), f.user.ID, account.ID)
			if err != nil || current.Enabled || !current.OAuthExpiry.After(f.a.now()) || f.calls.Load() != 1 {
				t.Fatal("refresh did not preserve disabled state and update expiry", err)
			}
		})
	}
}

func TestExternalMailOAuthInvalidGrantRequiresReauthorizationAndSkipsWorker(t *testing.T) {
	f := newExternalMailOAuthFixture(t)
	account := f.connect(t)
	if _, err := f.a.db.Exec(`UPDATE external_imap_accounts SET oauth_expiry=? WHERE id=?`, f.a.now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), account.ID); err != nil {
		t.Fatal(err)
	}
	f.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"synthetic-sensitive-error"}`))
	}
	if _, err := f.a.externalIMAPOAuthAccessToken(t.Context(), account); !errors.Is(err, errExternalMailReauth) {
		t.Fatalf("invalid_grant: %v", err)
	}
	current, err := f.a.externalIMAPAccountForUser(t.Context(), f.user.ID, account.ID)
	if err != nil || current.AuthStatus != "needs_reauth" || current.LastStatus != "error" || current.LastError != "reauthorization required" {
		t.Fatal("invalid_grant status not persisted", err)
	}
	factory := &externalIMAPWorkerTestFactory{client: &externalIMAPSyncTestClient{}}
	f.a.externalIMAP = factory
	f.a.syncDueExternalIMAPAccounts(t.Context())
	if len(factory.accountIDs) != 0 {
		t.Fatal("worker opened needs_reauth account")
	}
	for _, action := range []string{"test", "sync"} {
		response := f.request(t, "POST", "/accounts/"+account.ID+"/"+action, map[string]any{}, nil, nil)
		if response.Code != 400 || !strings.Contains(response.Body.String(), "reauthorization required") {
			t.Fatalf("manual %s: %d %s", action, response.Code, response.Body.String())
		}
	}
	if _, err := f.a.syncExternalIMAPAccountFolder(t.Context(), account.ID, "INBOX"); !errors.Is(err, errExternalMailReauth) {
		t.Fatal("folder sync did not reject needs_reauth", err)
	}
	if len(factory.accountIDs) != 0 {
		t.Fatal("manual action opened needs_reauth account")
	}
}

func TestExternalMailXOAUTH2AcknowledgesErrorChallenge(t *testing.T) {
	client := newExternalMailXOAUTH2Client("person@example.test", "synthetic-token")
	response, err := client.Next([]byte(`{"status":"401","schemes":"bearer","scope":"mail"}`))
	if err != nil || len(response) != 0 {
		t.Fatal("XOAUTH2 did not acknowledge error challenge")
	}
}

func TestExternalMailOAuthRefreshCASDoesNotOverwriteReauthorization(t *testing.T) {
	for _, responseError := range []bool{false, true} {
		name := "success"
		if responseError {
			name = "invalid_grant"
		}
		t.Run(name, func(t *testing.T) {
			f := newExternalMailOAuthFixture(t)
			account := f.connect(t)
			if _, err := f.a.db.Exec(`UPDATE external_imap_accounts SET oauth_expiry=? WHERE id=?`, f.a.now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), account.ID); err != nil {
				t.Fatal(err)
			}
			started, resume := make(chan struct{}), make(chan struct{})
			f.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-resume
				if responseError {
					w.WriteHeader(400)
					_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "stale-refreshed-access", "refresh_token": "stale-rotated-refresh", "token_type": "Bearer", "expires_in": 3600})
			}
			result := make(chan struct {
				token string
				err   error
			}, 1)
			go func() {
				token, err := f.a.externalIMAPOAuthAccessToken(t.Context(), account)
				result <- struct {
					token string
					err   error
				}{token, err}
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("refresh did not start")
			}
			cipher, err := f.a.encryptExternalIMAPPassword("reauthorized-access")
			if err != nil {
				close(resume)
				t.Fatal(err)
			}
			if _, err := f.a.db.Exec(`UPDATE external_imap_accounts SET enabled=0,oauth_generation=oauth_generation+1,oauth_access_token_ciphertext=?,auth_status='ok',last_error='' WHERE id=?`, cipher, account.ID); err != nil {
				close(resume)
				t.Fatal(err)
			}
			close(resume)
			got := <-result
			if got.err != nil || got.token != "reauthorized-access" {
				t.Fatalf("stale refresh won: %q %v", got.token, got.err)
			}
			current, err := f.a.externalIMAPAccountForUser(t.Context(), f.user.ID, account.ID)
			if err != nil || current.Enabled || current.OAuthGeneration != 1 || current.AuthStatus != "ok" || current.OAuthAccessTokenCiphertext != cipher || current.OAuthRefreshTokenCiphertext != account.OAuthRefreshTokenCiphertext {
				t.Fatal("stale response changed reauthorized account", err)
			}
		})
	}
}

func TestExternalMailOAuthRefreshRotatedTokenIsSaved(t *testing.T) {
	f := newExternalMailOAuthFixture(t)
	account := f.connect(t)
	if _, err := f.a.db.Exec(`UPDATE external_imap_accounts SET oauth_expiry=? WHERE id=?`, f.a.now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), account.ID); err != nil {
		t.Fatal(err)
	}
	f.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "refreshed", "refresh_token": "rotated-refresh", "token_type": "Bearer", "expires_in": 3600})
	}
	if _, err := f.a.externalIMAPOAuthAccessToken(t.Context(), account); err != nil {
		t.Fatal(err)
	}
	current, err := f.a.externalIMAPAccountForUser(t.Context(), f.user.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := f.a.decryptExternalIMAPPassword(current.OAuthRefreshTokenCiphertext)
	if err != nil || refresh != "rotated-refresh" {
		t.Fatal("rotated refresh token lost", err)
	}
}

func TestExternalMailOIDCProductionIssuersAndScopes(t *testing.T) {
	a := &App{now: time.Now}
	for _, tt := range []struct {
		provider, issuer, tid string
		valid                 bool
	}{
		{"gmail", "https://accounts.google.com", "", true}, {"gmail", "accounts.google.com", "", true}, {"gmail", "https://accounts.google.com.attacker.test", "", false},
		{"outlook", "https://login.microsoftonline.com/12345678-abcd-1234-abcd-123456789012/v2.0", "12345678-abcd-1234-abcd-123456789012", true},
		{"outlook", "https://login.microsoftonline.com/common/v2.0", "common", false},
		{"outlook", "https://login.microsoftonline.com/12345678-abcd-1234-abcd-123456789012/v2.0", "87654321-abcd-1234-abcd-123456789012", false},
	} {
		t.Run(tt.provider+tt.issuer+tt.tid, func(t *testing.T) {
			token := (&oauth2.Token{}).WithExtra(map[string]any{"id_token": testIDToken(map[string]any{"iss": tt.issuer, "tid": tt.tid, "aud": "client", "exp": time.Now().Add(time.Hour).Unix(), "sub": "synthetic", "nonce": "nonce", "email": "person@example.test"})})
			_, _, err := a.validateExternalMailIDToken(tt.provider, "", "client", "nonce", token)
			if (err == nil) != tt.valid {
				t.Fatalf("issuer accepted=%t want=%t", err == nil, tt.valid)
			}
		})
	}
	a.cfg.ExternalIMAPOutlookClientID, a.cfg.ExternalIMAPOutlookClientSecret = "synthetic", "synthetic"
	conf, _, err := a.externalIMAPOAuthConfig("outlook")
	if err != nil || !strings.Contains(strings.Join(conf.Scopes, " "), "https://outlook.office.com/SMTP.Send") {
		t.Fatal("SMTP.Send missing", err)
	}
	for _, tt := range []struct {
		provider, scope string
		want            bool
	}{
		{"gmail", "https://mail.google.com/", true}, {"gmail", "openid email", false},
		{"outlook", "https://outlook.office.com/IMAP.AccessAsUser.All https://outlook.office.com/SMTP.Send", true},
		{"outlook", "https://outlook.office.com/IMAP.AccessAsUser.All", false},
	} {
		token := (&oauth2.Token{}).WithExtra(map[string]any{"scope": tt.scope})
		if externalMailOAuthScopeGranted(tt.provider, token) != tt.want {
			t.Fatal("incorrect granted scope validation", tt.provider)
		}
	}
}
