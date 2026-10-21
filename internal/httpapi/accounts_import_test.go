package httpapi

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveEmclientUploadRaw(t *testing.T) {
	dir := t.TempDir()
	if err := saveEmclientUpload(strings.NewReader("database"), "accounts.dat", dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "accounts.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "database" {
		t.Fatalf("content = %q", data)
	}
}

func TestSaveEmclientUploadZip(t *testing.T) {
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"eM Client/accounts.dat":     "database",
		"eM Client/accounts.dat-wal": "wal",
		"../evil.txt":                "nope",
	} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := saveEmclientUpload(bytes.NewReader(buf.Bytes()), "accounts.zip", dir); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"accounts.dat": "database", "accounts.dat-wal": "wal"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Fatalf("%s = %q", name, data)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.txt")); err == nil {
		t.Fatal("zip entry outside the allowlist was extracted")
	}
}

func TestSaveEmclientUploadZipWithoutDatabase(t *testing.T) {
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	entry, err := writer.Create("notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("nope")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	if err := saveEmclientUpload(bytes.NewReader(buf.Bytes()), "accounts.zip", t.TempDir()); err == nil {
		t.Fatal("expected an error for a zip without accounts.dat")
	}
}
