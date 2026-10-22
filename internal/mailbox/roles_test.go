package mailbox

import (
	"testing"

	"github.com/mailquay/mailquay/internal/imapclient"
)

func roleStrings(roles []FolderRole) []string {
	out := make([]string, len(roles))
	for i, role := range roles {
		if role.Skip {
			out[i] = "skip"
			continue
		}
		out[i] = role.Role
	}
	return out
}

func TestAssignRolesKeepsOneFolderPerSystemRole(t *testing.T) {
	folders := []imapclient.Folder{
		{Path: "INBOX", Attributes: []string{"\\Inbox"}},
		{Path: "Deleted Items", Attributes: []string{"\\Trash"}},
		{Path: "Trash"},
		{Path: "Sent", Attributes: []string{"\\Sent"}},
		{Path: "Sent Items"},
		{Path: "Archive"},
	}
	roles := roleStrings(AssignRoles(folders, FolderConfig{AutoDetect: true}))
	want := []string{"inbox", "trash", "custom", "sent", "custom", "archive"}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("role[%d] for %q = %q, want %q", i, folders[i].Path, roles[i], want[i])
		}
	}
}

func TestAssignRolesFallsBackToPathOrder(t *testing.T) {
	folders := []imapclient.Folder{
		{Path: "Trash"},
		{Path: "Deleted Items"},
	}
	roles := roleStrings(AssignRoles(folders, FolderConfig{AutoDetect: true}))
	if roles[0] != "custom" || roles[1] != "trash" {
		t.Fatalf("roles = %v, want [custom trash]", roles)
	}
}

func TestAssignRolesMatchesLocalizedNames(t *testing.T) {
	folders := []imapclient.Folder{
		{Path: "INBOX"},
		{Path: "已发送", Name: "已发送"},
		{Path: "草稿箱", Name: "草稿箱"},
		{Path: "已删除", Name: "已删除"},
		{Path: "垃圾邮件", Name: "垃圾邮件"},
		{Path: "存档", Name: "存档"},
	}
	roles := roleStrings(AssignRoles(folders, FolderConfig{AutoDetect: true}))
	want := []string{"inbox", "sent", "drafts", "trash", "junk", "archive"}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("role[%d] for %q = %q, want %q", i, folders[i].Path, roles[i], want[i])
		}
	}
}

func TestAssignRolesMatchesNestedPaths(t *testing.T) {
	folders := []imapclient.Folder{
		{Path: "INBOX", Name: "INBOX", Delimiter: "/"},
		{Path: "INBOX/Trash", Name: "Trash", Delimiter: "/"},
		{Path: "[Gmail]/Spam", Name: "Spam", Delimiter: "/"},
	}
	roles := roleStrings(AssignRoles(folders, FolderConfig{AutoDetect: true}))
	if roles[1] != "trash" || roles[2] != "junk" {
		t.Fatalf("roles = %v, want nested trash and junk", roles)
	}
}

func TestAssignRolesUsesManualOverride(t *testing.T) {
	folders := []imapclient.Folder{
		{Path: "INBOX"},
		{Path: "Deleted Items", Attributes: []string{"\\Trash"}},
		{Path: "Trash"},
	}
	cfg := FolderConfig{
		AutoDetect:     false,
		SpecialFolders: map[string]string{"trash": "Trash"},
	}
	roles := roleStrings(AssignRoles(folders, cfg))
	if roles[1] != "custom" || roles[2] != "trash" {
		t.Fatalf("roles = %v, want manual Trash to win", roles)
	}
}

func TestAssignRolesIgnoresManualWhileAutoDetectIsOn(t *testing.T) {
	folders := []imapclient.Folder{
		{Path: "INBOX"},
		{Path: "Deleted Items", Attributes: []string{"\\Trash"}},
		{Path: "Trash"},
	}
	cfg := FolderConfig{
		AutoDetect:     true,
		SpecialFolders: map[string]string{"trash": "Trash"},
	}
	roles := roleStrings(AssignRoles(folders, cfg))
	if roles[1] != "trash" || roles[2] != "custom" {
		t.Fatalf("roles = %v, want automatic Trash to win", roles)
	}
}

func TestAssignRolesKeepsInbox(t *testing.T) {
	folders := []imapclient.Folder{
		{Path: "INBOX", Attributes: []string{"\\Inbox"}},
	}
	cfg := FolderConfig{AutoDetect: false, SpecialFolders: map[string]string{"trash": "INBOX"}}
	roles := roleStrings(AssignRoles(folders, cfg))
	if roles[0] != "inbox" {
		t.Fatalf("roles = %v, want inbox kept", roles)
	}
}

func TestAssignRolesWithoutAutoDetect(t *testing.T) {
	folders := []imapclient.Folder{
		{Path: "INBOX", Attributes: []string{"\\Inbox"}},
		{Path: "Deleted Items", Attributes: []string{"\\Trash"}},
		{Path: "Sent Items"},
		{Path: "Custom"},
	}
	cfg := FolderConfig{AutoDetect: false}
	roles := roleStrings(AssignRoles(folders, cfg))
	if roles[0] != "inbox" || roles[1] != "custom" || roles[2] != "custom" || roles[3] != "custom" {
		t.Fatalf("roles = %v, want only inbox detected", roles)
	}

	cfg.SpecialFolders = map[string]string{"sent": "Sent Items"}
	roles = roleStrings(AssignRoles(folders, cfg))
	if roles[2] != "sent" {
		t.Fatalf("roles = %v, want manual sent role", roles)
	}
}

func TestAssignRolesIgnoresMissingManualPath(t *testing.T) {
	folders := []imapclient.Folder{
		{Path: "INBOX"},
		{Path: "Deleted Items", Attributes: []string{"\\Trash"}},
	}
	cfg := FolderConfig{AutoDetect: false, SpecialFolders: map[string]string{"trash": "Gone"}}
	roles := roleStrings(AssignRoles(folders, cfg))
	if roles[1] != "custom" {
		t.Fatalf("roles = %v, want unresolved trash", roles)
	}
}

func TestAssignRolesSkipsAllMail(t *testing.T) {
	folders := []imapclient.Folder{
		{Path: "[Gmail]/All Mail", Attributes: []string{"\\All"}},
		{Path: "[Gmail]/Trash", Attributes: []string{"\\Trash"}},
	}
	roles := roleStrings(AssignRoles(folders, FolderConfig{AutoDetect: true}))
	if roles[0] != "skip" || roles[1] != "trash" {
		t.Fatalf("roles = %v, want [skip trash]", roles)
	}
}

func TestSpecialFolderCodec(t *testing.T) {
	raw := EncodeSpecialFolders(map[string]string{
		"trash": "Deleted Items",
		"sent":  " ",
		"bogus": "Ignore",
	})
	got := ParseSpecialFolders(raw)
	if len(got) != 1 || got["trash"] != "Deleted Items" {
		t.Fatalf("decoded = %v, want only trash", got)
	}
}
