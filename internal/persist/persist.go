// Package persist implements the state file: load and save key and proxy
// health by SHA256 hash, atomic write, debounced flush, and immediate flush on
// retirement.
//
// persist is an observer package: it takes a State as data and round-trips it.
// It imports only the standard library and yaml; it does not import keypool or
// proxy, which would be an upward edge. The app layer maps between the pool
// and proxy snapshots and this package's State.
package persist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// KeyRecord is one persisted key's health state.
type KeyRecord struct {
	Provider          string     `json:"provider" yaml:"provider"`
	KeyHash           string     `json:"key_hash" yaml:"key_hash"` // "sha256:" + hex
	ConsecutiveErrors int        `json:"consecutiveErrors" yaml:"consecutiveErrors"`
	ExhaustedAt       *time.Time `json:"exhaustedAt,omitempty" yaml:"exhaustedAt,omitempty"`
	Retired           bool       `json:"retired,omitempty" yaml:"retired,omitempty"`
	RetiredAt         *time.Time `json:"retiredAt,omitempty" yaml:"retiredAt,omitempty"`
	Reason            string     `json:"reason,omitempty" yaml:"reason,omitempty"`
}

// ProxyRecord is one persisted proxy's health state.
type ProxyRecord struct {
	URL               string     `json:"url" yaml:"url"`
	ConsecutiveErrors int        `json:"consecutiveErrors" yaml:"consecutiveErrors"`
	DeadUntil         *time.Time `json:"deadUntil,omitempty" yaml:"deadUntil,omitempty"`
}

// State is the persisted snapshot.
type State struct {
	Keys    []KeyRecord   `json:"keys" yaml:"keys"`
	Proxies []ProxyRecord `json:"proxies" yaml:"proxies"`
}

// HashKey returns the key hash: "sha256:" + hex(sha256(key)).
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Store loads and saves the state file.
type Store struct {
	path string

	mu       sync.Mutex
	current  State
	dirty    bool
	flushing bool
	debounce time.Duration
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// New builds a Store. When path is empty, persistence is disabled: Load
// returns an empty State and Save is a no-op.
func New(path string) *Store {
	return &Store{path: path, debounce: time.Second, stopCh: make(chan struct{}), doneCh: make(chan struct{})}
}

// Load reads the state file. It returns an empty State when the file is absent
// or path is empty. JSON or YAML is inferred from the extension.
func (s *Store) Load() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return State{}, nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("read state %q: %w", s.path, err)
	}
	var st State
	ext := filepath.Ext(s.path)
	switch ext {
	case ".json":
		if err := json.Unmarshal(data, &st); err != nil {
			return State{}, fmt.Errorf("parse state json: %w", err)
		}
	default: // .yaml or .yml
		if err := yaml.Unmarshal(data, &st); err != nil {
			return State{}, fmt.Errorf("parse state yaml: %w", err)
		}
	}
	s.current = st
	return st, nil
}

// Set updates the in-memory state and marks it dirty for a debounced save.
func (s *Store) Set(st State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return
	}
	s.current = st
	s.dirty = true
}

// FlushImmediately forces an immediate atomic write, used on retirement
// transitions.
func (s *Store) FlushImmediately() {
	s.mu.Lock()
	if s.path == "" {
		s.mu.Unlock()
		return
	}
	s.dirty = true
	s.mu.Unlock()
	s.flushNow()
}

// StartFlusher runs the debounced flush loop. It stops on ctx cancellation or
// Stop.
func (s *Store) StartFlusher(ctx context.Context) {
	defer close(s.doneCh)
	if s.path == "" {
		return
	}
	t := time.NewTicker(s.debounce)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flushNow()
			return
		case <-s.stopCh:
			s.flushNow()
			return
		case <-t.C:
			s.flushNow()
		}
	}
}

// Stop signals the flusher to flush and exit, and waits for it to finish.
func (s *Store) Stop() {
	select {
	case <-s.stopCh:
		return // already stopped
	default:
	}
	close(s.stopCh)
	<-s.doneCh
}

// flushNow writes the current state when dirty, atomically with a temp file
// and rename.
func (s *Store) flushNow() {
	s.mu.Lock()
	if !s.dirty || s.path == "" || s.flushing {
		s.mu.Unlock()
		return
	}
	s.flushing = true
	st := s.current
	s.dirty = false
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.flushing = false
		s.mu.Unlock()
	}()

	ext := filepath.Ext(s.path)
	var data []byte
	var err error
	switch ext {
	case ".json":
		data, err = json.MarshalIndent(st, "", "  ")
	default:
		data, err = yaml.Marshal(st)
	}
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}
