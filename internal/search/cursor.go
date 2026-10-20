package search

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// encodeCursor serializes the sort values of the last hit using base64url so
// it is safe to expose to API clients.
func encodeCursor(sortValues []string) (string, error) {
	payload, err := json.Marshal(sortValues)
	if err != nil {
		return "", fmt.Errorf("search: encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

// decodeCursor restores sort values previously returned by encodeCursor.
func decodeCursor(cursor string) ([]string, error) {
	payload, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("%w: decode base64: %w", ErrInvalidCursor, err)
	}
	var sortValues []string
	if err := json.Unmarshal(payload, &sortValues); err != nil {
		return nil, fmt.Errorf("%w: decode json: %w", ErrInvalidCursor, err)
	}
	if sortValues == nil {
		return nil, fmt.Errorf("%w: empty sort values", ErrInvalidCursor)
	}
	return sortValues, nil
}
