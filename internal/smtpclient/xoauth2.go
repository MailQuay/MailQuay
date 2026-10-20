package smtpclient

import "errors"

// xoauth2Client implements the XOAUTH2 SASL mechanism used by Google and
// Microsoft SMTP submission.
type xoauth2Client struct {
	username string
	token    string
}

func (a *xoauth2Client) Start() (string, []byte, error) {
	ir := "user=" + a.username + "\x01auth=Bearer " + a.token + "\x01\x01"
	return "XOAUTH2", []byte(ir), nil
}

func (a *xoauth2Client) Next(challenge []byte) ([]byte, error) {
	return nil, errors.New("xoauth2: authentication rejected")
}
