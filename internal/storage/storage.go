// Package storage implements the on-disk content-addressed blob store and the
// derived plain-text cache.
package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Errors returned by Store.
var (
	ErrInvalidSHA      = errors.New("storage: invalid sha256")
	ErrLimitExceeded   = errors.New("storage: size limit exceeded")
	ErrInvalidTempPath = errors.New("storage: path is outside the temporary directory")
)

const (
	dirMode  = 0o750
	fileMode = 0o640
)

// Store owns blob/<xx>/<sha256>, text/<sha256> and tmp inside a data directory.
type Store struct {
	root    string
	blobDir string
	textDir string
	tmpDir  string
}

// New creates the store layout under root.
func New(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("storage: empty root")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve root: %w", err)
	}
	s := &Store{
		root:    root,
		blobDir: filepath.Join(root, "blob"),
		textDir: filepath.Join(root, "text"),
		tmpDir:  filepath.Join(root, "tmp"),
	}
	for _, dir := range []string{s.root, s.blobDir, s.textDir, s.tmpDir} {
		if err := os.MkdirAll(dir, dirMode); err != nil {
			return nil, fmt.Errorf("storage: create %s: %w", dir, err)
		}
	}
	return s, nil
}

// Root returns the data directory.
func (s *Store) Root() string { return s.root }

// BlobPath returns blob/<first2>/<sha>. It returns an empty string when sha is
// not a valid SHA-256 hex digest.
func (s *Store) BlobPath(sha string) string {
	normalized, ok := normalizeSHA(sha)
	if !ok {
		return ""
	}
	return filepath.Join(s.blobDir, normalized[:2], normalized)
}

// HasBlob reports whether a blob exists.
func (s *Store) HasBlob(sha string) bool {
	path := s.BlobPath(sha)
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// PutBlob stores data and returns its lowercase hex SHA-256. Writing the same
// content twice is idempotent.
func (s *Store) PutBlob(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	path := s.BlobPath(sha)
	if path == "" {
		return "", ErrInvalidSHA
	}
	if s.HasBlob(sha) {
		return sha, nil
	}
	if err := s.writeAtomic(path, data); err != nil {
		return "", err
	}
	return sha, nil
}

// PutBlobReader streams r into the blob store, rejecting content larger than
// limit bytes. A limit of zero or less means unlimited. It returns the digest
// and the number of bytes read.
func (s *Store) PutBlobReader(r io.Reader, limit int64) (string, int64, error) {
	if r == nil {
		return "", 0, errors.New("storage: nil reader")
	}
	tmp, err := os.CreateTemp(s.tmpDir, "blob-*")
	if err != nil {
		return "", 0, fmt.Errorf("storage: create temporary blob: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		return "", 0, fmt.Errorf("storage: chmod temporary blob: %w", err)
	}

	hasher := sha256.New()
	source := io.Reader(r)
	if limit > 0 {
		source = io.LimitReader(r, limit+1)
	}
	size, copyErr := io.Copy(io.MultiWriter(tmp, hasher), source)
	if copyErr != nil {
		_ = tmp.Close()
		return "", size, fmt.Errorf("storage: write temporary blob: %w", copyErr)
	}
	if limit > 0 && size > limit {
		_ = tmp.Close()
		return "", size, ErrLimitExceeded
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", size, fmt.Errorf("storage: sync temporary blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", size, fmt.Errorf("storage: close temporary blob: %w", err)
	}

	sha := hex.EncodeToString(hasher.Sum(nil))
	path := s.BlobPath(sha)
	if path == "" {
		return "", size, ErrInvalidSHA
	}
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return "", size, fmt.Errorf("storage: create blob directory: %w", err)
	}
	if s.HasBlob(sha) {
		return sha, size, nil
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", size, fmt.Errorf("storage: commit blob: %w", err)
	}
	cleanup = false
	return sha, size, nil
}

// OpenBlob opens a stored blob.
func (s *Store) OpenBlob(sha string) (io.ReadCloser, error) {
	path := s.BlobPath(sha)
	if path == "" {
		return nil, ErrInvalidSHA
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("storage: open blob: %w", err)
	}
	return f, nil
}

// PutText stores the derived text for a raw message digest.
func (s *Store) PutText(sha string, text []byte) error {
	path, err := s.textPath(sha)
	if err != nil {
		return err
	}
	return s.writeAtomic(path, text)
}

// ReadText reads the derived text for a raw message digest.
func (s *Store) ReadText(sha string) ([]byte, error) {
	path, err := s.textPath(sha)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("storage: read text: %w", err)
	}
	return data, nil
}

// HasText reports whether derived text exists.
func (s *Store) HasText(sha string) bool {
	path, err := s.textPath(sha)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// DeleteBlob removes a blob. Deleting a missing blob is a no-op.
func (s *Store) DeleteBlob(sha string) error {
	path := s.BlobPath(sha)
	if path == "" {
		return ErrInvalidSHA
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("storage: delete blob: %w", err)
	}
	return nil
}

// DeleteText removes derived text. Deleting missing text is a no-op.
func (s *Store) DeleteText(sha string) error {
	path, err := s.textPath(sha)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("storage: delete text: %w", err)
	}
	return nil
}

// WriteTemp copies r into a new file in the temporary directory and returns its
// path. Content larger than limit is rejected; zero or less means unlimited.
func (s *Store) WriteTemp(prefix string, r io.Reader, limit int64) (string, error) {
	if r == nil {
		return "", errors.New("storage: nil reader")
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "tmp"
	}
	prefix = filepath.Base(prefix)
	if prefix == "." || prefix == string(filepath.Separator) {
		prefix = "tmp"
	}

	f, err := os.CreateTemp(s.tmpDir, prefix+"-*")
	if err != nil {
		return "", fmt.Errorf("storage: create temporary file: %w", err)
	}
	path := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(path)
		}
	}()

	if err := f.Chmod(fileMode); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("storage: chmod temporary file: %w", err)
	}

	source := io.Reader(r)
	if limit > 0 {
		source = io.LimitReader(r, limit+1)
	}
	size, err := io.Copy(f, source)
	if err != nil {
		_ = f.Close()
		return "", fmt.Errorf("storage: write temporary file: %w", err)
	}
	if limit > 0 && size > limit {
		_ = f.Close()
		return "", ErrLimitExceeded
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("storage: sync temporary file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("storage: close temporary file: %w", err)
	}
	cleanup = false
	return path, nil
}

// RemoveTemp removes a path previously returned by WriteTemp. Paths outside the
// temporary directory are rejected.
func (s *Store) RemoveTemp(path string) error {
	if path == "" {
		return ErrInvalidTempPath
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return ErrInvalidTempPath
	}
	rel, err := filepath.Rel(s.tmpDir, absolute)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ErrInvalidTempPath
	}
	if err := os.Remove(absolute); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("storage: remove temporary file: %w", err)
	}
	return nil
}

func (s *Store) textPath(sha string) (string, error) {
	normalized, ok := normalizeSHA(sha)
	if !ok {
		return "", ErrInvalidSHA
	}
	return filepath.Join(s.textDir, normalized), nil
}

func (s *Store) writeAtomic(target string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(target), dirMode); err != nil {
		return fmt.Errorf("storage: create directory: %w", err)
	}
	tmp, err := os.CreateTemp(s.tmpDir, "write-*")
	if err != nil {
		return fmt.Errorf("storage: create temporary file: %w", err)
	}
	name := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(name)
		}
	}()

	if err := tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("storage: chmod temporary file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("storage: write temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("storage: sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("storage: close temporary file: %w", err)
	}
	if err := os.Rename(name, target); err != nil {
		return fmt.Errorf("storage: commit file: %w", err)
	}
	committed = true
	return nil
}

func normalizeSHA(sha string) (string, bool) {
	if len(sha) != sha256.Size*2 {
		return "", false
	}
	if _, err := hex.DecodeString(sha); err != nil {
		return "", false
	}
	return strings.ToLower(sha), true
}

// ListBlobs returns every stored blob hash.
func (s *Store) ListBlobs() ([]string, error) {
	root := filepath.Join(s.Root(), "blob")
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || len(d.Name()) != 64 {
			return nil
		}
		out = append(out, d.Name())
		return nil
	})
	return out, err
}

// ListTexts returns every derived text cache key.
func (s *Store) ListTexts() ([]string, error) {
	root := filepath.Join(s.Root(), "text")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && len(entry.Name()) == 64 {
			out = append(out, entry.Name())
		}
	}
	return out, nil
}

// ModTime returns the modification time of a stored blob.
func (s *Store) ModTime(sha string) (time.Time, error) {
	info, err := os.Stat(s.BlobPath(sha))
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

// TextModTime returns the modification time of a text cache entry.
func (s *Store) TextModTime(sha string) (time.Time, error) {
	info, err := os.Stat(filepath.Join(s.Root(), "text", sha))
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}
