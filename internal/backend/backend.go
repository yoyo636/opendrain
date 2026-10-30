package backend

import (
	"context"
	"sync"

	"opendrain/internal/core"
)

// cancelRegistry lets Cancel() stop an in-flight Start() by state id.
var (
	regMu    sync.Mutex
	registry = map[string]context.CancelFunc{}
)

func trackCancel(id string, cf context.CancelFunc) {
	regMu.Lock()
	registry[id] = cf
	regMu.Unlock()
}

func cancelByID(id string) error {
	regMu.Lock()
	cf, ok := registry[id]
	regMu.Unlock()
	if !ok {
		return core.Fail(core.CodeNotFound, "download not running in this process", nil)
	}
	cf()
	return nil
}

func statusFromStore(id string) (*core.State, error) {
	store, err := core.NewStore()
	if err != nil {
		return nil, err
	}
	st, err := store.Get(id)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, core.Fail(core.CodeNotFound, "no state for id "+id, nil)
	}
	return st, nil
}
