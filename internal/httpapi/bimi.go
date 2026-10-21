package httpapi

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const (
	bimiMaxBytes = 512 << 10
	bimiSelector = "default"
)

var errBIMIBlockedAddress = errors.New("blocked BIMI address")

var (
	bimiLookupTXT = func(ctx context.Context, name string) ([]string, error) {
		return net.DefaultResolver.LookupTXT(ctx, name)
	}
	bimiClient = newBIMIClient()
)

func newBIMIClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 3 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, port, err := net.SplitHostPort(address)
			if err != nil || port != "443" {
				return errBIMIBlockedAddress
			}
			ip := net.ParseIP(host)
			if ip == nil || bimiBlockedIP(ip) {
				return errBIMIBlockedAddress
			}
			return nil
		},
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   4 * time.Second,
		ResponseHeaderTimeout: 4 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          4,
	}
	return &http.Client{
		Timeout:   8 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many BIMI redirects")
			}
			if req.URL.Scheme != "https" {
				return errors.New("BIMI redirect must use https")
			}
			return nil
		},
	}
}

// bimiBlockedIP rejects addresses that could reach the host or private
// networks. The fake-ip ranges used by the local transparent proxy
// (198.18.0.0/15 and 2001:2::/48) are deliberately allowed.
func bimiBlockedIP(ip net.IP) bool {
	if ip == nil ||
		ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsUnspecified() ||
		ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() {
		return true
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	switch {
	case v4[0] == 0 || v4[0] == 255:
		return true
	case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127:
		return true
	case v4[0] == 192 && v4[1] == 0 && v4[2] == 0:
		return true
	case v4[0] == 192 && v4[1] == 0 && v4[2] == 2:
		return true
	case v4[0] == 198 && v4[1] == 51 && v4[2] == 100:
		return true
	case v4[0] == 203 && v4[1] == 0 && v4[2] == 113:
		return true
	}
	return false
}

func fetchBIMIAvatar(ctx context.Context, domain string) (*avatarImage, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	rawURL, err := lookupBIMILogo(lookupCtx, domain)
	cancel()
	if err != nil || rawURL == "" {
		return nil, err
	}

	endpoint, err := bimiLogoRequestURL(rawURL)
	if err != nil {
		return nil, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := bimiClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("BIMI logo status %d", response.StatusCode)
	}
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || contentType != "image/svg+xml" {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, bimiMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > bimiMaxBytes || !validBIMISVG(data) {
		return nil, nil
	}
	return &avatarImage{data: data, contentType: "image/svg+xml"}, nil
}

func lookupBIMILogo(ctx context.Context, domain string) (string, error) {
	records, err := bimiLookupTXT(ctx, bimiSelector+"._bimi."+domain)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return "", nil
		}
		return "", err
	}
	for _, record := range records {
		if logo := parseBIMIRecord(record); logo != "" {
			return logo, nil
		}
	}
	return "", nil
}

func parseBIMIRecord(record string) string {
	version := ""
	logo := ""
	for _, field := range strings.Split(record, ";") {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "v":
			version = strings.TrimSpace(value)
		case "l":
			logo = strings.TrimSpace(value)
		}
	}
	if !strings.EqualFold(version, "BIMI1") {
		return ""
	}
	return logo
}

func bimiLogoRequestURL(raw string) (*url.URL, error) {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(endpoint.Scheme, "https") || endpoint.Hostname() == "" || endpoint.User != nil {
		return nil, errors.New("invalid BIMI logo URL")
	}
	return endpoint, nil
}

func validBIMISVG(data []byte) bool {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	rootSeen := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return rootSeen
		}
		if err != nil {
			return false
		}
		switch value := token.(type) {
		case xml.StartElement:
			name := strings.ToLower(value.Name.Local)
			if !rootSeen {
				if name != "svg" {
					return false
				}
				rootSeen = true
			}
			switch name {
			case "script", "foreignobject", "iframe", "object", "embed", "audio", "video",
				"animate", "animatetransform", "animatemotion", "set", "handler":
				return false
			}
			for _, attr := range value.Attr {
				attrName := strings.ToLower(attr.Name.Local)
				if strings.HasPrefix(attrName, "on") || attrName == "base" {
					return false
				}
				if attrName == "href" && !safeSVGReference(attr.Value) {
					return false
				}
				if attrName == "style" && unsafeSVGStyle(attr.Value) {
					return false
				}
			}
		case xml.CharData:
			if unsafeSVGStyle(string(value)) {
				return false
			}
		case xml.Directive:
			if strings.Contains(strings.ToUpper(string(value)), "DOCTYPE") {
				return false
			}
		}
	}
}

func safeSVGReference(value string) bool {
	reference := strings.TrimSpace(value)
	if reference == "" || strings.HasPrefix(reference, "#") {
		return true
	}
	lower := strings.ToLower(reference)
	for _, prefix := range []string{"data:image/png", "data:image/jpeg", "data:image/gif", "data:image/webp"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func unsafeSVGStyle(value string) bool {
	compact := strings.ToLower(strings.Join(strings.Fields(value), ""))
	compact = strings.NewReplacer(`"`, "", `'`, "").Replace(compact)
	for _, token := range []string{
		"@import", "expression(", "javascript:", "url(http", "url(https", "url(//",
		"data:image/svg", "-moz-binding", "behavior:",
	} {
		if strings.Contains(compact, token) {
			return true
		}
	}
	return false
}
