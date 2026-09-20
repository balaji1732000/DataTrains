package id

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func New(prefix string) (string, error) {
	if prefix == "" {
		return "", fmt.Errorf("ID prefix is required")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate ID: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(random[:]), nil
}
