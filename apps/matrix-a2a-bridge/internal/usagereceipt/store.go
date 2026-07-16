package usagereceipt

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Archive appends signed receipts without rewriting prior evidence.
type Archive struct {
	Path string
	mu   sync.Mutex
}

// Append durably writes one compact signed receipt as a JSONL record.
func (a *Archive) Append(signed Signed) error {
	if a == nil || a.Path == "" {
		return fmt.Errorf("usage receipt archive path is empty")
	}
	encoded, err := Marshal(signed)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(a.Path), 0o700); err != nil {
		return fmt.Errorf("create usage receipt archive directory: %w", err)
	}
	file, err := os.OpenFile(a.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open usage receipt archive: %w", err)
	}
	if _, err := file.Write(encoded); err != nil {
		return errors.Join(
			fmt.Errorf("append usage receipt archive: %w", err),
			closeFile(file, "usage receipt archive"),
		)
	}
	if err := file.Sync(); err != nil {
		return errors.Join(
			fmt.Errorf("sync usage receipt archive: %w", err),
			closeFile(file, "usage receipt archive"),
		)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close usage receipt archive: %w", err)
	}
	return nil
}

// PendingStore persists the initial reservation evidence for terminal GetTask responses.
type PendingStore struct {
	Dir string
}

// Save atomically persists the reservation evidence for one accepted task ID.
func (s *PendingStore) Save(taskID string, evidence pendingEvidence) (returnErr error) {
	path, err := s.path(taskID)
	if err != nil {
		return err
	}
	if !hashRE.MatchString(evidence.RequestHash) || evidence.TokensReserved == 0 {
		return fmt.Errorf("pending usage receipt evidence is invalid")
	}
	existing, found, err := s.Load(taskID)
	if err != nil {
		return err
	}
	if found {
		if existing != evidence {
			return fmt.Errorf("pending usage receipt evidence conflicts with existing task ID")
		}
		return nil
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("encode pending usage receipt evidence: %w", err)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create pending usage receipt directory: %w", err)
	}
	temporary, err := os.CreateTemp(s.Dir, ".pending-*")
	if err != nil {
		return fmt.Errorf("create pending usage receipt evidence: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove temporary pending evidence: %w", err))
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return errors.Join(
			fmt.Errorf("protect pending usage receipt evidence: %w", err),
			closeFile(temporary, "temporary pending evidence"),
		)
	}
	if _, err := temporary.Write(encoded); err != nil {
		return errors.Join(
			fmt.Errorf("write pending usage receipt evidence: %w", err),
			closeFile(temporary, "temporary pending evidence"),
		)
	}
	if err := temporary.Sync(); err != nil {
		return errors.Join(
			fmt.Errorf("sync pending usage receipt evidence: %w", err),
			closeFile(temporary, "temporary pending evidence"),
		)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close pending usage receipt evidence: %w", err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("publish pending usage receipt evidence: %w", err)
		}
		existing, found, loadErr := s.Load(taskID)
		if loadErr != nil {
			return loadErr
		}
		if !found || existing != evidence {
			return fmt.Errorf("pending usage receipt evidence conflicts with existing task ID")
		}
		return nil
	}
	return syncDirectory(s.Dir)
}

// Load returns the persisted reservation evidence for a task ID when present.
func (s *PendingStore) Load(taskID string) (pendingEvidence, bool, error) {
	path, err := s.path(taskID)
	if err != nil {
		return pendingEvidence{}, false, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return pendingEvidence{}, false, nil
	}
	if err != nil {
		return pendingEvidence{}, false, fmt.Errorf("read pending usage receipt evidence: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var evidence pendingEvidence
	if err := decoder.Decode(&evidence); err != nil || expectEOF(decoder) != nil ||
		!hashRE.MatchString(evidence.RequestHash) || evidence.TokensReserved == 0 {
		return pendingEvidence{}, false, fmt.Errorf("pending usage receipt evidence is invalid")
	}
	return evidence, true, nil
}

func closeFile(file *os.File, label string) error {
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", label, err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open pending usage receipt directory: %w", err)
	}
	if err := directory.Sync(); err != nil {
		return errors.Join(
			fmt.Errorf("sync pending usage receipt directory: %w", err),
			closeFile(directory, "pending usage receipt directory"),
		)
	}
	return closeFile(directory, "pending usage receipt directory")
}

// Delete removes reservation evidence after its terminal receipt has been archived.
func (s *PendingStore) Delete(taskID string) error {
	path, err := s.path(taskID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove pending usage receipt evidence: %w", err)
	}
	return nil
}

func (s *PendingStore) path(taskID string) (string, error) {
	if s == nil || s.Dir == "" {
		return "", fmt.Errorf("pending usage receipt directory is empty")
	}
	if !validIdentifier(taskID) {
		return "", fmt.Errorf("pending usage receipt task ID is invalid")
	}
	digest := sha256.Sum256([]byte(taskID))
	return filepath.Join(s.Dir, hex.EncodeToString(digest[:])+".json"), nil
}
