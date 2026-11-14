package app

import "github.com/emersion/go-sasl"

// Shared by IMAP and SMTP. The initial response is deliberately not base64:
// each protocol client applies its own SASL framing.
type externalMailXOAUTH2Client struct {
	username string
	token    string
}

var _ sasl.Client = externalMailXOAUTH2Client{}

func newExternalMailXOAUTH2Client(username, token string) sasl.Client {
	return externalMailXOAUTH2Client{username: username, token: token}
}

func (c externalMailXOAUTH2Client) Start() (string, []byte, error) {
	return "XOAUTH2", []byte("user=" + c.username + "\x01auth=Bearer " + c.token + "\x01\x01"), nil
}

func (c externalMailXOAUTH2Client) Next(challenge []byte) ([]byte, error) {
	// XOAUTH2 sends a JSON challenge on failure. Acknowledge it with an empty
	// response so the server can send its final authentication error (aerc behavior).
	return []byte{}, nil
}
