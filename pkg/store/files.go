package store

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	filesDir = "files"
	// tempPrefix marks half-written downloads; the leading dot keeps them out of Cached.
	tempPrefix = ".part"
)

// CachePath returns the on-disk location for an attachment, creating its directory. Each
// attachment gets a directory of its own keyed by file_unique_id, so the original file name is
// preserved without risking collisions.
func (s *Store) CachePath(fileUniqueID, name string) (string, error) {
	dir := filepath.Join(s.dir, filesDir, sanitize(fileUniqueID))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create cache dir for %q: %w", fileUniqueID, err)
	}
	return filepath.Join(dir, sanitize(name)), nil
}

// Cached returns the path of an already downloaded attachment; ok is false on a cache miss.
func (s *Store) Cached(fileUniqueID string) (path string, ok bool) {
	dir := filepath.Join(s.dir, filesDir, sanitize(fileUniqueID))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".") {
			return filepath.Join(dir, e.Name()), true
		}
	}
	return "", false
}

// SaveFile caches an attachment, handing write the destination to fill. The data lands in a
// temporary file that is renamed into place, so an interrupted download never leaves a truncated
// file behind for Cached to hand out.
func (s *Store) SaveFile(fileUniqueID, name string, write func(w io.Writer) error) (path string, err error) {
	path, err = s.CachePath(fileUniqueID, name)
	if err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), tempPrefix)
	if err != nil {
		return "", fmt.Errorf("create temp file for %q: %w", name, err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name()) // a no-op once the rename below succeeded
	}()

	if err := write(tmp); err != nil {
		return "", fmt.Errorf("write %q to cache: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temp file for %q: %w", name, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("cache %q: %w", name, err)
	}
	return path, nil
}

// sanitize reduces a telegram-supplied identifier or file name to a single safe path element.
// Whitespace is trimmed before the leading dots, so a padded ".." cannot survive as one, and the
// traversal names are rejected outright: the id reaches this from the /files/ url path.
func sanitize(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	name = strings.TrimSpace(strings.TrimLeft(name, "."))
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return name
}
