package manifest

import (
	"crypto/sha256"
	"encoding/hex"
)

// Signature is a content fingerprint that lets a re-run tell an unchanged
// object from an updated one without diffing files.
//
// Create instances with [SignatureOf], or set the fields directly when only a
// size is known.
type Signature struct {
	// Hash is the lowercase hex SHA-256 of the content, or empty when only the
	// size is recorded.
	Hash string `json:"hash,omitempty"`
	// Size is the content length in bytes.
	Size int64 `json:"size"`
}

// SignatureOf computes the [Signature] of data.
//
// It records the byte length and the lowercase hex SHA-256 digest. The digest
// identifies content, not secrets, so SHA-256 is used for its collision
// resistance, not as a security control.
func SignatureOf(data []byte) Signature {
	sum := sha256.Sum256(data)

	return Signature{
		Hash: hex.EncodeToString(sum[:]),
		Size: int64(len(data)),
	}
}

// Equal reports whether two signatures describe the same content.
//
// When either hash is empty the comparison falls back to size alone, so a
// size-only signature still detects a changed length.
func (s Signature) Equal(other Signature) bool {
	if s.Hash != "" && other.Hash != "" {
		return s.Hash == other.Hash
	}

	return s.Size == other.Size
}
