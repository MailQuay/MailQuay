package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/mailquay/mailquay/internal/httpapi/oapi"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/settings"
)

func (s *Server) GetSettings(w http.ResponseWriter, r *http.Request) {
	snap, err := s.settings.Get(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mapSettings(snap))
}

func (s *Server) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	var body oapi.SettingsUpdate
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	raw, _ := json.Marshal(body)
	patch := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &patch); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	snap, err := s.settings.Update(r.Context(), patch)
	if err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_settings", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, mapSettings(snap))
}

func (s *Server) DetectConcurrency(w http.ResponseWriter, r *http.Request) {
	resources := platform.DetectResources()
	s.settings.SetResources(resources)
	snap, err := s.settings.Get(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mapResources(snap))
}

func mapSettings(snap settings.Snapshot) oapi.Settings {
	v := snap.Values
	logLevel := oapi.SettingsLogLevel(v.LogLevel)
	return oapi.Settings{
		DefaultInitialSyncRange:       oapi.SettingsDefaultInitialSyncRange(v.DefaultInitialSyncRange),
		DefaultDownloadScope:          oapi.SettingsDefaultDownloadScope(v.DefaultDownloadScope),
		PreserveMarkedOnAccountDelete: v.PreserveMarked,
		SentSyncMode:                  oapi.SettingsSentSyncMode(v.SentSyncMode),
		DraftSyncMode:                 oapi.SettingsDraftSyncMode(v.DraftSyncMode),
		DraftPullIntervalSeconds:      v.DraftPullIntervalSeconds,
		DraftPushDelaySeconds:         v.DraftPushDelaySeconds,
		ComposeWindowMode:             oapi.SettingsComposeWindowMode(v.ComposeWindowMode),
		SendRetryLimit:                v.SendRetryLimit,
		DeleteMode:                    oapi.SettingsDeleteMode(v.DeleteMode),
		DeleteQueueDays:               v.DeleteQueueDays,
		SignaturePosition:             oapi.SettingsSignaturePosition(v.SignaturePosition),
		NetworkAvatars:                v.NetworkAvatars,
		GravatarAvatars:               v.GravatarAvatars,
		BimiAvatars:                   v.BimiAvatars,
		GoogleFaviconAvatars:          v.GoogleFaviconAvatars,
		Timezone:                      v.Timezone,
		Language:                      oapi.SettingsLanguage(v.Language),
		EnvelopeBatchSize:             v.EnvelopeBatchSize,
		SearchConcurrency:             v.SearchConcurrency,
		IndexBatchSize:                v.IndexBatchSize,
		AutoLocalConcurrency:          v.AutoLocalConcurrency,
		Concurrency:                   mapConcurrency(snap.Effective),
		Resources:                     mapResources(snap),
		SessionTtlHours:               v.SessionTTLHours,
		SyncIntervalSeconds:           v.SyncIntervalSeconds,
		Pop3IntervalSeconds:           v.POP3IntervalSeconds,
		BodyPrefetchMaxBytes:          int(v.BodyPrefetchMaxBytes),
		MaxAttachmentBytes:            int(v.MaxAttachmentBytes),
		GoogleClientId:                v.GoogleClientID,
		MicrosoftClientId:             v.MicrosoftClientID,
		GoogleSecretSet:               snap.Secrets.GoogleSecretSet,
		MicrosoftSecretSet:            snap.Secrets.MicrosoftSecretSet,
		LogLevel:                      &logLevel,
	}
}

func mapConcurrency(c settings.Concurrency) oapi.ConcurrencySettings {
	return oapi.ConcurrencySettings{
		SyncConcurrency: c.SyncConcurrency, FetchConcurrency: c.FetchConcurrency,
		SearchConcurrency: c.SearchConcurrency, IndexBatchSize: c.IndexBatchSize,
	}
}

func mapResources(snap settings.Snapshot) oapi.ResourceInfo {
	return oapi.ResourceInfo{
		CgroupVersion: snap.Resources.CgroupVersion, Cpus: snap.Resources.CPUs,
		MemoryBytes: int(snap.Resources.MemoryBytes), CpuSource: snap.Resources.CPUSource,
		MemorySource: snap.Resources.MemorySource,
	}
}

func sliceOrEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
