package message

import (
	"strings"
)

// DedupeKey builds the stable account-scoped key used by unified views.
// Priority: provider message ID, Message-ID (+ raw hash when known), raw hash, UID.
func DedupeKey(providerMsgID, messageIDHeader, rawSHA256, mailboxID string, uid *int64) string {
	if id := strings.TrimSpace(providerMsgID); id != "" {
		return "p:" + id
	}
	if id := normalizeMessageID(messageIDHeader); id != "" {
		if rawSHA256 != "" {
			return "m:" + id + ":" + rawSHA256[:min(12, len(rawSHA256))]
		}
		return "m:" + id
	}
	if rawSHA256 != "" {
		return "h:" + rawSHA256
	}
	if uid != nil {
		return "u:" + mailboxID + ":" + itoa(*uid)
	}
	return "u:" + mailboxID + ":0"
}

// NormalizeMessageID strips angle brackets and whitespace and lowercases.
func normalizeMessageID(raw string) string {
	id := strings.TrimSpace(raw)
	id = strings.TrimPrefix(id, "<")
	id = strings.TrimSuffix(id, ">")
	return strings.ToLower(strings.TrimSpace(id))
}

// NormalizeMessageID exposes normalization to sync.
func NormalizeMessageID(raw string) string { return normalizeMessageID(raw) }

// SubjectThreadKey strips common reply/forward prefixes for thread grouping.
func SubjectThreadKey(subject string) string {
	s := strings.TrimSpace(subject)
	for {
		lower := strings.ToLower(s)
		switch {
		case strings.HasPrefix(lower, "re:"):
			s = strings.TrimSpace(s[3:])
		case strings.HasPrefix(lower, "fwd:"):
			s = strings.TrimSpace(s[4:])
		case strings.HasPrefix(lower, "fw:"):
			s = strings.TrimSpace(s[3:])
		default:
			if s == "" {
				return ""
			}
			return strings.ToLower(s)
		}
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
