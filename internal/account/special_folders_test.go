package account

import "testing"

func TestNormalizeSpecialFolders(t *testing.T) {
	got, err := normalizeSpecialFolders(map[string]string{
		"trash": " Deleted Items ",
		"sent":  "Sent Items",
		"junk":  " ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["trash"] != "Deleted Items" || got["sent"] != "Sent Items" {
		t.Fatalf("normalized = %v", got)
	}
	if _, ok := got["junk"]; ok {
		t.Fatalf("blank path kept: %v", got)
	}
}

func TestNormalizeSpecialFoldersRejectsUnknownRole(t *testing.T) {
	if _, err := normalizeSpecialFolders(map[string]string{"bogus": "Folder"}); err == nil {
		t.Fatal("unknown role accepted")
	}
}

func TestNormalizeSpecialFoldersRejectsSharedPath(t *testing.T) {
	if _, err := normalizeSpecialFolders(map[string]string{"trash": "Same", "junk": "Same"}); err == nil {
		t.Fatal("shared path accepted")
	}
}
