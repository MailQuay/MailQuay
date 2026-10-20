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
