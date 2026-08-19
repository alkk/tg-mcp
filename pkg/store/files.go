package store

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	filesDir = "files"
	// tempDir collects half-written downloads; a hex key never starts with a dot, so it cannot
	// collide with a cached attachment.
	tempDir = ".tmp"
)

// fileKey encodes a telegram file_unique_id into one path element. Hex cannot express a separator
// or a dot, so no traversal is representable and none has to be checked for, and the mapping stays
// injective on case-insensitive filesystems, where two base64url ids differing only in case would
// otherwise collide and serve each other's bytes.
func fileKey(id string) string { return hex.EncodeToString([]byte(id)) }

// cachePath returns the on-disk location for an attachment. The bytes are stored flat under the
// encoded file_unique_id: the id keys content, not a name, so the display name is taken from the
// message row instead.
func (s *Store) cachePath(fileUniqueID string) string {
	return filepath.Join(s.dir, filesDir, fileKey(fileUniqueID))
}

// Cached returns the path of an already downloaded attachment; ok is false on a cache miss.
// Lstat, not Stat: a symlink planted at the key path resolves elsewhere and is a miss.
func (s *Store) Cached(fileUniqueID string) (path string, ok bool) {
	if fileUniqueID == "" {
		return "", false
	}
	path = s.cachePath(fileUniqueID)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return path, true
}

// SaveFile caches an attachment, handing write the destination to fill. The data lands in a
// temporary file that is renamed into place, so an interrupted download never leaves a truncated
// file behind for Cached to hand out.
func (s *Store) SaveFile(fileUniqueID string, write func(w io.Writer) error) (path string, err error) {
	if fileUniqueID == "" {
		return "", errors.New("empty file id")
	}
	path = s.cachePath(fileUniqueID)

	tmpDir := filepath.Join(s.dir, filesDir, tempDir)
	if err = os.MkdirAll(tmpDir, 0o750); err != nil { // creates the cache directory as its parent
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	tmp, err := os.CreateTemp(tmpDir, "part")
	if err != nil {
		return "", fmt.Errorf("create temp file for %q: %w", fileUniqueID, err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name()) // a no-op once the rename below succeeded
	}()

	if err := write(tmp); err != nil {
		return "", fmt.Errorf("write %q to cache: %w", fileUniqueID, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temp file for %q: %w", fileUniqueID, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("cache %q: %w", fileUniqueID, err)
	}
	return path, nil
}
