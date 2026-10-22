package mailbox

import (
	"testing"

	"github.com/mailquay/mailquay/internal/imapclient"
)

func TestAssignRolesKeepsOneFolderPerSystemRole(t *testing.T) {
	folders := []imapclient.Folder{
		{Path: "INBOX", Attributes: []string{"\\Inbox"}},
		{Path: "Deleted Items", Attributes: []string{"\\Trash"}},
		{Path: "Trash"},
		{Path: "Sent", Attributes: []string{"\\Sent"}},
		{Path: "Sent Items"},
		{Path: "Archive"},
	}
	roles := AssignRoles(folders)
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
	roles := AssignRoles(folders)
	if roles[0] != "custom" || roles[1] != "trash" {
		t.Fatalf("roles = %v, want [custom trash]", roles)
	}
}

func TestAssignRolesSkipsAllMail(t *testing.T) {
	folders := []imapclient.Folder{
		{Path: "[Gmail]/All Mail", Attributes: []string{"\\All"}},
		{Path: "[Gmail]/Trash", Attributes: []string{"\\Trash"}},
	}
	roles := AssignRoles(folders)
	if roles[0] != "" || roles[1] != "trash" {
		t.Fatalf("roles = %v, want [\"\" trash]", roles)
	}
}
