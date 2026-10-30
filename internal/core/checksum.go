package core

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
)

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
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
