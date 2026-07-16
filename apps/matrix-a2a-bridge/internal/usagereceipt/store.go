package usagereceipt

import (
	"bufio"
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

const maxArchivedReceiptBytes = 128 << 10

// Archive appends signed receipts without rewriting prior evidence.
type Archive struct {
	Path string
	mu   sync.Mutex
}

// Find returns the single archived receipt for a task when present.
func (a *Archive) Find(taskID string) (Signed, bool, error) {
	if a == nil || a.Path == "" {
		return Signed{}, false, fmt.Errorf("usage receipt archive path is empty")
	}
	if !validIdentifier(taskID) {
		return Signed{}, false, fmt.Errorf("usage receipt archive task ID is invalid")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.findLocked(taskID)
}

// AppendUnique durably writes at most one compact signed receipt per task. Concurrent or retried
// terminal responses reuse the first assertion instead of minting a new timestamp and signature.
func (a *Archive) AppendUnique(signed Signed) (Signed, error) {
	if a == nil || a.Path == "" {
		return Signed{}, fmt.Errorf("usage receipt archive path is empty")
	}
	encoded, err := Marshal(signed)
	if err != nil {
		return Signed{}, err
	}
	encoded = append(encoded, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	existing, found, err := a.findLocked(signed.Receipt.TaskID)
	if err != nil {
		return Signed{}, err
	}
	if found {
		if !sameAssertion(existing.Receipt, signed.Receipt) {
			return Signed{}, fmt.Errorf("usage receipt archive conflicts with existing task ID")
		}
		return existing, nil
	}
	if err := os.MkdirAll(filepath.Dir(a.Path), 0o700); err != nil {
		return Signed{}, fmt.Errorf("create usage receipt archive directory: %w", err)
	}
	file, created, err := openArchive(a.Path)
	if err != nil {
		return Signed{}, err
	}
	if _, err := file.Write(encoded); err != nil {
		return Signed{}, errors.Join(
			fmt.Errorf("append usage receipt archive: %w", err),
			closeFile(file, "usage receipt archive"),
		)
	}
	if err := file.Sync(); err != nil {
		return Signed{}, errors.Join(
			fmt.Errorf("sync usage receipt archive: %w", err),
			closeFile(file, "usage receipt archive"),
		)
	}
	if err := file.Close(); err != nil {
		return Signed{}, fmt.Errorf("close usage receipt archive: %w", err)
	}
	if created {
		if err := syncDirectory(filepath.Dir(a.Path), "usage receipt archive"); err != nil {
			return Signed{}, err
		}
	}
	return signed, nil
}

func (a *Archive) findLocked(taskID string) (_ Signed, _ bool, returnErr error) {
	file, err := os.Open(a.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Signed{}, false, nil
	}
	if err != nil {
		return Signed{}, false, fmt.Errorf("open usage receipt archive: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, closeFile(file, "usage receipt archive"))
	}()

	var found Signed
	matched := false
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), maxArchivedReceiptBytes)
	for scanner.Scan() {
		signed, parseErr := Parse(scanner.Bytes())
		if parseErr != nil {
			return Signed{}, false, fmt.Errorf("parse usage receipt archive: %w", parseErr)
		}
		if signed.Receipt.TaskID != taskID {
			continue
		}
		if matched {
			return Signed{}, false, fmt.Errorf("usage receipt archive has duplicate task ID")
		}
		found = signed
		matched = true
	}
	if err := scanner.Err(); err != nil {
		return Signed{}, false, fmt.Errorf("scan usage receipt archive: %w", err)
	}
	return found, matched, nil
}

func openArchive(path string) (*os.File, bool, error) {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		return file, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("open usage receipt archive: %w", err)
	}
	file, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("create usage receipt archive: %w", err)
	}
	return file, true, nil
}

func sameAssertion(first, second Receipt) bool {
	return first.Schema == second.Schema && first.AZP == second.AZP &&
		first.TaskID == second.TaskID && first.ContextID == second.ContextID &&
		first.RequestHash == second.RequestHash && first.TokensReserved == second.TokensReserved &&
		first.TokensConsumed == nil && second.TokensConsumed == nil &&
		first.Outcome == second.Outcome && first.KeyID == second.KeyID
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
	if !hashRE.MatchString(evidence.RequestHash) ||
		!validTokenReservation(evidence.TokensReserved) {
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
	return syncDirectory(s.Dir, "pending usage receipt")
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
		!hashRE.MatchString(evidence.RequestHash) ||
		!validTokenReservation(evidence.TokensReserved) {
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

func syncDirectory(path, label string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s directory: %w", label, err)
	}
	if err := directory.Sync(); err != nil {
		return errors.Join(
			fmt.Errorf("sync %s directory: %w", label, err),
			closeFile(directory, label+" directory"),
		)
	}
	return closeFile(directory, label+" directory")
}

// Delete removes reservation evidence after its terminal receipt has been archived.
func (s *PendingStore) Delete(taskID string) error {
	path, err := s.path(taskID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove pending usage receipt evidence: %w", err)
	}
	return syncDirectory(s.Dir, "pending usage receipt")
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
