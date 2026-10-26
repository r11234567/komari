// Package upload provides the shared chunked archive upload flow.
package upload

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const ChunkSize int64 = 5 * 1024 * 1024

type Purpose string

const (
	PurposeBackup Purpose = "backup"
	PurposePlugin Purpose = "plugin"
	PurposeTheme  Purpose = "theme"
)

var ErrNotFound = errors.New("upload not found")

type Metadata struct {
	Purpose  Purpose `json:"purpose"`
	Size     int64   `json:"size"`
	Filename string  `json:"filename"`
}

type Session struct {
	ID          string
	Metadata    Metadata
	Directory   string
	ArchivePath string
}

const Lifetime = 30 * time.Minute
const MaxStagedBytes int64 = 512 << 20

type ownedSession struct {
	owner   [32]byte
	size    int64
	expires time.Time
}

// Operations serialize disk writes, retry replacement, merge/finalize and cancel.
// Multipart bodies are bounded and parsed before acquiring this lock.
type Store struct {
	mu          sync.Mutex
	Root        string
	MaxSize     int64
	sessions    map[string]ownedSession
	initialized bool
	cleanupOnce sync.Once
	now         func() time.Time
}

func NewStore(root string) *Store {
	return &Store{Root: root, MaxSize: 256 << 20, sessions: make(map[string]ownedSession), now: time.Now}
}

var DefaultStore = NewStore(filepath.Join(".", "data", ".uploading"))

func (s *Store) StartCleanup() {
	s.cleanupOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				s.Sweep()
			}
		}()
	})
}
func (s *Store) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
}
func (s *Store) sweepLocked() {
	for id, session := range s.sessions {
		if !s.now().Before(session.expires) {
			s.removeLocked(id)
		}
	}
}
func (s *Store) removeLocked(id string) {
	_ = os.RemoveAll(filepath.Join(s.Root, id))
	delete(s.sessions, id)
}
func (s *Store) ownLocked(owner, id string) error {
	session, ok := s.sessions[id]
	if !ok || session.owner != sha256.Sum256([]byte(owner)) {
		return ErrNotFound
	}
	if !s.now().Before(session.expires) {
		s.removeLocked(id)
		return ErrNotFound
	}
	return nil
}

func (s *Store) Init(owner string, purpose Purpose, filename string, size int64) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner == "" {
		return Session{}, ErrNotFound
	}
	if !s.initialized {
		if err := os.MkdirAll(s.Root, 0700); err != nil {
			return Session{}, err
		}
		entries, err := os.ReadDir(s.Root)
		if err != nil {
			return Session{}, err
		}
		// Sessions never survive a process restart. Remove only this store's own
		// generated directories; upload IDs never become caller-controlled paths.
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "upload-") {
				if err := os.RemoveAll(filepath.Join(s.Root, entry.Name())); err != nil {
					return Session{}, err
				}
			}
		}
		s.initialized = true
	}
	s.sweepLocked()
	if !isKnownPurpose(purpose) {
		return Session{}, fmt.Errorf("invalid upload purpose")
	}
	limit := s.MaxSize
	if purpose == PurposePlugin && limit > 100<<20 {
		limit = 100 << 20
	}
	if purpose == PurposeTheme && limit > 128<<20 {
		limit = 128 << 20
	}
	if size <= 0 || size > limit {
		return Session{}, fmt.Errorf("size must be between 1 and %d bytes", limit)
	}

	ownerKey := sha256.Sum256([]byte(owner))
	var total int64
	count := 0
	for _, session := range s.sessions {
		total += session.size
		if session.owner == ownerKey {
			count++
		}
	}
	if len(s.sessions) >= 4 || count >= 2 || total+size > MaxStagedBytes {
		return Session{}, fmt.Errorf("upload staging quota exceeded")
	}
	id := "upload-" + uuid.NewString()
	directory := filepath.Join(s.Root, id)
	if err := os.Mkdir(directory, 0700); err != nil {
		return Session{}, err
	}

	metadata := Metadata{Purpose: purpose, Size: size, Filename: filename}
	data, err := json.Marshal(metadata)
	if err != nil {
		_ = os.RemoveAll(directory)
		return Session{}, fmt.Errorf("encode upload metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "upload.json"), data, 0600); err != nil {
		_ = os.RemoveAll(directory)
		return Session{}, fmt.Errorf("write upload metadata: %w", err)
	}
	s.sessions[id] = ownedSession{owner: ownerKey, size: size, expires: s.now().Add(Lifetime)}
	return Session{ID: id, Metadata: metadata, Directory: directory}, nil
}

func (s *Store) SaveChunk(owner, uploadID string, index int64, source io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ownLocked(owner, uploadID); err != nil {
		return err
	}
	session, err := s.load(uploadID)
	if err != nil {
		return err
	}
	expectedSize, err := expectedChunkSize(session.Metadata.Size, index)
	if err != nil {
		return err
	}

	temporary, err := os.CreateTemp(session.Directory, fmt.Sprintf(".%d-*.part", index))
	if err != nil {
		return fmt.Errorf("create chunk: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	written, copyErr := io.Copy(temporary, io.LimitReader(source, ChunkSize+1))
	closeErr := temporary.Close()
	if copyErr != nil {
		return fmt.Errorf("write chunk: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close chunk: %w", closeErr)
	}
	if written != expectedSize {
		return fmt.Errorf("chunk %d has size %d, want %d", index, written, expectedSize)
	}

	chunkPath := filepath.Join(session.Directory, chunkFilename(index))
	if err := os.Remove(chunkPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replace chunk: %w", err)
	}
	if err := os.Rename(temporaryPath, chunkPath); err != nil {
		return fmt.Errorf("publish chunk: %w", err)
	}
	return nil
}

func (s *Store) mergeLocked(uploadID string) (Session, error) {
	session, err := s.load(uploadID)
	if err != nil {
		return Session{}, err
	}

	temporary, err := os.CreateTemp(session.Directory, ".merged-*.zip")
	if err != nil {
		return Session{}, fmt.Errorf("create merged archive: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()

	for index := int64(0); index < chunkCount(session.Metadata.Size); index++ {
		chunkPath := filepath.Join(session.Directory, chunkFilename(index))
		info, err := os.Stat(chunkPath)
		if err != nil {
			if os.IsNotExist(err) {
				return Session{}, fmt.Errorf("chunk %d is missing", index)
			}
			return Session{}, fmt.Errorf("read chunk %d: %w", index, err)
		}
		expectedSize, _ := expectedChunkSize(session.Metadata.Size, index)
		if info.Size() != expectedSize {
			return Session{}, fmt.Errorf("chunk %d has size %d, want %d", index, info.Size(), expectedSize)
		}
		chunk, err := os.Open(chunkPath)
		if err != nil {
			return Session{}, fmt.Errorf("open chunk %d: %w", index, err)
		}
		_, copyErr := io.Copy(temporary, chunk)
		closeErr := chunk.Close()
		if copyErr != nil {
			return Session{}, fmt.Errorf("merge chunk %d: %w", index, copyErr)
		}
		if closeErr != nil {
			return Session{}, fmt.Errorf("close chunk %d: %w", index, closeErr)
		}
	}
	if err := temporary.Close(); err != nil {
		return Session{}, fmt.Errorf("close merged archive: %w", err)
	}

	archivePath := filepath.Join(session.Directory, "archive.zip")
	if err := os.Rename(temporaryPath, archivePath); err != nil {
		return Session{}, fmt.Errorf("publish merged archive: %w", err)
	}
	session.ArchivePath = archivePath
	return session, nil
}

// Finalize holds the lock until installation consumes the merged archive.
// Cancellation or duplicate merge cannot remove/replace an in-use archive.
func (s *Store) Finalize(owner, id string, finish func(Session) (Result, error)) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ownLocked(owner, id); err != nil {
		return Result{}, err
	}
	session, err := s.mergeLocked(id)
	if err != nil {
		return Result{}, err
	} // allow missing chunks to be retried
	defer s.removeLocked(id)
	return finish(session)
}
func (s *Store) Cancel(owner, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ownLocked(owner, id); err != nil {
		return err
	}
	s.removeLocked(id)
	return nil
}

func (s *Store) load(uploadID string) (Session, error) {
	if !validUploadID(uploadID) {
		return Session{}, fmt.Errorf("invalid upload id")
	}
	directory := filepath.Join(s.Root, uploadID)
	data, err := os.ReadFile(filepath.Join(directory, "upload.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return Session{}, ErrNotFound
		}
		return Session{}, fmt.Errorf("read upload metadata: %w", err)
	}
	var metadata Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return Session{}, fmt.Errorf("read upload metadata: %w", err)
	}
	if !isKnownPurpose(metadata.Purpose) || metadata.Size <= 0 || metadata.Size > s.MaxSize {
		return Session{}, fmt.Errorf("invalid upload metadata")
	}
	return Session{ID: uploadID, Metadata: metadata, Directory: directory}, nil
}

func isKnownPurpose(purpose Purpose) bool {
	return purpose == PurposeBackup || purpose == PurposePlugin || purpose == PurposeTheme
}

func validUploadID(uploadID string) bool {
	if !strings.HasPrefix(uploadID, "upload-") {
		return false
	}
	parsed, err := uuid.Parse(strings.TrimPrefix(uploadID, "upload-"))
	return err == nil && "upload-"+parsed.String() == uploadID
}

func chunkCount(size int64) int64 {
	return (size + ChunkSize - 1) / ChunkSize
}

func expectedChunkSize(size, index int64) (int64, error) {
	if index < 0 || index >= chunkCount(size) {
		return 0, fmt.Errorf("invalid chunk index")
	}
	if index == chunkCount(size)-1 {
		return size - index*ChunkSize, nil
	}
	return ChunkSize, nil
}

func chunkFilename(index int64) string {
	return strconv.FormatInt(index, 10) + ".part"
}
