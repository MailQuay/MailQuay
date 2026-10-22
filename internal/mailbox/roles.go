package mailbox

import (
	"strings"

	"github.com/mailquay/mailquay/internal/imapclient"
)

// RoleFromFolder maps an IMAP folder to a MailQuay role. skip is true for
// folders that must not be synced (provider All Mail).
func RoleFromFolder(folder imapclient.Folder) (role string, skip bool) {
	attrs := make([]string, 0, len(folder.Attributes))
	for _, a := range folder.Attributes {
		attrs = append(attrs, strings.ToLower(a))
	}
	for _, a := range attrs {
		switch a {
		case "\\all":
			return "", true
		case "\\inbox":
			return "inbox", false
		case "\\sent":
			return "sent", false
		case "\\drafts":
			return "drafts", false
		case "\\trash":
			return "trash", false
		case "\\junk":
			return "junk", false
		case "\\archive":
			return "archive", false
		}
	}
	lower := strings.ToLower(folder.Path)
	switch lower {
	case "inbox":
		return "inbox", false
	case "sent", "sent mail", "sent items", "sent messages":
		return "sent", false
	case "drafts", "draft":
		return "drafts", false
	case "trash", "deleted", "deleted items", "deleted messages", "bin":
		return "trash", false
	case "junk", "spam", "junk e-mail", "bulk mail":
		return "junk", false
	case "archive", "archives":
		return "archive", false
	case "[gmail]/all mail":
		return "", true
	case "[gmail]/sent mail":
		return "sent", false
	case "[gmail]/drafts":
		return "drafts", false
	case "[gmail]/trash":
		return "trash", false
	case "[gmail]/spam":
		return "junk", false
	case "[gmail]/starred", "[gmail]/important":
		return "custom", false
	}
	return "custom", false
}

// SystemRole reports whether the role is one of the fixed sidebar folders.
func SystemRole(role string) bool {
	switch role {
	case "inbox", "sent", "drafts", "trash":
		return true
	}
	return false
}

// systemRoleAttributes maps a role to the special-use attribute that declares
// it.
var systemRoleAttributes = map[string]string{
	"inbox":   "\\inbox",
	"sent":    "\\sent",
	"drafts":  "\\drafts",
	"trash":   "\\trash",
	"junk":    "\\junk",
	"archive": "\\archive",
}

// AssignRoles maps folders to roles and keeps at most one folder per system
// role. Providers can expose both a special-use folder and a plain folder with
// a matching name (for example "Deleted Items" declared as \Trash next to a
// "Trash" folder). Duplicate roles make the sidebar, the delete target and the
// global counters pick an arbitrary folder, so only the special-use folder
// keeps the role, ties fall back to path order, and the rest become custom.
func AssignRoles(folders []imapclient.Folder) []string {
	roles := make([]string, len(folders))
	owner := make(map[string]int, len(systemRoleAttributes))
	for i, folder := range folders {
		role, skip := RoleFromFolder(folder)
		if skip {
			continue
		}
		roles[i] = role
		if _, ok := systemRoleAttributes[role]; !ok {
			continue
		}
		current, found := owner[role]
		if !found || preferRoleFolder(folder, folders[current], role) {
			owner[role] = i
		}
	}
	for role, index := range owner {
		for i := range roles {
			if i != index && roles[i] == role {
				roles[i] = "custom"
			}
		}
	}
	return roles
}

func preferRoleFolder(candidate, current imapclient.Folder, role string) bool {
	attr := systemRoleAttributes[role]
	candidateDeclared := hasAttribute(candidate, attr)
	if candidateDeclared != hasAttribute(current, attr) {
		return candidateDeclared
	}
	return candidate.Path < current.Path
}

func hasAttribute(folder imapclient.Folder, attr string) bool {
	for _, a := range folder.Attributes {
		if strings.EqualFold(a, attr) {
			return true
		}
	}
	return false
}
