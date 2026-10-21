package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mailquay/mailquay/internal/httpapi/oapi"
	"github.com/mailquay/mailquay/internal/settings"
)

const (
	avatarMaxBytes    = 1 << 20
	avatarDefaultSize = 80
	avatarMinSize     = 16
	avatarMaxSize     = 256
)

var (
	gravatarUpstream = "https://gravatar.com/avatar"
	faviconUpstream  = "https://t3.gstatic.cn/faviconV2"
	avatarClient     = &http.Client{Timeout: 5 * time.Second}
)

type avatarImage struct {
	data        []byte
	contentType string
}

type avatarSettingsSource interface {
	Current() settings.Values
}

// GetAvatar returns a sender, contact or mailbox avatar. Gravatar is tried
// first; the sender domain favicon is the fallback. Misses return an empty 404
// so the client can render its own placeholder without inspecting the status.
// The backend settings decide which upstreams may be contacted at all.
func (s *Server) GetAvatar(w http.ResponseWriter, r *http.Request, params oapi.GetAvatarParams) {
	email, domain, ok := avatarEmail(params.Email)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_email", "Invalid email address.")
		return
	}
	size := avatarSize(params.Size)
	gravatarAllowed, bimiAllowed, faviconAllowed := avatarModules(
		s.currentAvatarSettings(), params.Gravatar, params.Bimi, params.Favicon,
	)
	if !gravatarAllowed && !bimiAllowed && !faviconAllowed {
		avatarNotFound(w)
		return
	}

	if gravatarAllowed {
		image, err := fetchAvatarImage(r.Context(), gravatarURL(email, size))
		switch {
		case err != nil:
			s.logger.Warn("gravatar request failed", "domain", domain, "error", err)
		case image != nil:
			writeAvatar(w, image)
			return
		}
	}

	if bimiAllowed {
		image, err := fetchBIMIAvatar(r.Context(), domain)
		switch {
		case err != nil:
			s.logger.Warn("bimi avatar failed", "domain", domain, "error", err)
		case image != nil:
			writeAvatar(w, image)
			return
		}
	}

	if !faviconAllowed {
		avatarNotFound(w)
		return
	}
	endpoint, err := faviconURL(domain, size)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	image, err := fetchAvatarImage(r.Context(), endpoint)
	if err != nil {
		s.logger.Warn("favicon request failed", "domain", domain, "error", err)
		avatarNotFound(w)
		return
	}
	if image == nil {
		avatarNotFound(w)
		return
	}
	writeAvatar(w, image)
}

func (s *Server) currentAvatarSettings() settings.Values {
	if s.avatarSettings != nil {
		return s.avatarSettings.Current()
	}
	if s.settings != nil {
		return s.settings.Current()
	}
	return settings.Defaults()
}

func avatarModules(values settings.Values, gravatar, bimi, favicon *bool) (bool, bool, bool) {
	if !values.NetworkAvatars {
		return false, false, false
	}
	return values.GravatarAvatars && avatarModuleEnabled(gravatar),
		values.BimiAvatars && avatarModuleEnabled(bimi),
		values.GoogleFaviconAvatars && avatarModuleEnabled(favicon)
}

func avatarModuleEnabled(flag *bool) bool {
	return flag == nil || *flag
}

func gravatarURL(email string, size int) string {
	sum := sha256.Sum256([]byte(email))
	query := url.Values{}
	query.Set("d", "404")
	query.Set("r", "g")
	query.Set("s", strconv.Itoa(size))
	return strings.TrimRight(gravatarUpstream, "/") + "/" + hex.EncodeToString(sum[:]) + "?" + query.Encode()
}

func faviconURL(domain string, size int) (string, error) {
	endpoint, err := url.Parse(faviconUpstream)
	if err != nil {
		return "", err
	}
	query := endpoint.Query()
	query.Set("client", "SOCIAL")
	query.Set("type", "FAVICON")
	query.Set("fallback_opts", "TYPE,SIZE,URL")
	query.Set("url", "http://"+domain)
	query.Set("size", strconv.Itoa(size))
	endpoint.RawQuery = query.Encode()
	return endpoint.String(), nil
}

func fetchAvatarImage(ctx context.Context, endpoint string) (*avatarImage, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	response, err := avatarClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream status %d", response.StatusCode)
	}

	contentType, ok := avatarContentType(response.Header.Get("Content-Type"))
	if !ok {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, avatarMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > avatarMaxBytes {
		return nil, nil
	}
	return &avatarImage{data: data, contentType: contentType}, nil
}

func avatarContentType(value string) (string, bool) {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return "", false
	}
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return mediaType, true
	default:
		return "", false
	}
}

func avatarEmail(raw string) (string, string, bool) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if email == "" || len(email) > 320 || strings.ContainsAny(email, " \t\r\n") {
		return "", "", false
	}
	at := strings.LastIndexByte(email, '@')
	if at <= 0 || at == len(email)-1 || at > 64 {
		return "", "", false
	}
	domain, ok := avatarDomain(email[at+1:])
	if !ok {
		return "", "", false
	}
	return email, domain, true
}

func avatarDomain(raw string) (string, bool) {
	domain := strings.ToLower(strings.TrimSpace(raw))
	if domain == "" || len(domain) > 253 || !strings.Contains(domain, ".") {
		return "", false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return "", false
			}
		}
	}
	return domain, true
}

func avatarSize(size *int) int {
	if size == nil {
		return avatarDefaultSize
	}
	switch {
	case *size < avatarMinSize:
		return avatarMinSize
	case *size > avatarMaxSize:
		return avatarMaxSize
	default:
		return *size
	}
}

func writeAvatar(w http.ResponseWriter, image *avatarImage) {
	w.Header().Set("Content-Type", image.contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(image.data)))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if image.contentType == "image/svg+xml" {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; sandbox")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(image.data)
}

func avatarNotFound(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
}
