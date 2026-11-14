package app

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"
)

type externalSMTPDialFunc func(context.Context, string, string) (net.Conn, error)

var errExternalSender = errors.New("external account cannot send")

func externalSendAddress(account ExternalIMAPAccount) string {
	candidates := []string{account.SMTPUsername, account.Username}
	if account.AuthMode == externalIMAPAuthOAuth2 {
		candidates = []string{account.OAuthEmail}
	}
	for _, value := range candidates {
		address, err := mail.ParseAddress(value)
		if err == nil && address.Address == value && strings.Contains(value, "@") && !strings.ContainsAny(value, "\r\n") {
			return normalizeEmail(value)
		}
	}
	return ""
}

func externalAccountCanSend(account ExternalIMAPAccount) bool {
	return account.Enabled && account.SendEnabled && account.AuthStatus == "ok" && externalSendAddress(account) != "" && account.SMTPHost != "" && account.SMTPPort > 0 && account.SMTPPort <= 65535 && (account.SMTPTLSMode == externalIMAPTLS || account.SMTPTLSMode == externalIMAPStartTLS)
}

func externalOAuthSMTPEndpoint(provider string) (string, int, string) {
	preset, ok := externalMailProviderByID(provider)
	if !ok || preset.OAuthProvider == "" {
		return "", 0, ""
	}
	return preset.SMTPHost, preset.SMTPPort, preset.SMTPTLSMode
}

func (a *App) normalizeExternalSMTPPayload(ctx context.Context, req *externalIMAPPayload) error {
	if req.SendEnabled == nil {
		v := false
		req.SendEnabled = &v
	}
	req.SMTPHost = strings.ToLower(strings.TrimSpace(req.SMTPHost))
	req.SMTPUsername = strings.TrimSpace(req.SMTPUsername)
	req.SMTPTLSMode = strings.ToLower(strings.TrimSpace(req.SMTPTLSMode))
	if req.SentCopyMode == "" {
		preset, _ := externalMailProviderByID(req.ProviderPreset)
		req.SentCopyMode = preset.SentCopyMode
	}
	if req.SentCopyMode != "server" && req.SentCopyMode != "append" && req.SentCopyMode != "none" {
		return errors.New("invalid sent copy mode")
	}
	if req.SMTPTLSMode != "" && req.SMTPTLSMode != externalIMAPTLS && req.SMTPTLSMode != externalIMAPStartTLS {
		return errors.New("external smtp requires tls")
	}
	if *req.SendEnabled {
		if req.SMTPHost == "" || req.SMTPPort < 1 || req.SMTPPort > 65535 || req.SMTPTLSMode == "" {
			return errors.New("external smtp settings are incomplete")
		}
		if externalSendAddress(ExternalIMAPAccount{SMTPUsername: req.SMTPUsername, Username: req.Username}) == "" {
			return errors.New("external send address is invalid")
		}
	}
	if req.SMTPHost != "" {
		return a.validateExternalIMAPHost(ctx, req.SMTPHost)
	}
	return nil
}

func (a *App) externalSMTPPasswordCipher(password, existing string) (string, error) {
	if strings.TrimSpace(password) == "" {
		return existing, nil
	}
	return a.encryptExternalIMAPPassword(password)
}

func (a *App) authorizedExternalSender(ctx context.Context, user *User, mb *Mailbox, id string) (externalIMAPAccountRecord, error) {
	if !a.cfg.ExternalIMAPEnabled || user == nil || mb == nil || mb.UserID != user.ID || mb.Status != "active" || user.Disabled {
		return externalIMAPAccountRecord{}, errExternalSender
	}
	account, err := a.externalIMAPAccountForUser(ctx, user.ID, strings.TrimSpace(id))
	if err != nil || account.MailboxID != mb.ID || !account.CanSend {
		return externalIMAPAccountRecord{}, errExternalSender
	}
	if account.AuthMode == externalIMAPAuthOAuth2 {
		host, port, mode := externalOAuthSMTPEndpoint(account.OAuthProvider)
		if account.SMTPHost != host || account.SMTPPort != port || account.SMTPTLSMode != mode {
			return externalIMAPAccountRecord{}, errExternalSender
		}
	}
	return account, nil
}

type externalSMTPError struct {
	code                   string
	permanent, unconfirmed bool
}

func (e *externalSMTPError) Error() string { return e.code }

// Never persist raw responses: they can contain addresses, tokens or server internals.
func classifyExternalSMTPError(err error, auth, afterData bool) *externalSMTPError {
	var response *smtp.SMTPError
	if errors.As(err, &response) {
		if auth && response.Code == 535 && (response.EnhancedCode == (smtp.EnhancedCode{5, 7, 139}) || strings.Contains(strings.ToLower(response.Message), "smtpclientauthentication is disabled")) {
			return &externalSMTPError{code: "external_smtp_auth_disabled", permanent: true}
		}
		if response.Code >= 500 {
			code := "external_smtp_rejected"
			if auth {
				code = "external_smtp_auth_failed"
			}
			return &externalSMTPError{code: code, permanent: true}
		}
		return &externalSMTPError{code: "external_smtp_temporary"}
	}
	if afterData {
		return &externalSMTPError{code: "external_smtp_unconfirmed", unconfirmed: true}
	}
	return &externalSMTPError{code: "external_smtp_temporary"}
}

func (a *App) sendViaExternalSMTP(ctx context.Context, item sendQueueItem) (string, error) {
	user, err := a.userByID(ctx, item.UserID)
	if err != nil || !userHasPermission(user, PermissionMailSend) {
		return "", &externalSMTPError{code: "external_account_unavailable", permanent: true}
	}
	mb, err := a.mailboxByID(ctx, item.MailboxID)
	if err != nil {
		return "", &externalSMTPError{code: "external_account_unavailable", permanent: true}
	}
	account, err := a.authorizedExternalSender(ctx, user, mb, item.ExternalAccountID)
	if err != nil {
		// Preserve the actionable refresh error on subsequent attempts as well.
		current, loadErr := a.externalIMAPAccountForUser(ctx, item.UserID, item.ExternalAccountID)
		if loadErr == nil && current.AuthStatus == "needs_reauth" {
			return "", &externalSMTPError{code: "external_reauthorization_required", permanent: true}
		}
		return "", &externalSMTPError{code: "external_account_unavailable", permanent: true}
	}
	// Queued identities must not silently change when an account is edited.
	if item.MailFrom != account.SendAddress || item.HeaderFrom != account.SendAddress {
		return "", &externalSMTPError{code: "external_account_unavailable", permanent: true}
	}
	if err := a.validateExternalIMAPHost(ctx, account.SMTPHost); err != nil {
		return "", &externalSMTPError{code: "external_account_unavailable", permanent: true}
	}
	ctx, cancel := context.WithTimeout(ctx, smtpSessionTimeout)
	defer cancel()
	tlsConfig := &tls.Config{ServerName: account.SMTPHost, MinVersion: tls.VersionTLS12, RootCAs: a.externalSMTPRootCAsForTest}
	address := net.JoinHostPort(account.SMTPHost, strconv.Itoa(account.SMTPPort))
	dial := a.externalMailDialer().DialContext
	if a.externalSMTPDialForTest != nil {
		dial = a.externalSMTPDialForTest
	}
	conn, err := dial(ctx, "tcp", address)
	if err != nil {
		return "", classifyExternalSMTPError(err, false, false)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	var client *smtp.Client
	if account.SMTPTLSMode == externalIMAPTLS {
		secured := tls.Client(conn, tlsConfig)
		if err := secured.HandshakeContext(ctx); err != nil {
			return "", classifyExternalSMTPError(err, false, false)
		}
		client = smtp.NewClient(secured)
	} else {
		client, err = smtp.NewClientStartTLS(conn, tlsConfig)
		if err != nil {
			return "", classifyExternalSMTPError(err, false, false)
		}
	}
	defer client.Close()
	client.CommandTimeout = 15 * time.Second
	client.SubmissionTimeout = 30 * time.Second
	if err := client.Hello("localhost"); err != nil {
		return "", classifyExternalSMTPError(err, false, false)
	}
	var auth sasl.Client
	username := account.SMTPUsername
	if username == "" {
		username = account.Username
	}
	if account.AuthMode == externalIMAPAuthOAuth2 {
		token, err := a.externalIMAPOAuthAccessToken(ctx, account)
		if err != nil {
			if errors.Is(err, errExternalMailReauth) {
				return "", &externalSMTPError{code: "external_reauthorization_required", permanent: true}
			}
			return "", &externalSMTPError{code: "external_smtp_temporary"}
		}
		auth = newExternalMailXOAUTH2Client(account.OAuthEmail, token)
	} else {
		ciphertext := account.SMTPPasswordCiphertext
		if ciphertext == "" {
			ciphertext = account.PasswordCiphertext
		}
		password, err := a.decryptExternalIMAPPassword(ciphertext)
		if err != nil {
			return "", &externalSMTPError{code: "external_smtp_auth_failed", permanent: true}
		}
		if client.SupportsAuth(sasl.Plain) {
			auth = sasl.NewPlainClient("", username, password)
		} else if client.SupportsAuth(sasl.Login) {
			auth = sasl.NewLoginClient(username, password)
		} else {
			return "", &externalSMTPError{code: "external_smtp_auth_failed", permanent: true}
		}
	}
	if err := client.Auth(auth); err != nil {
		return "", classifyExternalSMTPError(err, true, false)
	}
	if err := client.Mail(account.SendAddress, nil); err != nil {
		return "", classifyExternalSMTPError(err, false, false)
	}
	for _, recipient := range item.Recipients {
		if err := client.Rcpt(recipient, nil); err != nil {
			return "", classifyExternalSMTPError(err, false, false)
		}
	}
	data, err := client.Data()
	if err != nil {
		return "", classifyExternalSMTPError(err, false, false)
	}
	if _, err := data.Write(item.MIMEBytes); err != nil {
		return "", classifyExternalSMTPError(err, false, true)
	}
	if err := data.Close(); err != nil {
		return "", classifyExternalSMTPError(err, false, true)
	}
	// DATA was accepted. QUIT is housekeeping and cannot change that outcome.
	_ = client.Quit()
	return account.SentCopyMode, nil
}

func (c *goExternalIMAPClient) Append(ctx context.Context, folder string, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.client.Close() })
	defer stop()
	command := c.client.Append(folder, int64(len(raw)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagSeen}, Time: time.Now().UTC()})
	if _, err := command.Write(raw); err != nil {
		return err
	}
	if err := command.Close(); err != nil {
		return err
	}
	_, err := command.Wait()
	return err
}
