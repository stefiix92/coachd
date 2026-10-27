// Package garminkey holds the Garmin Personal API key in use and its expiry:
// the configured key, or one the runner renewed on the page and coachd stored
// in the data directory, whichever expires later. The key never leaves this
// package except to the Garmin client.
package garminkey

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/config"
)

// FileName is the renewed key's file in the data directory.
const FileName = "garmin-key.json"

// Key is a Garmin Personal API key and the last day it is valid.
type Key struct {
	Value   string     `json:"key"`
	Expires civil.Date `json:"expires"`
}

// Exchanger checks a key with Garmin and, once commit has succeeded, uses it.
// garmin.Client implements it.
type Exchanger interface {
	UseKey(ctx context.Context, key string, commit func() error) error
}

// Holder is the key in use. It is safe for concurrent use.
type Holder struct {
	path string
	mu   sync.RWMutex // guards k; held across Renew so renewals do not interleave
	k    Key
}

// Load returns a holder with the configured key or the renewed key stored in
// dataDir, whichever expires later (the configured one on a tie). A renewed
// key file that is unreadable, group- or world-readable, or malformed is an
// error, never a silent fallback. Errors never include the key.
func Load(dataDir string, configured Key) (*Holder, error) {
	h := &Holder{path: filepath.Join(dataDir, FileName), k: configured}
	s, err := config.ReadSecretFile(h.path)
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, fmt.Errorf("garminkey: %s: %w", FileName, err)
	}
	var stored Key
	if err := json.Unmarshal([]byte(s), &stored); err != nil || stored.Value == "" || stored.Expires == (civil.Date{}) {
		return nil, fmt.Errorf("garminkey: %s is malformed", FileName)
	}
	if configured.Expires.Before(stored.Expires) {
		h.k = stored
	}
	return h, nil
}

// NoKey returns a holder without a key whose expiry is expires, for demo
// mode, which calls no Garmin. It reads nothing from disk and never renews.
func NoKey(expires civil.Date) *Holder { return &Holder{k: Key{Expires: expires}} }

// Value is the key in use.
func (h *Holder) Value() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.k.Value
}

// Expires is the last day the key in use is valid.
func (h *Holder) Expires() civil.Date {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.k.Expires
}

// Renew makes g check k with Garmin; when Garmin accepts it, k is written to
// the data directory and then becomes the key in use, for g too. On any
// error nothing changes.
func (h *Holder) Renew(ctx context.Context, g Exchanger, k Key) error {
	if h.path == "" {
		return errors.New("garminkey: no key to renew")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return g.UseKey(ctx, k.Value, func() error {
		if err := h.write(k); err != nil {
			return err
		}
		h.k = k
		return nil
	})
}

// write stores k atomically with mode 0600.
func (h *Holder) write(k Key) error {
	b, err := json.Marshal(k)
	if err != nil {
		return fmt.Errorf("garminkey: encode: %w", err)
	}
	dir := filepath.Dir(h.path)
	tmp, err := os.CreateTemp(dir, ".garmin-key-*.tmp") // created 0600
	if err != nil {
		return fmt.Errorf("garminkey: temp file: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("garminkey: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("garminkey: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("garminkey: close: %w", err)
	}
	if err := os.Rename(tmp.Name(), h.path); err != nil {
		return fmt.Errorf("garminkey: rename: %w", err)
	}
	ok = true
	if d, err := os.Open(dir); err == nil { // best effort: persist the rename
		d.Sync()
		d.Close()
	}
	return nil
}
