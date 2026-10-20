package message

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type cursor struct {
	Received time.Time `json:"t"`
	ID       uuid.UUID `json:"id"`
}

// EncodeCursor returns an opaque cursor for a received_at/id pair.
func EncodeCursor(received time.Time, id uuid.UUID) string {
	raw, _ := json.Marshal(cursor{Received: received.UTC(), ID: id})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeCursor parses an opaque cursor.
func DecodeCursor(value string) (time.Time, uuid.UUID, error) {
	if value == "" {
		return time.Time{}, uuid.Nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("invalid cursor: %w", err)
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("invalid cursor: %w", err)
	}
	return c.Received, c.ID, nil
}
