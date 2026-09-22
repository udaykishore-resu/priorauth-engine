package rules

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// Store holds the active rule Set and swaps it atomically on reload, so a
// determination in flight always sees one consistent set.
type Store struct {
	cur     atomic.Pointer[Set]
	onSwap  func(old, next *Set)
	reloads atomic.Int64
}

// NewStore creates a store with an initial set.
func NewStore(initial *Set) *Store {
	s := &Store{}
	s.cur.Store(initial)
	return s
}

// OnSwap registers a callback invoked after every successful Replace.
func (s *Store) OnSwap(fn func(old, next *Set)) { s.onSwap = fn }

// Current returns the active set. Never nil after NewStore.
func (s *Store) Current() *Set { return s.cur.Load() }

// Replace swaps in a new set.
func (s *Store) Replace(next *Set) {
	if next == nil {
		return
	}
	old := s.cur.Swap(next)
	s.reloads.Add(1)
	if s.onSwap != nil {
		s.onSwap(old, next)
	}
}

// Reloads returns how many successful swaps happened (for metrics).
func (s *Store) Reloads() int64 { return s.reloads.Load() }

// DirLoader loads rule files from a directory on the local filesystem and
// detects changes by comparing a fingerprint of (name, size, mtime) of the
// files, which is portable and needs no inotify dependency.
type DirLoader struct {
	Dir string
	Now func() time.Time
}

// Load reads the directory into a Set.
func (l DirLoader) Load() (*Set, error) {
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	abs, err := filepath.Abs(l.Dir)
	if err != nil {
		return nil, fmt.Errorf("rules: resolve %q: %w", l.Dir, err)
	}
	return LoadFS(os.DirFS(abs), ".", now().UTC())
}

// Fingerprint returns a string that changes whenever any rule file changes.
func (l DirLoader) Fingerprint() (string, error) {
	entries, err := os.ReadDir(l.Dir)
	if err != nil {
		return "", err
	}
	var fp string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return "", err
		}
		fp += fmt.Sprintf("%s:%d:%d;", e.Name(), info.Size(), info.ModTime().UnixNano())
	}
	return fp, nil
}

// Watch polls the directory every interval and hot-reloads the store when
// the fingerprint changes. A set that fails validation is logged and the
// previous set stays active (fail-safe). Watch returns when ctx is done.
func (s *Store) Watch(ctx context.Context, loader DirLoader, interval time.Duration, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	last, _ := loader.Fingerprint()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		fp, err := loader.Fingerprint()
		if err != nil {
			log.Warn("rules watch: fingerprint", "err", err)
			continue
		}
		if fp == last {
			continue
		}
		set, err := loader.Load()
		if err != nil {
			log.Error("rules reload rejected; keeping previous set", "err", err)
			last = fp // don't spam on the same broken files
			continue
		}
		s.Replace(set)
		last = fp
		log.Info("rules reloaded", "hash", set.Hash, "count", len(set.Rules))
	}
}
