package backend

import (
	"testing"

	"opendrain/internal/core"
)

func mustStore(t *testing.T) *core.Store {
	t.Helper()
	s, err := core.NewStore()
	if err != nil {
		t.Fatal(err)
	}
	return s
}
