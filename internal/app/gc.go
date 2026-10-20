package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/mailquay/mailquay/internal/storage"
	"github.com/mailquay/mailquay/internal/store"
)

func gcBlobs(ctx context.Context, st *store.Store, blobs *storage.Store, logger *slog.Logger) {
	refs, err := st.ListReferencedBlobs(ctx)
	if err != nil {
		logger.Debug("list referenced blobs", "error", err)
		return
	}
	referenced := make(map[string]struct{}, len(refs))
	for _, sha := range refs {
		referenced[sha] = struct{}{}
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	stored, err := blobs.ListBlobs()
	if err != nil {
		logger.Debug("list stored blobs", "error", err)
		return
	}
	for _, sha := range stored {
		if _, ok := referenced[sha]; ok {
			continue
		}
		if mod, err := blobs.ModTime(sha); err == nil && mod.After(cutoff) {
			continue
		}
		if err := blobs.DeleteBlob(sha); err != nil {
			logger.Debug("delete orphan blob", "sha", sha, "error", err)
		}
	}
	texts, err := blobs.ListTexts()
	if err != nil {
		logger.Debug("list text cache", "error", err)
		return
	}
	for _, sha := range texts {
		if _, ok := referenced[sha]; ok {
			continue
		}
		if mod, err := blobs.TextModTime(sha); err == nil && mod.After(cutoff) {
			continue
		}
		_ = blobs.DeleteText(sha)
	}
}
