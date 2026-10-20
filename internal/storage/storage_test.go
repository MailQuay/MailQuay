package storage_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mailquay/mailquay/internal/storage"
)

const helloSum = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"

func newStore(t *testing.T) *storage.Store {
	t.Helper()
	root := t.TempDir()
	store, err := storage.New(filepath.Join(root, "data"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return store
}

func TestNewCreatesLayout(t *testing.T) {
	store := newStore(t)
	for _, dir := range []string{"blob", "text", "tmp"} {
		info, err := os.Stat(filepath.Join(store.Root(), dir))
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
	}
}

func TestPutBlobIdempotent(t *testing.T) {
	store := newStore(t)
	sha, err := store.PutBlob([]byte("hello"))
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if sha != helloSum {
		t.Fatalf("sha = %q", sha)
	}

	wantPath := filepath.Join(store.Root(), "blob", sha[:2], sha)
	if got := store.BlobPath(sha); got != wantPath {
		t.Fatalf("BlobPath = %q, want %q", got, wantPath)
	}
	if !store.HasBlob(sha) {
		t.Fatal("HasBlob = false")
	}

	info, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("stat blob: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Errorf("blob mode = %o", perm)
	}

	again, err := store.PutBlob([]byte("hello"))
	if err != nil {
		t.Fatalf("PutBlob again: %v", err)
	}
	if again != sha {
		t.Fatalf("second sha = %q", again)
	}
	after, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("stat blob again: %v", err)
	}
	if !after.ModTime().Equal(info.ModTime()) {
		t.Errorf("idempotent write changed mtime")
	}

	reader, err := store.OpenBlob(sha)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	reader.Close()
	if string(data) != "hello" {
		t.Errorf("blob content = %q", data)
	}
}

func TestPutBlobReader(t *testing.T) {
	store := newStore(t)
	sha, size, err := store.PutBlobReader(strings.NewReader("reader content"), 100)
	if err != nil {
		t.Fatalf("PutBlobReader: %v", err)
	}
	if size != int64(len("reader content")) {
		t.Errorf("size = %d", size)
	}
	reader, err := store.OpenBlob(sha)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	data, _ := io.ReadAll(reader)
	reader.Close()
	if string(data) != "reader content" {
		t.Errorf("content = %q", data)
	}
}

func TestPutBlobReaderLimit(t *testing.T) {
	store := newStore(t)
	_, _, err := store.PutBlobReader(strings.NewReader("too long"), 3)
	if !errors.Is(err, storage.ErrLimitExceeded) {
		t.Fatalf("err = %v, want ErrLimitExceeded", err)
	}

	entries, err := os.ReadDir(filepath.Join(store.Root(), "tmp"))
	if err != nil {
		t.Fatalf("read tmp: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

func TestSHAValidationAndTraversal(t *testing.T) {
	store := newStore(t)
	bad := []string{"", "abc", "../../etc/passwd", strings.Repeat("g", 64), strings.Repeat("a", 63)}

	for _, sha := range bad {
		if path := store.BlobPath(sha); path != "" {
			t.Errorf("BlobPath(%q) = %q, want empty", sha, path)
		}
		if store.HasBlob(sha) {
			t.Errorf("HasBlob(%q) = true", sha)
		}
		if _, err := store.OpenBlob(sha); !errors.Is(err, storage.ErrInvalidSHA) {
			t.Errorf("OpenBlob(%q) err = %v", sha, err)
		}
		if err := store.DeleteBlob(sha); !errors.Is(err, storage.ErrInvalidSHA) {
			t.Errorf("DeleteBlob(%q) err = %v", sha, err)
		}
		if err := store.PutText(sha, []byte("x")); !errors.Is(err, storage.ErrInvalidSHA) {
			t.Errorf("PutText(%q) err = %v", sha, err)
		}
		if _, err := store.ReadText(sha); !errors.Is(err, storage.ErrInvalidSHA) {
			t.Errorf("ReadText(%q) err = %v", sha, err)
		}
	}
}

func TestTextCache(t *testing.T) {
	store := newStore(t)
	sha := strings.Repeat("ab", 32)
	if store.HasText(sha) {
		t.Fatal("HasText before write = true")
	}
	if err := store.PutText(sha, []byte("plain text")); err != nil {
		t.Fatalf("PutText: %v", err)
	}
	if !store.HasText(sha) {
		t.Fatal("HasText = false")
	}
	data, err := store.ReadText(sha)
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if string(data) != "plain text" {
		t.Errorf("text = %q", data)
	}
	if err := store.PutText(sha, []byte("updated")); err != nil {
		t.Fatalf("PutText update: %v", err)
	}
	data, _ = store.ReadText(sha)
	if string(data) != "updated" {
		t.Errorf("updated text = %q", data)
	}
	if err := store.DeleteText(sha); err != nil {
		t.Fatalf("DeleteText: %v", err)
	}
	if store.HasText(sha) {
		t.Error("HasText after delete = true")
	}
	if err := store.DeleteText(sha); err != nil {
		t.Errorf("second DeleteText: %v", err)
	}
	if _, err := store.ReadText(sha); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadText after delete err = %v", err)
	}
}

func TestDeleteBlob(t *testing.T) {
	store := newStore(t)
	sha, err := store.PutBlob([]byte("gone"))
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if err := store.DeleteBlob(sha); err != nil {
		t.Fatalf("DeleteBlob: %v", err)
	}
	if store.HasBlob(sha) {
		t.Error("HasBlob after delete = true")
	}
	if err := store.DeleteBlob(sha); err != nil {
		t.Errorf("second DeleteBlob: %v", err)
	}
}

func TestWriteTempAndRemove(t *testing.T) {
	store := newStore(t)
	path, err := store.WriteTemp("upload", bytes.NewReader([]byte("temp data")), 100)
	if err != nil {
		t.Fatalf("WriteTemp: %v", err)
	}
	if filepath.Dir(path) != filepath.Join(store.Root(), "tmp") {
		t.Errorf("path = %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp: %v", err)
	}
	if string(data) != "temp data" {
		t.Errorf("temp content = %q", data)
	}
	if err := store.RemoveTemp(path); err != nil {
		t.Fatalf("RemoveTemp: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp still exists: %v", err)
	}
	if err := store.RemoveTemp(path); err != nil {
		t.Errorf("RemoveTemp missing file: %v", err)
	}
}

func TestWriteTempLimit(t *testing.T) {
	store := newStore(t)
	if _, err := store.WriteTemp("big", strings.NewReader("123456"), 5); !errors.Is(err, storage.ErrLimitExceeded) {
		t.Fatalf("err = %v, want ErrLimitExceeded", err)
	}
	entries, _ := os.ReadDir(filepath.Join(store.Root(), "tmp"))
	if len(entries) != 0 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

func TestRemoveTempRejectsOutsidePath(t *testing.T) {
	store := newStore(t)
	outside := filepath.Join(t.TempDir(), "keep.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := store.RemoveTemp(outside); !errors.Is(err, storage.ErrInvalidTempPath) {
		t.Fatalf("err = %v, want ErrInvalidTempPath", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("outside file was removed: %v", err)
	}
	if err := store.RemoveTemp("../../etc/passwd"); !errors.Is(err, storage.ErrInvalidTempPath) {
		t.Errorf("err = %v, want ErrInvalidTempPath", err)
	}
}
