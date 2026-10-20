package imapclient

import (
	"errors"
	"strings"
)

// xoauth2Client implements the XOAUTH2 SASL mechanism (RFC 7628 predecessor
// used by Google and Microsoft IMAP).
type xoauth2Client struct {
	username string
	token    string
}

func (a *xoauth2Client) Start() (string, []byte, error) {
	ir := "user=" + a.username + "\x01auth=Bearer " + a.token + "\x01\x01"
	return "XOAUTH2", []byte(ir), nil
}

func (a *xoauth2Client) Next(challenge []byte) ([]byte, error) {
	msg := strings.TrimSpace(string(challenge))
	if msg == "" {
		return nil, errors.New("xoauth2: authentication rejected")
	}
	return nil, errors.New("xoauth2: authentication rejected: " + msg)
}
