package app

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/oauth2"
)

const externalMailOAuthCookiePath = "/api/external-imap-oauth"
const externalMailOAuthStateTTL = 10 * time.Minute
const externalMailOAuthStateLimit = 5

var errExternalMailReauth = errors.New("reauthorization required")

// Token requests have fixed provider endpoints, a bounded lifetime and no redirects.
// Never use http.DefaultClient: its timeout and transport may be changed elsewhere.
var externalMailOAuthHTTPClient = &http.Client{
	Timeout:       20 * time.Second,
	Transport:     &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func externalMailOAuthContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, externalMailOAuthHTTPClient)
}

func (a *App) externalMailOAuthCookie(w http.ResponseWriter, value string, clear bool) {
	cookie := &http.Cookie{Name: a.cfg.CookieName + "_external_mail_oauth", Value: value,
		Path: externalMailOAuthCookiePath, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: strings.HasPrefix(strings.ToLower(a.cfg.PublicBaseURL), "https://"),
		MaxAge: int(externalMailOAuthStateTTL.Seconds()), Expires: a.now().UTC().Add(externalMailOAuthStateTTL)}
	if clear {
		cookie.MaxAge = -1
		cookie.Expires = time.Unix(1, 0)
	}
	http.SetCookie(w, cookie)
}

func (a *App) handleStartExternalIMAPOAuth(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	provider := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "provider")))
	conf, profile, err := a.externalIMAPOAuthConfig(provider)
	if err != nil {
		badRequest(w, err)
		return
	}
	var req externalIMAPOAuthStartPayload
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	req.AccountID = strings.TrimSpace(req.AccountID)
	if req.AccountID != "" {
		current, err := a.externalIMAPAccountForUser(r.Context(), user.ID, req.AccountID)
		if err != nil {
			respondError(w, http.StatusNotFound, "external account not found")
			return
		}
		if current.AuthMode != externalIMAPAuthOAuth2 || current.OAuthProvider != provider {
			badRequest(w, errors.New("oauth account provider does not match"))
			return
		}
		if strings.TrimSpace(req.MailboxID) == "" {
			req.MailboxID = current.MailboxID
		}
	}
	mb, err := a.mailboxForUserByID(r.Context(), user.ID, strings.TrimSpace(req.MailboxID))
	if err != nil {
		respondError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	email := normalizeEmail(req.Email)
	if email != "" && !strings.Contains(email, "@") {
		badRequest(w, errors.New("invalid oauth email"))
		return
	}
	req.StorageMode = strings.ToLower(strings.TrimSpace(req.StorageMode))
	if req.StorageMode == "" {
		req.StorageMode = externalIMAPStorageLocal
	}
	if req.StorageMode != externalIMAPStorageLocal && req.StorageMode != externalIMAPStorageRemote {
		badRequest(w, errors.New("invalid storage mode"))
		return
	}
	state := externalIMAPOAuthState{Verifier: oauth2.GenerateVerifier(), Nonce: randomToken(), Name: strings.Join(strings.Fields(req.Name), " "), Email: email, StorageMode: req.StorageMode, SyncReadState: true, Enabled: true}
	if req.SyncReadState != nil {
		state.SyncReadState = *req.SyncReadState
	}
	if req.Enabled != nil {
		state.Enabled = *req.Enabled
	}
	if state.Name == "" {
		state.Name = profile.Name
	}
	raw, err := json.Marshal(state)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to start oauth")
		return
	}
	payload, err := a.encryptExternalIMAPPassword(string(raw))
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to start oauth")
		return
	}
	value, binding := randomToken(), randomToken()
	now := a.now().UTC()
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to start oauth")
		return
	}
	defer tx.Rollback()
	if err := a.pruneExternalMailOAuthStatesTx(r.Context(), tx, user.ID, now.Format(time.RFC3339Nano)); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to start oauth")
		return
	}
	_, err = tx.ExecContext(r.Context(), `INSERT INTO external_mail_oauth_states(token_hash,user_id,binding_hash,provider,account_id,mailbox_id,payload_ciphertext,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		hashToken(value), user.ID, hashToken(binding), provider, nullableString(req.AccountID), mb.ID, payload, now.Add(externalMailOAuthStateTTL).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to start oauth")
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to start oauth")
		return
	}
	a.externalMailOAuthCookie(w, binding, false)
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(state.Verifier), oauth2.SetAuthURLParam("nonce", state.Nonce), oauth2.SetAuthURLParam("prompt", "consent")}
	if provider == externalIMAPOAuthGmail {
		opts = append(opts, oauth2.AccessTypeOffline)
	}
	respondJSON(w, http.StatusOK, map[string]any{"url": conf.AuthCodeURL(value, opts...)})
}

func (a *App) pruneExternalMailOAuthStatesTx(ctx context.Context, tx *sql.Tx, userID, now string) error {
	// Serialize pruning and insertion for this user, including concurrent starts.
	if err := a.lockUserQuotaRowTx(ctx, tx, userID); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT token_hash,expires_at FROM external_mail_oauth_states WHERE expires_at<=? OR (user_id=? AND used_at IS NULL) ORDER BY created_at DESC,token_hash DESC`, now, userID)
	if err != nil {
		return err
	}
	var obsolete []string
	count := 0
	for rows.Next() {
		var hash, expiry string
		if err := rows.Scan(&hash, &expiry); err != nil {
			rows.Close()
			return err
		}
		// Delete all expired states and reserve one slot for this user's new state.
		if expiry <= now || count >= externalMailOAuthStateLimit-1 {
			obsolete = append(obsolete, hash)
		}
		if expiry > now {
			count++
		}
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// Selecting keys first avoids MySQL's DELETE subquery restriction and expiry
	// range locks that can deadlock concurrent state inserts.
	for _, hash := range obsolete {
		if _, err := tx.ExecContext(ctx, `DELETE FROM external_mail_oauth_states WHERE token_hash=?`, hash); err != nil {
			return err
		}
	}
	return nil
}

type externalMailOAuthRecord struct {
	UserID, Provider, BindingHash, MailboxID, Payload string
	AccountID                                         sql.NullString
}

func (a *App) consumeExternalMailOAuthState(r *http.Request, provider string) (externalMailOAuthRecord, externalIMAPOAuthState, error) {
	var record externalMailOAuthRecord
	var payload externalIMAPOAuthState
	value := r.URL.Query().Get("state")
	if value == "" || len(value) > 256 {
		return record, payload, errors.New("invalid state")
	}
	hash := hashToken(value)
	now := a.now().UTC().Format(time.RFC3339Nano)
	result, err := a.db.ExecContext(r.Context(), `UPDATE external_mail_oauth_states SET used_at=? WHERE token_hash=? AND used_at IS NULL AND expires_at>?`, now, hash, now)
	if err != nil {
		return record, payload, err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return record, payload, errors.New("invalid state")
	}
	err = a.db.QueryRowContext(r.Context(), `SELECT user_id,binding_hash,provider,account_id,mailbox_id,payload_ciphertext FROM external_mail_oauth_states WHERE token_hash=?`, hash).Scan(&record.UserID, &record.BindingHash, &record.Provider, &record.AccountID, &record.MailboxID, &record.Payload)
	if err != nil {
		return record, payload, err
	}
	cookie, err := r.Cookie(a.cfg.CookieName + "_external_mail_oauth")
	if err != nil || cookie.Value == "" || hashToken(cookie.Value) != record.BindingHash || record.Provider != provider {
		return record, payload, errors.New("invalid state")
	}
	plain, err := a.decryptExternalIMAPPassword(record.Payload)
	if err != nil {
		return record, payload, err
	}
	err = json.Unmarshal([]byte(plain), &payload)
	if err == nil && (payload.Verifier == "" || payload.Nonce == "") {
		err = errors.New("invalid state")
	}
	return record, payload, err
}

func (a *App) handleExternalIMAPOAuthCallback(w http.ResponseWriter, r *http.Request) {
	result := "failed"
	defer func() {
		a.externalMailOAuthCookie(w, "", true)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		http.Redirect(w, r, strings.TrimRight(a.cfg.PublicBaseURL, "/")+"/profile?tab=mailboxes&externalMail="+result, http.StatusFound)
	}()
	provider := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "provider")))
	record, state, err := a.consumeExternalMailOAuthState(r, provider)
	if err != nil {
		result = "invalid_state"
		return
	}
	if r.URL.Query().Get("error") != "" {
		result = "denied"
		return
	}
	if !a.cfg.ExternalIMAPEnabled {
		result = "not_configured"
		return
	}
	conf, profile, err := a.externalIMAPOAuthConfig(provider)
	if err != nil {
		result = "not_configured"
		return
	}
	// The start request was authorized, but access may have been revoked while
	// consent was open. Re-check before exchanging the code.
	owner, err := a.userByID(r.Context(), record.UserID)
	if err != nil || owner.Disabled || !userHasPermission(owner, PermissionMailAccess) {
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" || len(code) > 8192 || strings.ContainsAny(code, "\r\n\x00") {
		result = "exchange_failed"
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	token, err := conf.Exchange(externalMailOAuthContext(ctx), code, oauth2.VerifierOption(state.Verifier))
	if err != nil || !validExternalMailOAuthToken(token, a.now()) {
		result = "exchange_failed"
		return
	}
	subject, email, err := a.validateExternalMailIDToken(provider, profile.Issuer, conf.ClientID, state.Nonce, token)
	if err != nil || !externalMailOAuthScopeGranted(provider, token) {
		result = "exchange_failed"
		return
	}
	var reauthAccount externalIMAPAccountRecord
	if record.AccountID.Valid {
		reauthAccount, err = a.externalIMAPAccountForUser(ctx, record.UserID, record.AccountID.String)
		if err != nil || reauthAccount.AuthMode != externalIMAPAuthOAuth2 || reauthAccount.OAuthProvider != provider {
			result = "account_mismatch"
			return
		}
		if reauthAccount.OAuthSubject != "" && reauthAccount.OAuthSubject != subject || reauthAccount.OAuthSubject == "" && !strings.EqualFold(reauthAccount.OAuthEmail, email) {
			result = "account_mismatch"
			return
		}
	}
	if state.Email != "" && !strings.EqualFold(state.Email, email) {
		result = "email_mismatch"
		return
	}
	// A mailbox may have been removed or transferred while consent was open.
	if _, err := a.mailboxForUserByID(ctx, record.UserID, record.MailboxID); err != nil {
		return
	}
	accessCipher, err := a.encryptExternalIMAPPassword(token.AccessToken)
	if err != nil {
		return
	}
	refreshCipher := ""
	if token.RefreshToken != "" {
		refreshCipher, err = a.encryptExternalIMAPPassword(token.RefreshToken)
		if err != nil {
			return
		}
	}
	smtpHost, smtpPort, smtpMode := externalOAuthSMTPEndpoint(provider)
	now, expiry := a.now().UTC().Format(time.RFC3339Nano), externalMailTokenExpiry(token)
	if record.AccountID.Valid {
		account := reauthAccount
		if refreshCipher == "" {
			refreshCipher = account.OAuthRefreshTokenCiphertext
		}
		// Prevent a callback racing with another successful reauthorization from reverting it.
		res, err := a.db.ExecContext(ctx, `UPDATE external_imap_accounts SET oauth_access_token_ciphertext=?,oauth_refresh_token_ciphertext=?,oauth_expiry=?,oauth_subject=?,oauth_generation=oauth_generation+1,oauth_email=?,username=?,provider_preset=?,smtp_host=?,smtp_port=?,smtp_tls_mode=?,smtp_username=?,auth_status='ok',last_error='',updated_at=? WHERE id=? AND user_id=? AND auth_mode='oauth2' AND oauth_provider=? AND oauth_generation=?`,
			accessCipher, refreshCipher, expiry, subject, email, email, provider, smtpHost, smtpPort, smtpMode, email, now, account.ID, record.UserID, provider, account.OAuthGeneration)
		if err != nil {
			return
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return
		}
		result = "reauthorized"
		return
	}
	_, err = a.db.ExecContext(ctx, `INSERT INTO external_imap_accounts(id,user_id,mailbox_id,name,host,port,tls_mode,username,password_ciphertext,auth_mode,oauth_provider,oauth_email,oauth_access_token_ciphertext,oauth_refresh_token_ciphertext,oauth_expiry,storage_mode,sync_read_state,enabled,last_status,created_at,updated_at,oauth_subject,provider_preset,smtp_host,smtp_port,smtp_tls_mode,smtp_username) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		newID("ximap"), record.UserID, record.MailboxID, state.Name, profile.Host, profile.Port, profile.TLSMode, email, "", externalIMAPAuthOAuth2, provider, email, accessCipher, refreshCipher, expiry, state.StorageMode, boolInt(state.SyncReadState), boolInt(state.Enabled), "idle", now, now, subject, provider, smtpHost, smtpPort, smtpMode, email)
	if err == nil {
		result = "connected"
	}
}

func validExternalMailOAuthToken(token *oauth2.Token, now time.Time) bool {
	return token != nil && strings.EqualFold(token.TokenType, "Bearer") && strings.TrimSpace(token.AccessToken) != "" && (token.Expiry.IsZero() || token.Expiry.After(now))
}

func externalMailTokenExpiry(token *oauth2.Token) string {
	if token.Expiry.IsZero() {
		return ""
	}
	return token.Expiry.UTC().Format(time.RFC3339Nano)
}

func externalMailOAuthScopeGranted(provider string, token *oauth2.Token) bool {
	raw, _ := token.Extra("scope").(string)
	// RFC 6749 permits omission when the granted scope equals the requested scope.
	if raw == "" {
		return true
	}
	scopes := map[string]bool{}
	for _, scope := range strings.Fields(raw) {
		scopes[scope] = true
	}
	if provider == externalIMAPOAuthGmail {
		return scopes["https://mail.google.com/"]
	}
	return scopes["https://outlook.office.com/IMAP.AccessAsUser.All"] && scopes["https://outlook.office.com/SMTP.Send"]
}

var externalMailTenantID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (a *App) validateExternalMailIDToken(provider, testIssuer, clientID, nonce string, token *oauth2.Token) (string, string, error) {
	idToken, _ := token.Extra("id_token").(string)
	claims, err := externalIMAPOIDCClaims(idToken)
	if err != nil {
		return "", "", err
	}
	iss, _ := claims["iss"].(string)
	issuerOK := false
	if testIssuer != "" {
		issuerOK = iss == testIssuer
	} else {
		switch provider {
		case externalIMAPOAuthGmail:
			issuerOK = iss == "https://accounts.google.com" || iss == "accounts.google.com"
		case externalIMAPOAuthOutlook:
			tid, _ := claims["tid"].(string)
			issuerOK = externalMailTenantID.MatchString(tid) && iss == "https://login.microsoftonline.com/"+tid+"/v2.0"
		}
	}
	aud, _ := claims["aud"].(string)
	if audiences, ok := claims["aud"].([]any); ok && len(audiences) == 1 {
		aud, _ = audiences[0].(string)
	}
	exp, _ := claims["exp"].(float64)
	gotNonce, _ := claims["nonce"].(string)
	sub, _ := claims["sub"].(string)
	if !issuerOK || aud != clientID || exp <= float64(a.now().Unix()) || gotNonce != nonce || sub == "" {
		return "", "", errors.New("invalid id token claims")
	}
	// OIDC Core 3.1.3.7 permits relying on TLS validation instead of checking the
	// signature for an ID token received directly from the authenticated token
	// endpoint. Production endpoints above are fixed HTTPS URLs, never browser input.
	email, err := externalIMAPOAuthEmail(provider, token)
	return sub, email, err
}

func (a *App) externalIMAPOAuthAccessToken(ctx context.Context, account externalIMAPAccountRecord) (string, error) {
	value, err, _ := a.externalIMAPOAuthRefresh.Do(account.ID, func() (any, error) {
		// The caller's record can predate a refresh or reauthorization. Never base a
		// token exchange or CAS on that snapshot.
		current, err := a.externalIMAPAccountByIDIncludingDisabled(ctx, account.ID)
		if err != nil {
			return "", err
		}
		if current.AuthStatus == "needs_reauth" {
			return "", errExternalMailReauth
		}
		access, err := a.decryptExternalIMAPPassword(current.OAuthAccessTokenCiphertext)
		if err != nil {
			return "", errors.New("oauth token unavailable")
		}
		if current.OAuthExpiry.IsZero() || current.OAuthExpiry.After(a.now().Add(2*time.Minute)) {
			return access, nil
		}
		if current.OAuthRefreshTokenCiphertext == "" {
			return "", errExternalMailReauth
		}
		refresh, err := a.decryptExternalIMAPPassword(current.OAuthRefreshTokenCiphertext)
		if err != nil {
			return "", errors.New("oauth token unavailable")
		}
		conf, _, err := a.externalIMAPOAuthConfig(current.OAuthProvider)
		if err != nil {
			return "", err
		}
		refreshCtx, cancel := context.WithTimeout(externalMailOAuthContext(ctx), 20*time.Second)
		defer cancel()
		// Force a refresh inside the early-refresh window, independent of oauth2's
		// smaller built-in expiry skew.
		token, err := conf.TokenSource(refreshCtx, &oauth2.Token{RefreshToken: refresh}).Token()
		if err != nil {
			var retrieve *oauth2.RetrieveError
			if errors.As(err, &retrieve) && retrieve.ErrorCode == "invalid_grant" {
				latest, readErr := a.externalIMAPAccountByIDIncludingDisabled(ctx, current.ID)
				if readErr != nil {
					return "", readErr
				}
				if latest.OAuthGeneration != current.OAuthGeneration || latest.OAuthRefreshTokenCiphertext != current.OAuthRefreshTokenCiphertext {
					return a.externalMailStoredAccessToken(latest)
				}
				res, updateErr := a.db.ExecContext(ctx, `UPDATE external_imap_accounts SET auth_status='needs_reauth',last_status='error',last_error='reauthorization required',updated_at=? WHERE id=? AND oauth_generation=? AND oauth_refresh_token_ciphertext=?`, a.now().UTC().Format(time.RFC3339Nano), current.ID, current.OAuthGeneration, current.OAuthRefreshTokenCiphertext)
				if updateErr != nil {
					return "", updateErr
				}
				n, updateErr := res.RowsAffected()
				if updateErr != nil {
					return "", updateErr
				}
				if n == 0 {
					latest, err = a.externalIMAPAccountByIDIncludingDisabled(ctx, current.ID)
					if err != nil {
						return "", err
					}
					return a.externalMailStoredAccessToken(latest)
				}
				return "", errExternalMailReauth
			}
			return "", errors.New("oauth token refresh failed")
		}
		if !validExternalMailOAuthToken(token, a.now()) {
			return "", errors.New("invalid oauth token response")
		}
		accessCipher, err := a.encryptExternalIMAPPassword(token.AccessToken)
		if err != nil {
			return "", err
		}
		refreshCipher := current.OAuthRefreshTokenCiphertext
		if token.RefreshToken != "" && token.RefreshToken != refresh {
			refreshCipher, err = a.encryptExternalIMAPPassword(token.RefreshToken)
			if err != nil {
				return "", err
			}
		}
		res, err := a.db.ExecContext(ctx, `UPDATE external_imap_accounts SET oauth_access_token_ciphertext=?,oauth_refresh_token_ciphertext=?,oauth_expiry=?,updated_at=? WHERE id=? AND oauth_generation=? AND oauth_refresh_token_ciphertext=?`, accessCipher, refreshCipher, externalMailTokenExpiry(token), a.now().UTC().Format(time.RFC3339Nano), current.ID, current.OAuthGeneration, current.OAuthRefreshTokenCiphertext)
		if err != nil {
			return "", err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return "", err
		}
		if n == 0 {
			latest, err := a.externalIMAPAccountByIDIncludingDisabled(ctx, current.ID)
			if err != nil {
				return "", err
			}
			return a.externalMailStoredAccessToken(latest)
		}
		return token.AccessToken, nil
	})
	if err != nil {
		return "", err
	}
	return value.(string), nil
}

func (a *App) externalMailStoredAccessToken(account externalIMAPAccountRecord) (string, error) {
	if account.AuthStatus == "needs_reauth" {
		return "", errExternalMailReauth
	}
	token, err := a.decryptExternalIMAPPassword(account.OAuthAccessTokenCiphertext)
	if err != nil || token == "" {
		return "", errors.New("oauth token unavailable")
	}
	return token, nil
}
