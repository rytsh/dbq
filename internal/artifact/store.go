// Package artifact stores generated exports outside MCP tool responses.
package artifact

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrTooLarge is returned when an export exceeds the configured byte limit.
var ErrTooLarge = errors.New("export exceeds the byte limit")

// ErrCapacity is returned when aggregate export capacity is exhausted.
var ErrCapacity = errors.New("export store capacity exhausted")

const (
	// DefaultTTL is how long a generated export remains downloadable.
	DefaultTTL = 15 * time.Minute
	// DefaultMaxBytes bounds one artifact when the operator does not set a limit.
	DefaultMaxBytes = 100 * 1024 * 1024
	// DefaultMaxTotalBytes bounds all live artifacts together.
	DefaultMaxTotalBytes = 500 * 1024 * 1024
	// DefaultMaxFiles bounds the number of live artifacts.
	DefaultMaxFiles = 100
	// DefaultMaxConcurrent bounds simultaneous database exports.
	DefaultMaxConcurrent = 2
	privateDirMode       = 0o700
	randomIDBytes        = 24
	maxSweepInterval     = time.Minute
)

// Metadata is the small, data-free description returned to MCP clients.
type Metadata struct {
	ID        string    `json:"export_id"`
	Filename  string    `json:"filename"`
	Size      int64     `json:"size_bytes"`
	SHA256    string    `json:"sha256"`
	ExpiresAt time.Time `json:"expires_at"`
}

type entry struct {
	Metadata
	path string
}

// Store owns short-lived export files and serves them by unguessable ID.
type Store struct {
	root          string
	ttl           time.Duration
	maxBytes      int64
	maxTotalBytes int64
	maxFiles      int
	slots         chan struct{}
	ownedRoot     bool
	stop          chan struct{}
	done          chan struct{}
	closeOnce     sync.Once
	active        sync.WaitGroup

	mu         sync.Mutex
	entries    map[string]entry
	totalBytes int64
	creating   int
	reserved   int64
	closed     bool
}

// NewStore creates an export store. An empty root gets a private temporary directory.
func NewStore(root string, ttl time.Duration, maxBytes, maxTotalBytes int64, maxFiles, maxConcurrent int) (*Store, error) {
	var err error
	ownedRoot := root == ""
	if root == "" {
		root, err = os.MkdirTemp("", "dbq-exports-")
	} else {
		err = os.MkdirAll(root, privateDirMode)
		if err == nil {
			err = os.Chmod(root, privateDirMode)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("creating export directory: %w", err)
	}

	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if maxTotalBytes <= 0 {
		maxTotalBytes = DefaultMaxTotalBytes
	}
	if maxFiles <= 0 {
		maxFiles = DefaultMaxFiles
	}
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultMaxConcurrent
	}

	store := &Store{
		root: root, ttl: ttl, maxBytes: maxBytes, maxTotalBytes: maxTotalBytes,
		maxFiles: maxFiles, slots: make(chan struct{}, maxConcurrent), ownedRoot: ownedRoot,
		stop: make(chan struct{}), done: make(chan struct{}), entries: map[string]entry{},
	}
	store.removeOrphans()
	go store.runJanitor()

	return store, nil
}

// Create writes an artifact privately and publishes it only after write succeeds.
func (s *Store) Create(ctx context.Context, filename string, write func(io.Writer) error) (*Metadata, error) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.stop:
		return nil, fmt.Errorf("export store is closed")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		return nil, fmt.Errorf("export store is closed")
	}
	s.active.Add(1)
	defer s.active.Done()
	s.removeExpiredLocked(time.Now())
	if len(s.entries)+s.creating >= s.maxFiles || s.totalBytes+s.reserved+s.maxBytes > s.maxTotalBytes {
		s.mu.Unlock()

		return nil, ErrCapacity
	}
	s.creating++
	s.reserved += s.maxBytes
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.creating--
		s.reserved -= s.maxBytes
		s.mu.Unlock()
	}()

	id, err := randomID()
	if err != nil {
		return nil, err
	}

	filename = safeFilename(filename)
	temporary, err := os.CreateTemp(s.root, ".partial-*")
	if err != nil {
		return nil, fmt.Errorf("creating export file: %w", err)
	}

	tempPath := temporary.Name()
	defer func() { _ = os.Remove(tempPath) }()

	digest := sha256.New()
	counter := &limitWriter{writers: []io.Writer{temporary, digest}, max: s.maxBytes}
	if err := write(counter); err != nil {
		_ = temporary.Close()

		return nil, err
	}

	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()

		return nil, fmt.Errorf("syncing export file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return nil, fmt.Errorf("closing export file: %w", err)
	}

	path := filepath.Join(s.root, id+"-"+filename)
	if err := os.Rename(tempPath, path); err != nil {
		return nil, fmt.Errorf("publishing export file: %w", err)
	}

	metadata := Metadata{
		ID:        id,
		Filename:  filename,
		Size:      counter.written,
		SHA256:    hex.EncodeToString(digest.Sum(nil)),
		ExpiresAt: time.Now().Add(s.ttl).UTC(),
	}

	s.mu.Lock()
	s.removeExpiredLocked(time.Now())
	s.entries[id] = entry{Metadata: metadata, path: path}
	s.totalBytes += metadata.Size
	s.mu.Unlock()

	return &metadata, nil
}

// ServeHTTP downloads an artifact. The random ID is a short-lived capability.
func (s *Store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	id := strings.Split(strings.TrimPrefix(r.PathValue("*"), "/"), "/")[0]

	s.mu.Lock()
	s.removeExpiredLocked(time.Now())
	item, ok := s.entries[id]
	s.mu.Unlock()

	if !ok {
		http.NotFound(w, r)

		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", item.Filename))
	w.Header().Set("Content-Type", "application/sql")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, item.path)
}

func (s *Store) removeExpiredLocked(now time.Time) {
	for id, item := range s.entries {
		if now.Before(item.ExpiresAt) {
			continue
		}

		_ = os.Remove(item.path)
		s.totalBytes -= item.Size
		delete(s.entries, id)
	}
}

func (s *Store) runJanitor() {
	interval := s.ttl / 2
	if interval <= 0 || interval > maxSweepInterval {
		interval = maxSweepInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer close(s.done)

	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			s.removeExpiredLocked(time.Now())
			s.mu.Unlock()
		case <-s.stop:
			return
		}
	}
}

// Close stops cleanup and removes every artifact created by this process.
func (s *Store) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.stop)
		<-s.done
		s.active.Wait()

		s.mu.Lock()
		for id, item := range s.entries {
			closeErr = errors.Join(closeErr, os.Remove(item.path))
			delete(s.entries, id)
		}
		s.totalBytes = 0
		s.mu.Unlock()

		if s.ownedRoot {
			closeErr = errors.Join(closeErr, os.RemoveAll(s.root))
		}
	})

	return closeErr
}

func (s *Store) removeOrphans() {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return
	}

	for _, item := range entries {
		name := item.Name()
		if strings.HasPrefix(name, ".partial-") || generatedFilename(name) {
			_ = os.Remove(filepath.Join(s.root, name))
		}
	}
}

func generatedFilename(name string) bool {
	prefixLength := randomIDBytes * 2
	if len(name) <= prefixLength || name[prefixLength] != '-' {
		return false
	}

	_, err := hex.DecodeString(name[:prefixLength])

	return err == nil
}

func randomID() (string, error) {
	value := make([]byte, randomIDBytes)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generating export ID: %w", err)
	}

	return hex.EncodeToString(value), nil
}

func safeFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r < 32 {
			return '-'
		}

		return r
	}, name)

	if name == "" || name == "." {
		return "export.sql"
	}
	if !strings.HasSuffix(strings.ToLower(name), ".sql") {
		name += ".sql"
	}

	return name
}

type limitWriter struct {
	writers []io.Writer
	max     int64
	written int64
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.max > 0 && w.written+int64(len(p)) > w.max {
		return 0, fmt.Errorf("%w (%d bytes)", ErrTooLarge, w.max)
	}

	n, err := io.MultiWriter(w.writers...).Write(p)
	w.written += int64(n)

	return n, err
}
