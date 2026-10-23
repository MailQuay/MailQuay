package httpapi

import (
	"net/http"

	"github.com/mailquay/mailquay/internal/httpapi/oapi"
)

func (s *Server) GetMailboxTree(w http.ResponseWriter, r *http.Request) {
	tree, err := s.mailboxes.Tree(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := oapi.MailboxTree{
		Accounts: make([]oapi.AccountMailboxes, 0, len(tree.Accounts)),
		Counts: oapi.MailboxCounts{
			All: int(tree.Counts.All), Unread: int(tree.Counts.Unread), Marked: int(tree.Counts.Marked),
			Drafts: int(tree.Counts.Drafts), Sent: int(tree.Counts.Sent), Trash: int(tree.Counts.Trash),
		},
	}
	for _, group := range tree.Accounts {
		folders := make([]oapi.Mailbox, 0, len(group.Folders))
		for _, mb := range group.Folders {
			folders = append(folders, mapMailbox(mb))
		}
		other := make([]oapi.Mailbox, 0, len(group.Other))
		for _, mb := range group.Other {
			other = append(other, mapMailbox(mb))
		}
		out.Accounts = append(out.Accounts, oapi.AccountMailboxes{
			Account: mapAccount(group.Account), Folders: folders, Other: other,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) GetMailbox(w http.ResponseWriter, r *http.Request, id oapi.MailboxID) {
	mailboxID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid mailbox id.")
		return
	}
	mb, err := s.mailboxes.Get(r.Context(), mailboxID)
	if err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mapMailbox(mb))
}

func (s *Server) SyncMailbox(w http.ResponseWriter, r *http.Request, id oapi.MailboxID) {
	mailboxID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid mailbox id.")
		return
	}
	if err := s.sync.TriggerMailbox(r.Context(), mailboxID); err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
