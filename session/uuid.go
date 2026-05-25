package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewSessionID generates a new random session ID.
func NewSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("failed to generate session id: %v", err))
	}
	return hex.EncodeToString(b)
}
