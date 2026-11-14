package app

import (
	"errors"
	"net/http"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// ExternalMailProvider contains public preset metadata only. Credentials never
// enter the registry or its API response. Presets do not override password endpoints.
type ExternalMailProvider struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	AuthMethods     []string `json:"authMethods"`
	OAuthProvider   string   `json:"oauthProvider"`
	OAuthConfigured bool     `json:"oauthConfigured"`
	IMAPHost        string   `json:"imapHost"`
	IMAPPort        int      `json:"imapPort"`
	IMAPTLSMode     string   `json:"imapTlsMode"`
	SMTPHost        string   `json:"smtpHost"`
	SMTPPort        int      `json:"smtpPort"`
	SMTPTLSMode     string   `json:"smtpTlsMode"`
	SentCopyMode    string   `json:"sentCopyMode"`
	RequiresIMAPID  bool     `json:"requiresImapId"`
}

// The append defaults below (Yahoo/iCloud/QQ/163/custom) await manual account
// verification. Gmail and Outlook rely on the server's own Sent copy.
var externalMailProviders = []ExternalMailProvider{
	// https://developers.google.com/workspace/gmail/imap/imap-smtp
	{ID: "gmail", Name: "Gmail", AuthMethods: []string{"oauth2", "app_password"}, OAuthProvider: "gmail", IMAPHost: "imap.gmail.com", IMAPPort: 993, IMAPTLSMode: "tls", SMTPHost: "smtp.gmail.com", SMTPPort: 465, SMTPTLSMode: "tls", SentCopyMode: "server"},
	// Exchange Online endpoints; personal Outlook.com compatibility still needs live verification.
	// https://learn.microsoft.com/en-us/exchange/clients-and-mobile-in-exchange-online/pop3-and-imap4/pop3-and-imap4
	{ID: "outlook", Name: "Outlook", AuthMethods: []string{"oauth2"}, OAuthProvider: "outlook", IMAPHost: "outlook.office365.com", IMAPPort: 993, IMAPTLSMode: "tls", SMTPHost: "smtp.office365.com", SMTPPort: 587, SMTPTLSMode: "starttls", SentCopyMode: "server"},
	// https://help.yahoo.com/kb/imap-settings-sln4075.html
	{ID: "yahoo", Name: "Yahoo Mail", AuthMethods: []string{"app_password"}, IMAPHost: "imap.mail.yahoo.com", IMAPPort: 993, IMAPTLSMode: "tls", SMTPHost: "smtp.mail.yahoo.com", SMTPPort: 465, SMTPTLSMode: "tls", SentCopyMode: "append"},
	// https://support.apple.com/en-ie/102525
	{ID: "icloud", Name: "iCloud Mail", AuthMethods: []string{"app_password"}, IMAPHost: "imap.mail.me.com", IMAPPort: 993, IMAPTLSMode: "tls", SMTPHost: "smtp.mail.me.com", SMTPPort: 587, SMTPTLSMode: "starttls", SentCopyMode: "append"},
	// https://help.mail.qq.com/detail/108/1089
	{ID: "qq", Name: "QQ Mail", AuthMethods: []string{"app_password"}, IMAPHost: "imap.qq.com", IMAPPort: 993, IMAPTLSMode: "tls", SMTPHost: "smtp.qq.com", SMTPPort: 465, SMTPTLSMode: "tls", SentCopyMode: "append"},
	// https://help.mail.163.com/faqDetail.do?code=d7a5dc8471cd0c0e8b4b8f4f8e49998b374173cfe9171305fa1ce630d7f67ac22b85ac2e7c90cd63
	{ID: "163", Name: "163 Mail", AuthMethods: []string{"app_password"}, IMAPHost: "imap.163.com", IMAPPort: 993, IMAPTLSMode: "tls", SMTPHost: "smtp.163.com", SMTPPort: 465, SMTPTLSMode: "tls", SentCopyMode: "append", RequiresIMAPID: true},
	// app_password includes ordinary passwords for custom servers; SMTP always uses TLS.
	{ID: "custom", Name: "Custom", AuthMethods: []string{"app_password"}, IMAPTLSMode: "tls", SMTPTLSMode: "tls", SentCopyMode: "append"},
}

func externalMailProviderByID(id string) (ExternalMailProvider, bool) {
	for _, provider := range externalMailProviders {
		if provider.ID == id {
			return provider, true
		}
	}
	return ExternalMailProvider{}, false
}

func normalizeExternalMailProvider(id string) (string, error) {
	if id == "" {
		id = "custom"
	}
	if _, ok := externalMailProviderByID(id); !ok {
		return "", errors.New("unknown mail provider")
	}
	return id, nil
}

func (a *App) handleExternalMailProviders(w http.ResponseWriter, r *http.Request) {
	items := make([]ExternalMailProvider, 0, len(externalMailProviders))
	for _, provider := range externalMailProviders {
		switch provider.OAuthProvider {
		case "gmail":
			provider.OAuthConfigured = a.cfg.ExternalIMAPGmailClientID != "" && a.cfg.ExternalIMAPGmailClientSecret != ""
		case "outlook":
			provider.OAuthConfigured = a.cfg.ExternalIMAPOutlookClientID != "" && a.cfg.ExternalIMAPOutlookClientSecret != ""
		}
		items = append(items, provider)
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items})
}

func externalMailRequiresIMAPID(account ExternalIMAPAccount) bool {
	if provider, ok := externalMailProviderByID(account.ProviderPreset); ok && provider.RequiresIMAPID {
		return true
	}
	switch strings.ToLower(strings.TrimSuffix(account.Host, ".")) {
	case "imap.163.com", "imap.126.com", "imap.yeah.net":
		return true
	}
	return false
}

// go-imap v2.0.0-beta.8 implements RFC 2971 natively. Send only product metadata,
// after authentication, and never transmit usernames, addresses or environment data.
func (a *App) sendExternalIMAPID(c *imapclient.Client, account ExternalIMAPAccount) {
	if !externalMailRequiresIMAPID(account) || !c.Caps().Has(imap.CapID) {
		return
	}
	if _, err := c.ID(&imap.IDData{Name: "LanQin", Vendor: "LanQin", Version: "1"}).Wait(); err != nil {
		a.log.Warn("external imap ID failed", "account_id", account.ID)
	}
}
