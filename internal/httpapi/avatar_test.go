package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mailquay/mailquay/internal/httpapi/oapi"
	"github.com/mailquay/mailquay/internal/settings"
)

type staticAvatarSettings struct {
	values settings.Values
}

func (s staticAvatarSettings) Current() settings.Values { return s.values }

func withAvatarUpstreams(t *testing.T, gravatar, favicon string) {
	t.Helper()
	previousGravatar, previousFavicon, previousClient := gravatarUpstream, faviconUpstream, avatarClient
	previousBIMILookup, previousBIMIClient := bimiLookupTXT, bimiClient
	t.Cleanup(func() {
		gravatarUpstream, faviconUpstream, avatarClient = previousGravatar, previousFavicon, previousClient
		bimiLookupTXT, bimiClient = previousBIMILookup, previousBIMIClient
	})
	gravatarUpstream = gravatar
	faviconUpstream = favicon
	avatarClient = &http.Client{Timeout: time.Second}
	bimiLookupTXT = func(context.Context, string) ([]string, error) { return nil, nil }
	bimiClient = &http.Client{Timeout: time.Second}
}

func avatarTestServer(values settings.Values) *Server {
	return &Server{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		avatarSettings: staticAvatarSettings{values: values},
	}
}

func TestGetAvatarPrefersGravatar(t *testing.T) {
	faviconCalls := 0
	bimiLookupCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/gravatar/"):
			if got := r.URL.Path; got != "/gravatar/8c2a47d3bdb8d3096a6479f53eac3b724291db5f1c31611100f675be5537329d" {
				t.Errorf("gravatar path = %q", got)
			}
			if got := r.URL.Query().Get("d"); got != "404" {
				t.Errorf("gravatar default = %q", got)
			}
			if got := r.URL.Query().Get("s"); got != "96" {
				t.Errorf("gravatar size = %q", got)
			}
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("GRAVATAR"))
		case r.URL.Path == "/favicon":
			faviconCalls++
			t.Error("favicon should not be requested")
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer upstream.Close()
	withAvatarUpstreams(t, upstream.URL+"/gravatar", upstream.URL+"/favicon")
	bimiLookupTXT = func(context.Context, string) ([]string, error) {
		bimiLookupCalls++
		return nil, nil
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/avatars?email=Me@Example.COM&size=96", nil)
	avatarTestServer(settings.Defaults()).GetAvatar(recorder, request, oapi.GetAvatarParams{
		Email: "Me@Example.COM", Size: new(96),
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("content type = %q", got)
	}
	if got := recorder.Body.String(); got != "GRAVATAR" {
		t.Errorf("body = %q", got)
	}
	if faviconCalls != 0 {
		t.Errorf("favicon calls = %d", faviconCalls)
	}
	if bimiLookupCalls != 0 {
		t.Errorf("BIMI lookup calls = %d", bimiLookupCalls)
	}
	if got := recorder.Header().Get("Cache-Control"); !strings.Contains(got, "private") {
		t.Errorf("cache control = %q", got)
	}
}

func TestGetAvatarFallsBackToFavicon(t *testing.T) {
	gravatarCalls, faviconCalls := 0, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/gravatar/"):
			gravatarCalls++
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/favicon":
			faviconCalls++
			if got := r.URL.Query().Get("url"); got != "http://example.com" {
				t.Errorf("favicon url = %q", got)
			}
			if got := r.URL.Query().Get("size"); got != "80" {
				t.Errorf("favicon size = %q", got)
			}
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("FAVICON"))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer upstream.Close()
	withAvatarUpstreams(t, upstream.URL+"/gravatar", upstream.URL+"/favicon")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/avatars?email=user@example.com", nil)
	avatarTestServer(settings.Defaults()).GetAvatar(recorder, request, oapi.GetAvatarParams{
		Email: "user@example.com",
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := recorder.Body.String(); got != "FAVICON" {
		t.Errorf("body = %q", got)
	}
	if gravatarCalls != 1 || faviconCalls != 1 {
		t.Errorf("calls gravatar=%d favicon=%d", gravatarCalls, faviconCalls)
	}
}

func TestGetAvatarHonorsSourceParameters(t *testing.T) {
	t.Run("gravatar disabled", func(t *testing.T) {
		gravatarCalls, faviconCalls := 0, 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/gravatar/") {
				gravatarCalls++
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			faviconCalls++
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("FAVICON"))
		}))
		defer upstream.Close()
		withAvatarUpstreams(t, upstream.URL+"/gravatar", upstream.URL+"/favicon")

		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/avatars?email=user@example.com", nil)
		avatarTestServer(settings.Defaults()).GetAvatar(recorder, request, oapi.GetAvatarParams{
			Email: "user@example.com", Gravatar: new(false),
		})

		if recorder.Code != http.StatusOK || recorder.Body.String() != "FAVICON" {
			t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
		}
		if gravatarCalls != 0 || faviconCalls != 1 {
			t.Errorf("calls gravatar=%d favicon=%d", gravatarCalls, faviconCalls)
		}
	})

	t.Run("favicon disabled", func(t *testing.T) {
		gravatarCalls, faviconCalls := 0, 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/gravatar/") {
				gravatarCalls++
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write([]byte("GRAVATAR"))
				return
			}
			faviconCalls++
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer upstream.Close()
		withAvatarUpstreams(t, upstream.URL+"/gravatar", upstream.URL+"/favicon")

		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/avatars?email=user@example.com", nil)
		avatarTestServer(settings.Defaults()).GetAvatar(recorder, request, oapi.GetAvatarParams{
			Email: "user@example.com", Favicon: new(false),
		})

		if recorder.Code != http.StatusOK || recorder.Body.String() != "GRAVATAR" {
			t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
		}
		if gravatarCalls != 1 || faviconCalls != 0 {
			t.Errorf("calls gravatar=%d favicon=%d", gravatarCalls, faviconCalls)
		}
	})
}

func TestGetAvatarRespectsBackendSettings(t *testing.T) {
	t.Run("network avatars off", func(t *testing.T) {
		requests := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
		}))
		defer upstream.Close()
		withAvatarUpstreams(t, upstream.URL+"/gravatar", upstream.URL+"/favicon")

		values := settings.Defaults()
		values.NetworkAvatars = false
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/avatars?email=user@example.com", nil)
		avatarTestServer(values).GetAvatar(recorder, request, oapi.GetAvatarParams{
			Email: "user@example.com",
		})

		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d", recorder.Code)
		}
		if requests != 0 {
			t.Errorf("upstream requests = %d", requests)
		}
	})

	t.Run("gravatar module off", func(t *testing.T) {
		gravatarCalls, faviconCalls := 0, 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/gravatar/") {
				gravatarCalls++
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			faviconCalls++
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("FAVICON"))
		}))
		defer upstream.Close()
		withAvatarUpstreams(t, upstream.URL+"/gravatar", upstream.URL+"/favicon")

		values := settings.Defaults()
		values.GravatarAvatars = false
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/avatars?email=user@example.com", nil)
		avatarTestServer(values).GetAvatar(recorder, request, oapi.GetAvatarParams{
			Email: "user@example.com",
		})

		if recorder.Code != http.StatusOK || recorder.Body.String() != "FAVICON" {
			t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
		}
		if gravatarCalls != 0 || faviconCalls != 1 {
			t.Errorf("calls gravatar=%d favicon=%d", gravatarCalls, faviconCalls)
		}
	})

	t.Run("favicon module off", func(t *testing.T) {
		gravatarCalls, faviconCalls := 0, 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/gravatar/") {
				gravatarCalls++
				w.WriteHeader(http.StatusNotFound)
				return
			}
			faviconCalls++
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("FAVICON"))
		}))
		defer upstream.Close()
		withAvatarUpstreams(t, upstream.URL+"/gravatar", upstream.URL+"/favicon")

		values := settings.Defaults()
		values.GoogleFaviconAvatars = false
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/avatars?email=user@example.com", nil)
		avatarTestServer(values).GetAvatar(recorder, request, oapi.GetAvatarParams{
			Email: "user@example.com",
		})

		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d", recorder.Code)
		}
		if gravatarCalls != 1 || faviconCalls != 0 {
			t.Errorf("calls gravatar=%d favicon=%d", gravatarCalls, faviconCalls)
		}
	})
}

func TestGetAvatarRejectsInvalidEmail(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer upstream.Close()
	withAvatarUpstreams(t, upstream.URL+"/gravatar", upstream.URL+"/favicon")

	for _, email := range []string{"", "not-an-email", "user@localhost", "user@evil.com/path", "user name@example.com", "@example.com", "user@"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/avatars", nil)
		avatarTestServer(settings.Defaults()).GetAvatar(recorder, request, oapi.GetAvatarParams{Email: email})
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("email %q: status = %d, want 400", email, recorder.Code)
		}
	}
	if requests != 0 {
		t.Errorf("upstream requests = %d, want 0", requests)
	}
}

func TestGetAvatarHidesUpstreamMiss(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header string
	}{
		{name: "not found", status: http.StatusNotFound, header: "image/png"},
		{name: "server error", status: http.StatusInternalServerError, header: "image/png"},
		{name: "non image", status: http.StatusOK, header: "text/html"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/gravatar/") {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", tc.header)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("FALLBACK"))
			}))
			defer upstream.Close()
			withAvatarUpstreams(t, upstream.URL+"/gravatar", upstream.URL+"/favicon")

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/avatars?email=user@example.com", nil)
			avatarTestServer(settings.Defaults()).GetAvatar(recorder, request, oapi.GetAvatarParams{
				Email: "user@example.com",
			})

			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d", recorder.Code)
			}
			if recorder.Body.Len() != 0 {
				t.Errorf("body = %q, want empty", recorder.Body.String())
			}
		})
	}
}

const testBIMISVG = `<?xml version="1.0" encoding="UTF-8"?><svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><rect width="16" height="16"/></svg>`

func TestGetAvatarUsesBIMIBeforeFavicon(t *testing.T) {
	faviconCalls := 0
	gravatar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer gravatar.Close()
	favicon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		faviconCalls++
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("FAVICON"))
	}))
	defer favicon.Close()
	bimi := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/logo.svg" {
			t.Errorf("BIMI path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write([]byte(testBIMISVG))
	}))
	defer bimi.Close()

	withAvatarUpstreams(t, gravatar.URL+"/avatar", favicon.URL+"/favicon")
	bimiLookupTXT = func(_ context.Context, name string) ([]string, error) {
		if name != "default._bimi.example.com" {
			t.Errorf("BIMI lookup = %q", name)
		}
		return []string{"v=BIMI1; l=" + bimi.URL + "/logo.svg; a=" + bimi.URL + "/vmc.pem"}, nil
	}
	bimiClient = bimi.Client()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/avatars?email=user@example.com", nil)
	avatarTestServer(settings.Defaults()).GetAvatar(recorder, request, oapi.GetAvatarParams{
		Email: "user@example.com",
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "image/svg+xml" {
		t.Errorf("content type = %q", got)
	}
	if got := recorder.Body.String(); got != testBIMISVG {
		t.Errorf("body = %q", got)
	}
	if got := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
		t.Errorf("BIMI content security policy = %q", got)
	}
	if faviconCalls != 0 {
		t.Errorf("favicon calls = %d", faviconCalls)
	}
}

func TestGetAvatarRejectsUnsafeBIMI(t *testing.T) {
	faviconCalls := 0
	gravatar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer gravatar.Close()
	favicon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		faviconCalls++
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("FAVICON"))
	}))
	defer favicon.Close()
	bimi := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	}))
	defer bimi.Close()

	withAvatarUpstreams(t, gravatar.URL+"/avatar", favicon.URL+"/favicon")
	bimiLookupTXT = func(context.Context, string) ([]string, error) {
		return []string{"v=BIMI1; l=" + bimi.URL + "/logo.svg"}, nil
	}
	bimiClient = bimi.Client()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/avatars?email=user@example.com", nil)
	avatarTestServer(settings.Defaults()).GetAvatar(recorder, request, oapi.GetAvatarParams{
		Email: "user@example.com",
	})

	if recorder.Code != http.StatusOK || recorder.Body.String() != "FAVICON" {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if faviconCalls != 1 {
		t.Errorf("favicon calls = %d", faviconCalls)
	}
}

func TestGetAvatarRespectsBIMISetting(t *testing.T) {
	lookupCalls, faviconCalls := 0, 0
	gravatar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer gravatar.Close()
	favicon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		faviconCalls++
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("FAVICON"))
	}))
	defer favicon.Close()

	withAvatarUpstreams(t, gravatar.URL+"/avatar", favicon.URL+"/favicon")
	bimiLookupTXT = func(context.Context, string) ([]string, error) {
		lookupCalls++
		return nil, nil
	}
	values := settings.Defaults()
	values.BimiAvatars = false

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/avatars?email=user@example.com", nil)
	avatarTestServer(values).GetAvatar(recorder, request, oapi.GetAvatarParams{
		Email: "user@example.com",
	})

	if recorder.Code != http.StatusOK || recorder.Body.String() != "FAVICON" {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if lookupCalls != 0 || faviconCalls != 1 {
		t.Errorf("lookup=%d favicon=%d", lookupCalls, faviconCalls)
	}
}
