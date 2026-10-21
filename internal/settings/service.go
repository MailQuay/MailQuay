package settings

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mailquay/mailquay/internal/crypto"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/store"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

// Values holds every runtime-editable preference. Secrets are kept separately
// and never serialized.
type Values struct {
	DefaultInitialSyncRange  string `json:"defaultInitialSyncRange"`
	DefaultDownloadScope     string `json:"defaultDownloadScope"`
	PreserveMarked           bool   `json:"preserveMarkedOnAccountDelete"`
	SentSyncMode             string `json:"sentSyncMode"`
	DraftSyncMode            string `json:"draftSyncMode"`
	DraftPullIntervalSeconds int    `json:"draftPullIntervalSeconds"`
	DraftPushDelaySeconds    int    `json:"draftPushDelaySeconds"`
	ComposeWindowMode        string `json:"composeWindowMode"`
	SendRetryLimit           int    `json:"sendRetryLimit"`
	DeleteMode               string `json:"deleteMode"`
	DeleteQueueDays          int    `json:"deleteQueueDays"`
	SignaturePosition        string `json:"signaturePosition"`
	NetworkAvatars           bool   `json:"networkAvatars"`
	GravatarAvatars          bool   `json:"gravatarAvatars"`
	BimiAvatars              bool   `json:"bimiAvatars"`
	GoogleFaviconAvatars     bool   `json:"googleFaviconAvatars"`
	Timezone                 string `json:"timezone"`
	Language                 string `json:"language"`
	RemoveReadImmediately    bool   `json:"removeReadImmediately"`
	PrependNewMessages       bool   `json:"prependNewMessages"`
	SyncConcurrency          int    `json:"syncConcurrency"`
	FetchConcurrency         int    `json:"fetchConcurrency"`
	SearchConcurrency        int    `json:"searchConcurrency"`
	IndexBatchSize           int    `json:"indexBatchSize"`
	AutoLocalConcurrency     bool   `json:"autoLocalConcurrency"`
	EnvelopeBatchSize        int    `json:"envelopeBatchSize"`
	SessionTTLHours          int    `json:"sessionTtlHours"`
	SyncIntervalSeconds      int    `json:"syncIntervalSeconds"`
	POP3IntervalSeconds      int    `json:"pop3IntervalSeconds"`
	BodyPrefetchMaxBytes     int64  `json:"bodyPrefetchMaxBytes"`
	MaxAttachmentBytes       int64  `json:"maxAttachmentBytes"`
	GoogleClientID           string `json:"googleClientId"`
	MicrosoftClientID        string `json:"microsoftClientId"`
	LogLevel                 string `json:"logLevel"`
}

// Concurrency is the effective worker configuration. Network pools come from
// operator settings; search and index pools follow detected local resources.
type Concurrency struct {
	SyncConcurrency   int `json:"syncConcurrency"`
	FetchConcurrency  int `json:"fetchConcurrency"`
	SearchConcurrency int `json:"searchConcurrency"`
	IndexBatchSize    int `json:"indexBatchSize"`
}

// Secrets reports which write-only secrets are configured.
type Secrets struct {
	GoogleSecretSet    bool `json:"googleSecretSet"`
	MicrosoftSecretSet bool `json:"microsoftSecretSet"`
}

// Snapshot is the full settings payload.
type Snapshot struct {
	Values    Values             `json:"values"`
	Resources platform.Resources `json:"resources"`
	Effective Concurrency        `json:"effective"`
	Secrets   Secrets            `json:"secrets"`
}

// Bootstrap seeds runtime settings on the first read from environment values.
type Bootstrap struct {
	SessionTTL      time.Duration
	SyncInterval    time.Duration
	POP3Interval    time.Duration
	BodyPrefetchMax int64
	MaxAttachment   int64
	GoogleClientID  string
	GoogleSecret    string
	MicrosoftID     string
	MicrosoftSecret string
	LogLevel        string
}

// Defaults returns the documented defaults.
func Defaults() Values {
	return Values{
		DefaultInitialSyncRange:  "all",
		DefaultDownloadScope:     "full_no_attachments",
		PreserveMarked:           true,
		SentSyncMode:             "pull",
		DraftSyncMode:            "pull",
		DraftPullIntervalSeconds: 60,
		DraftPushDelaySeconds:    30,
		ComposeWindowMode:        "in_app",
		SendRetryLimit:           3,
		DeleteMode:               "provider_auto",
		DeleteQueueDays:          30,
		SignaturePosition:        "above_quote",
		NetworkAvatars:           true,
		GravatarAvatars:          true,
		BimiAvatars:              true,
		GoogleFaviconAvatars:     true,
		Timezone:                 "UTC",
		Language:                 "en",
		RemoveReadImmediately:    false,
		PrependNewMessages:       true,
		SyncConcurrency:          2,
		FetchConcurrency:         2,
		SearchConcurrency:        1,
		IndexBatchSize:           50,
		AutoLocalConcurrency:     true,
		EnvelopeBatchSize:        50,
		SessionTTLHours:          168,
		SyncIntervalSeconds:      300,
		POP3IntervalSeconds:      300,
		BodyPrefetchMaxBytes:     2 << 20,
		MaxAttachmentBytes:       25 << 20,
		LogLevel:                 "info",
	}
}

// Service reads and writes global settings and caches the runtime snapshot.
type Service struct {
	store     *store.Store
	cipher    *crypto.Cipher
	bootstrap Bootstrap
	resources platform.Resources
	cache     atomic.Pointer[Snapshot]
	mu        sync.Mutex
	resMu     sync.RWMutex
	secrets   map[string]string
	// OnChange is invoked whenever the cached settings change.
	OnChange func(Values)
}

// NewService builds the settings service.
func NewService(st *store.Store, resources platform.Resources, cipher *crypto.Cipher, bootstrap Bootstrap) *Service {
	s := &Service{store: st, cipher: cipher, bootstrap: bootstrap, resources: resources, secrets: map[string]string{}}
	base := Defaults()
	applyBootstrap(&base, bootstrap)
	s.cache.Store(&Snapshot{Values: base, Resources: resources, Effective: EffectiveConcurrency(base, resources)})
	return s
}

// Resources returns the current detection result.
func (s *Service) Resources() platform.Resources {
	s.resMu.RLock()
	defer s.resMu.RUnlock()
	return s.resources
}

// SetResources replaces the detection result.
func (s *Service) SetResources(r platform.Resources) {
	s.resMu.Lock()
	s.resources = r
	s.resMu.Unlock()
	if snap := s.cache.Load(); snap != nil {
		updated := *snap
		updated.Resources = r
		updated.Effective = EffectiveConcurrency(updated.Values, r)
		s.cache.Store(&updated)
	}
}

// Get reloads settings from PostgreSQL and refreshes the cache.
func (s *Service) Get(ctx context.Context) (Snapshot, error) {
	values := Defaults()
	applyBootstrap(&values, s.bootstrap)
	rows, err := s.store.ListSettings(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list settings: %w", err)
	}
	s.mu.Lock()
	secrets := map[string]string{}
	if s.cipher != nil {
		for _, key := range []string{"google_client_secret", "microsoft_client_secret"} {
			var raw string
			for _, row := range rows {
				if row.Key == key {
					_ = json.Unmarshal(row.Value, &raw)
				}
			}
			if raw != "" {
				decoded, err := base64.StdEncoding.DecodeString(raw)
				if err == nil && s.cipher != nil {
					if plain, err := s.cipher.DecryptString(decoded, secretAAD(key)); err == nil {
						secrets[key] = plain
					}
				}
			}
		}
		if secrets["google_client_secret"] == "" {
			secrets["google_client_secret"] = s.bootstrap.GoogleSecret
		}
		if secrets["microsoft_client_secret"] == "" {
			secrets["microsoft_client_secret"] = s.bootstrap.MicrosoftSecret
		}
		s.secrets = secrets
	}
	s.mu.Unlock()
	for _, row := range rows {
		if err := decodeInto(row.Key, row.Value, &values); err != nil {
			return Snapshot{}, fmt.Errorf("decode setting %s: %w", row.Key, err)
		}
	}
	resources := s.Resources()
	snap := Snapshot{
		Values: values, Resources: resources, Effective: EffectiveConcurrency(values, resources),
		Secrets: Secrets{
			GoogleSecretSet:    secrets["google_client_secret"] != "",
			MicrosoftSecretSet: secrets["microsoft_client_secret"] != "",
		},
	}
	s.cache.Store(&snap)
	if s.OnChange != nil {
		s.OnChange(values)
	}
	return snap, nil
}

// Current returns the cached runtime settings.
func (s *Service) Current() Values {
	if snap := s.cache.Load(); snap != nil {
		return snap.Values
	}
	return Defaults()
}

// CurrentSnapshot returns the cached snapshot.
func (s *Service) CurrentSnapshot() Snapshot {
	if snap := s.cache.Load(); snap != nil {
		return *snap
	}
	v := Defaults()
	resources := s.Resources()
	return Snapshot{Values: v, Resources: resources, Effective: EffectiveConcurrency(v, resources)}
}

// OAuthSecret returns a decrypted provider secret.
func (s *Service) OAuthSecret(provider string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch provider {
	case "google":
		return s.secrets["google_client_secret"]
	case "microsoft":
		return s.secrets["microsoft_client_secret"]
	}
	return ""
}

// SessionTTL returns the configured session lifetime.
func (s *Service) SessionTTL() time.Duration {
	return time.Duration(s.Current().SessionTTLHours) * time.Hour
}

// SyncInterval returns the account reconciliation interval.
func (s *Service) SyncInterval() time.Duration {
	return time.Duration(s.Current().SyncIntervalSeconds) * time.Second
}

// POP3Interval returns the POP3 polling interval.
func (s *Service) POP3Interval() time.Duration {
	return time.Duration(s.Current().POP3IntervalSeconds) * time.Second
}

// MaxAttachmentBytes returns the configured upload limit.
func (s *Service) MaxAttachmentBytes() int64 { return s.Current().MaxAttachmentBytes }

// BodyPrefetchMaxBytes returns the background body prefetch limit.
func (s *Service) BodyPrefetchMaxBytes() int64 { return s.Current().BodyPrefetchMaxBytes }

// Update applies a partial settings patch.
func (s *Service) Update(ctx context.Context, patch map[string]json.RawMessage) (Snapshot, error) {
	current, err := s.Get(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	next := current.Values
	for key, raw := range patch {
		if isSecretKey(key) {
			if err := s.storeSecret(ctx, key, raw); err != nil {
				return Snapshot{}, err
			}
			continue
		}
		if err := setField(key, raw, &next); err != nil {
			return Snapshot{}, err
		}
	}
	if err := validate(next); err != nil {
		return Snapshot{}, err
	}
	for key := range patch {
		if isSecretKey(key) {
			continue
		}
		value, err := fieldValue(key, next)
		if err != nil {
			return Snapshot{}, err
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return Snapshot{}, err
		}
		if err := s.store.UpsertSetting(ctx, sqlcgen.UpsertSettingParams{Key: canonicalKey(key), Value: raw}); err != nil {
			return Snapshot{}, fmt.Errorf("save setting %s: %w", key, err)
		}
	}
	return s.Get(ctx)
}

// RetryLimit returns the configured send retry count.
func (s *Service) RetryLimit(ctx context.Context) int {
	return s.Current().SendRetryLimit
}

// PreserveMarked reports whether marked mail survives account deletion.
func (s *Service) PreserveMarked(ctx context.Context) bool {
	return s.Current().PreserveMarked
}

func (s *Service) storeSecret(ctx context.Context, key string, raw json.RawMessage) error {
	var plain string
	if err := json.Unmarshal(raw, &plain); err != nil {
		return fmt.Errorf("invalid secret value: %w", err)
	}
	if plain == "" {
		return nil
	}
	if s.cipher == nil {
		return errors.New("no encryption key available for secrets")
	}
	ciphertext, err := s.cipher.EncryptString(plain, secretAAD(canonicalKey(key)))
	if err != nil {
		return err
	}
	encoded, _ := json.Marshal(base64.StdEncoding.EncodeToString(ciphertext))
	return s.store.UpsertSetting(ctx, sqlcgen.UpsertSettingParams{Key: canonicalKey(key), Value: encoded})
}

// EffectiveConcurrency combines operator-set network pools with local pools.
// Sync and fetch concurrency are deliberately not derived from CPU or memory:
// their pressure lands on the mail provider, so the defaults stay conservative
// and the operator decides. Search and index work is local; it follows detected
// resources only while the operator leaves automatic sizing enabled.
func EffectiveConcurrency(v Values, res platform.Resources) Concurrency {
	searchN, batch := v.SearchConcurrency, v.IndexBatchSize
	if v.AutoLocalConcurrency {
		searchN, batch = LocalConcurrency(res)
	}
	return Concurrency{
		SyncConcurrency: v.SyncConcurrency, FetchConcurrency: v.FetchConcurrency,
		SearchConcurrency: searchN, IndexBatchSize: batch,
	}
}

// LocalConcurrency recommends conservative search and index pool sizes from
// detected resources. Indexing never touches the network, but Bleve writes are
// memory hungry, so the recommendation stays at half the visible CPUs and
// never exceeds four queries or a 250 document batch.
func LocalConcurrency(res platform.Resources) (searchN, batch int) {
	cpu := float64(res.CPUs)
	if cpu < 1 {
		cpu = 1
	}
	return clamp(int(math.Ceil(cpu/2)), 1, 4), clamp(int(cpu*50), 50, 250)
}

func applyBootstrap(v *Values, b Bootstrap) {
	if b.SessionTTL > 0 {
		v.SessionTTLHours = int(b.SessionTTL / time.Hour)
	}
	if b.SyncInterval > 0 {
		v.SyncIntervalSeconds = int(b.SyncInterval / time.Second)
	}
	if b.POP3Interval > 0 {
		v.POP3IntervalSeconds = int(b.POP3Interval / time.Second)
	}
	if b.BodyPrefetchMax > 0 {
		v.BodyPrefetchMaxBytes = b.BodyPrefetchMax
	}
	if b.MaxAttachment > 0 {
		v.MaxAttachmentBytes = b.MaxAttachment
	}
	if b.GoogleClientID != "" {
		v.GoogleClientID = b.GoogleClientID
	}
	if b.MicrosoftID != "" {
		v.MicrosoftClientID = b.MicrosoftID
	}
	if b.LogLevel != "" {
		v.LogLevel = b.LogLevel
	}
}

func isSecretKey(key string) bool {
	switch canonicalKey(key) {
	case "google_client_secret", "microsoft_client_secret":
		return true
	}
	return false
}

func secretAAD(key string) []byte { return []byte("settings:" + key) }

func clamp(v, low, high int) int {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}

func decodeInto(key string, raw json.RawMessage, v *Values) error {
	switch key {
	case "default_initial_sync_range":
		return json.Unmarshal(raw, &v.DefaultInitialSyncRange)
	case "default_download_scope":
		return json.Unmarshal(raw, &v.DefaultDownloadScope)
	case "preserve_marked_on_account_delete":
		return json.Unmarshal(raw, &v.PreserveMarked)
	case "sent_sync_mode":
		return json.Unmarshal(raw, &v.SentSyncMode)
	case "draft_sync_mode":
		return json.Unmarshal(raw, &v.DraftSyncMode)
	case "draft_pull_interval_seconds":
		return json.Unmarshal(raw, &v.DraftPullIntervalSeconds)
	case "draft_push_delay_seconds":
		return json.Unmarshal(raw, &v.DraftPushDelaySeconds)
	case "compose_window_mode":
		return json.Unmarshal(raw, &v.ComposeWindowMode)
	case "send_retry_limit":
		return json.Unmarshal(raw, &v.SendRetryLimit)
	case "delete_mode":
		return json.Unmarshal(raw, &v.DeleteMode)
	case "delete_queue_days":
		return json.Unmarshal(raw, &v.DeleteQueueDays)
	case "signature_position":
		return json.Unmarshal(raw, &v.SignaturePosition)
	case "network_avatars":
		return json.Unmarshal(raw, &v.NetworkAvatars)
	case "gravatar_avatars":
		return json.Unmarshal(raw, &v.GravatarAvatars)
	case "bimi_avatars":
		return json.Unmarshal(raw, &v.BimiAvatars)
	case "google_favicon_avatars":
		return json.Unmarshal(raw, &v.GoogleFaviconAvatars)
	case "timezone":
		return json.Unmarshal(raw, &v.Timezone)
	case "language":
		return json.Unmarshal(raw, &v.Language)
	case "remove_read_immediately":
		return json.Unmarshal(raw, &v.RemoveReadImmediately)
	case "prepend_new_messages":
		return json.Unmarshal(raw, &v.PrependNewMessages)
	case "sync_concurrency":
		return json.Unmarshal(raw, &v.SyncConcurrency)
	case "fetch_concurrency":
		return json.Unmarshal(raw, &v.FetchConcurrency)
	case "search_concurrency":
		return json.Unmarshal(raw, &v.SearchConcurrency)
	case "index_batch_size":
		return json.Unmarshal(raw, &v.IndexBatchSize)
	case "auto_local_concurrency":
		return json.Unmarshal(raw, &v.AutoLocalConcurrency)
	case "envelope_batch_size":
		return json.Unmarshal(raw, &v.EnvelopeBatchSize)
	case "session_ttl_hours":
		return json.Unmarshal(raw, &v.SessionTTLHours)
	case "sync_interval_seconds":
		return json.Unmarshal(raw, &v.SyncIntervalSeconds)
	case "pop3_interval_seconds":
		return json.Unmarshal(raw, &v.POP3IntervalSeconds)
	case "body_prefetch_max_bytes":
		return json.Unmarshal(raw, &v.BodyPrefetchMaxBytes)
	case "max_attachment_bytes":
		return json.Unmarshal(raw, &v.MaxAttachmentBytes)
	case "google_client_id":
		return json.Unmarshal(raw, &v.GoogleClientID)
	case "microsoft_client_id":
		return json.Unmarshal(raw, &v.MicrosoftClientID)
	case "log_level":
		return json.Unmarshal(raw, &v.LogLevel)
	}
	return nil
}

func setField(key string, raw json.RawMessage, v *Values) error {
	switch canonicalKey(key) {
	case "default_initial_sync_range":
		return json.Unmarshal(raw, &v.DefaultInitialSyncRange)
	case "default_download_scope":
		return json.Unmarshal(raw, &v.DefaultDownloadScope)
	case "preserve_marked_on_account_delete":
		return json.Unmarshal(raw, &v.PreserveMarked)
	case "sent_sync_mode":
		return json.Unmarshal(raw, &v.SentSyncMode)
	case "draft_sync_mode":
		return json.Unmarshal(raw, &v.DraftSyncMode)
	case "draft_pull_interval_seconds":
		return json.Unmarshal(raw, &v.DraftPullIntervalSeconds)
	case "draft_push_delay_seconds":
		return json.Unmarshal(raw, &v.DraftPushDelaySeconds)
	case "compose_window_mode":
		return json.Unmarshal(raw, &v.ComposeWindowMode)
	case "send_retry_limit":
		return json.Unmarshal(raw, &v.SendRetryLimit)
	case "delete_mode":
		return json.Unmarshal(raw, &v.DeleteMode)
	case "delete_queue_days":
		return json.Unmarshal(raw, &v.DeleteQueueDays)
	case "signature_position":
		return json.Unmarshal(raw, &v.SignaturePosition)
	case "network_avatars":
		return json.Unmarshal(raw, &v.NetworkAvatars)
	case "gravatar_avatars":
		return json.Unmarshal(raw, &v.GravatarAvatars)
	case "bimi_avatars":
		return json.Unmarshal(raw, &v.BimiAvatars)
	case "google_favicon_avatars":
		return json.Unmarshal(raw, &v.GoogleFaviconAvatars)
	case "timezone":
		return json.Unmarshal(raw, &v.Timezone)
	case "language":
		return json.Unmarshal(raw, &v.Language)
	case "remove_read_immediately":
		return json.Unmarshal(raw, &v.RemoveReadImmediately)
	case "prepend_new_messages":
		return json.Unmarshal(raw, &v.PrependNewMessages)
	case "sync_concurrency":
		return json.Unmarshal(raw, &v.SyncConcurrency)
	case "fetch_concurrency":
		return json.Unmarshal(raw, &v.FetchConcurrency)
	case "search_concurrency":
		return json.Unmarshal(raw, &v.SearchConcurrency)
	case "index_batch_size":
		return json.Unmarshal(raw, &v.IndexBatchSize)
	case "auto_local_concurrency":
		return json.Unmarshal(raw, &v.AutoLocalConcurrency)
	case "envelope_batch_size":
		return json.Unmarshal(raw, &v.EnvelopeBatchSize)
	case "session_ttl_hours":
		return json.Unmarshal(raw, &v.SessionTTLHours)
	case "sync_interval_seconds":
		return json.Unmarshal(raw, &v.SyncIntervalSeconds)
	case "pop3_interval_seconds":
		return json.Unmarshal(raw, &v.POP3IntervalSeconds)
	case "body_prefetch_max_bytes":
		return json.Unmarshal(raw, &v.BodyPrefetchMaxBytes)
	case "max_attachment_bytes":
		return json.Unmarshal(raw, &v.MaxAttachmentBytes)
	case "google_client_id":
		return json.Unmarshal(raw, &v.GoogleClientID)
	case "microsoft_client_id":
		return json.Unmarshal(raw, &v.MicrosoftClientID)
	case "log_level":
		return json.Unmarshal(raw, &v.LogLevel)
	}
	return fmt.Errorf("unknown setting %q", key)
}

func fieldValue(key string, v Values) (any, error) {
	switch canonicalKey(key) {
	case "default_initial_sync_range":
		return v.DefaultInitialSyncRange, nil
	case "default_download_scope":
		return v.DefaultDownloadScope, nil
	case "preserve_marked_on_account_delete":
		return v.PreserveMarked, nil
	case "sent_sync_mode":
		return v.SentSyncMode, nil
	case "draft_sync_mode":
		return v.DraftSyncMode, nil
	case "draft_pull_interval_seconds":
		return v.DraftPullIntervalSeconds, nil
	case "draft_push_delay_seconds":
		return v.DraftPushDelaySeconds, nil
	case "compose_window_mode":
		return v.ComposeWindowMode, nil
	case "send_retry_limit":
		return v.SendRetryLimit, nil
	case "delete_mode":
		return v.DeleteMode, nil
	case "delete_queue_days":
		return v.DeleteQueueDays, nil
	case "signature_position":
		return v.SignaturePosition, nil
	case "network_avatars":
		return v.NetworkAvatars, nil
	case "gravatar_avatars":
		return v.GravatarAvatars, nil
	case "bimi_avatars":
		return v.BimiAvatars, nil
	case "google_favicon_avatars":
		return v.GoogleFaviconAvatars, nil
	case "timezone":
		return v.Timezone, nil
	case "language":
		return v.Language, nil
	case "remove_read_immediately":
		return v.RemoveReadImmediately, nil
	case "prepend_new_messages":
		return v.PrependNewMessages, nil
	case "sync_concurrency":
		return v.SyncConcurrency, nil
	case "fetch_concurrency":
		return v.FetchConcurrency, nil
	case "search_concurrency":
		return v.SearchConcurrency, nil
	case "index_batch_size":
		return v.IndexBatchSize, nil
	case "auto_local_concurrency":
		return v.AutoLocalConcurrency, nil
	case "envelope_batch_size":
		return v.EnvelopeBatchSize, nil
	case "session_ttl_hours":
		return v.SessionTTLHours, nil
	case "sync_interval_seconds":
		return v.SyncIntervalSeconds, nil
	case "pop3_interval_seconds":
		return v.POP3IntervalSeconds, nil
	case "body_prefetch_max_bytes":
		return v.BodyPrefetchMaxBytes, nil
	case "max_attachment_bytes":
		return v.MaxAttachmentBytes, nil
	case "google_client_id":
		return v.GoogleClientID, nil
	case "microsoft_client_id":
		return v.MicrosoftClientID, nil
	case "log_level":
		return v.LogLevel, nil
	}
	return nil, fmt.Errorf("unknown setting %q", key)
}

func validate(v Values) error {
	switch v.DefaultInitialSyncRange {
	case "all", "1d", "3d", "1w", "1m", "1y":
	default:
		return errors.New("invalid defaultInitialSyncRange")
	}
	switch v.DefaultDownloadScope {
	case "headers", "full_no_attachments", "full_with_attachments":
	default:
		return errors.New("invalid defaultDownloadScope")
	}
	if v.SentSyncMode != "pull" && v.SentSyncMode != "off" {
		return errors.New("invalid sentSyncMode")
	}
	if v.DraftSyncMode != "local" && v.DraftSyncMode != "pull" && v.DraftSyncMode != "mirror" {
		return errors.New("invalid draftSyncMode")
	}
	if v.ComposeWindowMode != "in_app" && v.ComposeWindowMode != "new_window" {
		return errors.New("invalid composeWindowMode")
	}
	if v.SendRetryLimit < 0 || v.SendRetryLimit > 10 {
		return errors.New("sendRetryLimit must be 0-10")
	}
	if v.DeleteMode != "provider_auto" && v.DeleteMode != "local_queue" {
		return errors.New("invalid deleteMode")
	}
	if v.DeleteQueueDays < 1 || v.DeleteQueueDays > 365 {
		return errors.New("deleteQueueDays must be 1-365")
	}
	for _, n := range []int{v.SyncConcurrency, v.FetchConcurrency} {
		if n < 1 || n > 32 {
			return errors.New("concurrency values must be 1-32")
		}
	}
	if v.SearchConcurrency < 1 || v.SearchConcurrency > 8 {
		return errors.New("searchConcurrency must be 1-8")
	}
	if v.IndexBatchSize < 10 || v.IndexBatchSize > 500 {
		return errors.New("indexBatchSize must be 10-500")
	}
	if v.EnvelopeBatchSize < 1 || v.EnvelopeBatchSize > 200 {
		return errors.New("envelopeBatchSize must be 1-200")
	}
	if v.Language != "en" && v.Language != "zh-CN" {
		return errors.New("language must be en or zh-CN")
	}
	switch v.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return errors.New("logLevel must be debug, info, warn or error")
	}
	if v.SessionTTLHours < 1 || v.SessionTTLHours > 8760 {
		return errors.New("sessionTtlHours must be 1-8760")
	}
	if v.SyncIntervalSeconds < 30 || v.SyncIntervalSeconds > 86400 {
		return errors.New("syncIntervalSeconds must be 30-86400")
	}
	if v.POP3IntervalSeconds < 30 || v.POP3IntervalSeconds > 86400 {
		return errors.New("pop3IntervalSeconds must be 30-86400")
	}
	if v.BodyPrefetchMaxBytes < 0 || v.BodyPrefetchMaxBytes > 1<<30 {
		return errors.New("bodyPrefetchMaxBytes must be 0-1 GiB")
	}
	if v.MaxAttachmentBytes < 1<<20 || v.MaxAttachmentBytes > 1<<30 {
		return errors.New("maxAttachmentBytes must be 1 MiB-1 GiB")
	}
	return nil
}

func canonicalKey(key string) string {
	key = strings.TrimSpace(key)
	var b strings.Builder
	for i, r := range key {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
