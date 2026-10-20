package platform

import (
	"fmt"

	"github.com/google/uuid"
)

// NewID returns a time-ordered UUIDv7 identifier.
func NewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		panic(fmt.Sprintf("platform: uuidv7: %v", err))
	}
	return id.String()
}

// ParseID validates an external identifier and returns a UUID.
func ParseID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid id: %w", err)
	}
	return id, nil
}
