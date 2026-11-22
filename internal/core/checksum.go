package core

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
)

// hashBufSize is deliberately larger than io.Copy's default 32KB: on a
// cache hit we re-hash the whole target so the agent gets a digest of the
// bytes actually on disk, and that hash is pure latency on the hot path.
const hashBufSize = 1 << 20

// ParseChecksum accepts only "sha256:<hex>".
func ParseChecksum(s string) (algo, hexValue string, err error) {
	algo, hexValue, ok := strings.Cut(s, ":")
	if !ok || algo != "sha256" {
		return "", "", errors.New(`checksum must be "sha256:<64 hex chars>"`)
	}
	hexValue = strings.ToLower(strings.TrimSpace(hexValue))
	if len(hexValue) != 64 || !isHex(hexValue) {
		return "", "", errors.New(`checksum must be "sha256:<64 hex chars>"`)
	}
	return algo, hexValue, nil
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func FileSha256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, hashBufSize)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
