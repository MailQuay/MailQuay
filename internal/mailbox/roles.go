package mailbox

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/mailquay/mailquay/internal/imapclient"
)

// SpecialRoleNames lists the roles a remote folder can take.
var SpecialRoleNames = []string{"sent", "drafts", "trash", "junk", "archive"}

// FolderConfig controls special folder detection for one account.
type FolderConfig struct {
	// AutoDetect resolves roles from IMAP special-use flags, then from the
	// conventional folder names. When false only SpecialFolders is used.
	AutoDetect bool
	// SpecialFolders maps a role to an exact remote folder path.
	SpecialFolders map[string]string
}

// IsSpecialRole reports whether role can be chosen in the account settings.
func IsSpecialRole(role string) bool {
	for _, name := range SpecialRoleNames {
		if role == name {
			return true
		}
	}
	return false
}

// SystemRole reports whether the role belongs to the fixed sidebar folders.
func SystemRole(role string) bool {
	switch role {
	case "inbox", "sent", "drafts", "trash":
		return true
	}
	return false
}

type source int

const (
	sourceNone source = iota
	sourceName
	sourceFlag
	sourceManual
)

// roleFromFlag maps an IMAP special-use attribute to its role.
func roleFromFlag(flag string) (string, bool) {
	switch flag {
	case "\\inbox":
		return "inbox", true
	case "\\sent":
		return "sent", true
	case "\\drafts":
		return "drafts", true
	case "\\trash":
		return "trash", true
	case "\\junk":
		return "junk", true
	case "\\archive":
		return "archive", true
	}
	return "", false
}

// roleNames maps conventional folder names to roles. Servers without
// special-use support only expose names, and providers localize them.
var roleNames = func() map[string]string {
	out := make(map[string]string)
	for role, names := range map[string][]string{
		"inbox": {"inbox"},
		"sent": {
			"sent", "sent mail", "sent items", "sent messages",
			"已发送", "已发送邮件", "已傳送", "已傳送郵件", "发件箱", "寄件备份", "寄件備份", "寄件匣",
		},
		"drafts": {
			"drafts", "draft", "草稿", "草稿箱", "草稿匣",
		},
		"trash": {
			"trash", "deleted", "deleted items", "deleted messages", "bin",
			"已删除", "已删除邮件", "已刪除", "已刪除郵件", "废纸篓", "垃圾桶", "資源回收筒",
		},
		"junk": {
			"junk", "spam", "junk e-mail", "junk email", "bulk mail",
			"垃圾邮件", "垃圾郵件",
		},
		"archive": {
			"archive", "archives", "存档", "存档邮件", "存檔", "归档", "歸檔", "封存",
		},
	} {
		for _, name := range names {
			out[name] = role
		}
	}
	return out
}()

// RoleFromFolder maps an IMAP folder to a MailQuay role using flags first and
// conventional names second. skip is true for folders that must not be synced
// (provider All Mail).
func RoleFromFolder(folder imapclient.Folder) (role string, skip bool) {
	role, _, skip = detectRole(folder)
	return role, skip
}

func detectRole(folder imapclient.Folder) (role string, matched source, skip bool) {
	for _, attr := range folder.Attributes {
		flag := strings.ToLower(attr)
		if flag == "\\all" {
			return "", sourceNone, true
		}
		if role, ok := roleFromFlag(flag); ok {
			return role, sourceFlag, false
		}
	}
	for _, name := range folderNames(folder) {
		if name == "all mail" {
			return "", sourceNone, true
		}
		if candidate, ok := roleNames[name]; ok {
			return candidate, sourceName, false
		}
	}
	return "custom", sourceNone, false
}

// folderNames returns the lowercased candidates a folder name can match on:
// the full path, the folder name and the leaf path segment. Nested paths keep
// the last segment so "INBOX/Trash" and "[Gmail]/Spam" match their role.
func folderNames(folder imapclient.Folder) []string {
	candidates := make([]string, 0, 3)
	seen := make(map[string]struct{}, 3)
	add := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		candidates = append(candidates, value)
	}
	add(folder.Name)
	add(folder.Path)
	delimiter := folder.Delimiter
	if delimiter == "" {
		delimiter = "/"
	}
	if index := strings.LastIndex(folder.Path, delimiter); index >= 0 {
		add(folder.Path[index+len(delimiter):])
	}
	if index := strings.LastIndex(folder.Path, "/"); index >= 0 {
		add(folder.Path[index+1:])
	}
	if index := strings.LastIndex(folder.Path, "."); index >= 0 {
		add(folder.Path[index+1:])
	}
	return candidates
}

// FolderRole is the resolved role for one remote folder.
type FolderRole struct {
	// Role is one of inbox, sent, drafts, trash, junk, archive or custom.
	Role string
	// Skip marks provider folders that must not be synced.
	Skip bool
}

// AssignRoles maps folders to roles. A configured path wins, then a
// special-use flag, then a conventional name. Only one folder keeps each role;
// the remaining folders become custom. A configured path that no longer exists
// leaves its role unassigned instead of silently picking another folder.
func AssignRoles(folders []imapclient.Folder, cfg FolderConfig) []FolderRole {
	out := make([]FolderRole, len(folders))
	assigned := make([]string, len(folders))
	skipped := make([]bool, len(folders))
	chosen := make(map[string]int, len(SpecialRoleNames)+1)
	sources := make(map[string]source, len(SpecialRoleNames)+1)

	choose := func(role string, index int, matched source) {
		if role == "" || role == "custom" {
			return
		}
		current, ok := chosen[role]
		if !ok || matched > sources[role] ||
			(matched == sources[role] && folders[index].Path < folders[current].Path) {
			chosen[role] = index
			sources[role] = matched
		}
	}

	if !cfg.AutoDetect {
		for role, path := range cfg.SpecialFolders {
			path = strings.TrimSpace(path)
			if path == "" || !IsSpecialRole(role) {
				continue
			}
			for i := range folders {
				if folders[i].Path == path {
					choose(role, i, sourceManual)
					break
				}
			}
		}
	}

	for i, folder := range folders {
		role, matched, skip := detectRole(folder)
		if skip {
			skipped[i] = true
			continue
		}
		if role == "inbox" {
			choose(role, i, matched)
			continue
		}
		if !cfg.AutoDetect {
			continue
		}
		choose(role, i, matched)
	}

	// The inbox can never be taken by a special role, and a folder can only
	// hold one role.
	if inbox, ok := chosen["inbox"]; ok {
		for role, index := range chosen {
			if role != "inbox" && role != "custom" && index == inbox {
				delete(chosen, role)
				delete(sources, role)
			}
		}
	}
	order := append([]string{"inbox"}, SpecialRoleNames...)
	sort.SliceStable(order, func(a, b int) bool {
		if sources[order[a]] != sources[order[b]] {
			return sources[order[a]] > sources[order[b]]
		}
		return order[a] < order[b]
	})
	for _, role := range order {
		index, ok := chosen[role]
		if !ok || assigned[index] != "" {
			continue
		}
		assigned[index] = role
	}

	for i := range folders {
		if skipped[i] {
			out[i] = FolderRole{Skip: true}
			continue
		}
		role := assigned[i]
		if role == "" {
			role = "custom"
		}
		out[i] = FolderRole{Role: role}
	}
	return out
}

// ParseSpecialFolders decodes the per-account role-to-path map, ignoring
// unknown roles and blank paths.
func ParseSpecialFolders(raw []byte) map[string]string {
	out := make(map[string]string)
	if len(raw) == 0 {
		return out
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		return out
	}
	for role, path := range values {
		path = strings.TrimSpace(path)
		if path == "" || !IsSpecialRole(role) {
			continue
		}
		out[role] = path
	}
	return out
}

// EncodeSpecialFolders normalizes and serializes the per-account
// role-to-path map for the accounts table.
func EncodeSpecialFolders(values map[string]string) []byte {
	out := make(map[string]string, len(values))
	for role, path := range values {
		path = strings.TrimSpace(path)
		if path == "" || !IsSpecialRole(role) {
			continue
		}
		out[role] = path
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return []byte("{}")
	}
	return raw
}
