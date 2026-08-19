package store

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileKey(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{name: "base64url id", id: "AgADuQ", want: "416741447551"},
		{name: "separators are not expressible", id: "a/b", want: "612f62"},
		{name: "empty", id: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, fileKey(tt.id))
		})
	}

	// the invariant the flat layout rests on, checked on every filesystem: APFS folds case, so a
	// readable key would let two ids share one slot.
	assert.False(t, strings.EqualFold(fileKey("Ab"), fileKey("aB")))
}

func TestStore_CachePath(t *testing.T) {
	s := testStore(t)
	assert.Equal(t, filepath.Join(s.Dir(), filesDir, "416741447551"), s.cachePath("AgADuQ"),
		"one path element under files/, encoding covered by TestFileKey")

	_, err := os.Stat(filepath.Join(s.Dir(), filesDir))
	assert.True(t, os.IsNotExist(err), "cachePath is pure — SaveFile creates the directory")
}

func TestStore_Cached(t *testing.T) {
	t.Run("hit", func(t *testing.T) {
		s := testStore(t)
		path := s.cachePath("uid")
		writeFile(t, path, "payload")

		got, ok := s.Cached("uid")
		assert.True(t, ok)
		assert.Equal(t, path, got)
	})

	t.Run("miss on unknown id", func(t *testing.T) {
		s := testStore(t)
		_, ok := s.Cached("uid")
		assert.False(t, ok)
	})

	t.Run("miss on empty cache directory", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, os.MkdirAll(filepath.Join(s.Dir(), filesDir), 0o750))

		_, ok := s.Cached("uid")
		assert.False(t, ok, "a created but unpopulated cache directory is not a cache hit")
	})

	t.Run("miss on empty id", func(t *testing.T) {
		s := testStore(t)
		_, ok := s.Cached("")
		assert.False(t, ok)
	})

	t.Run("leftover directory from the old layout is a miss", func(t *testing.T) {
		s := testStore(t)
		path := s.cachePath("uid")
		require.NoError(t, os.MkdirAll(path, 0o750))
		writeFile(t, filepath.Join(path, "server.log"), "payload")

		_, ok := s.Cached("uid")
		assert.False(t, ok, "a directory where the bytes are expected must be a miss, not an error")

		_, err := s.SaveFile("uid", func(io.Writer) error { return nil })
		require.Error(t, err, "the re-download reports the blocked slot instead of looping forever")
		assert.Contains(t, err.Error(), `cache "uid"`)
	})

	t.Run("symlink at the key path is a miss", func(t *testing.T) {
		s := testStore(t)
		secret := filepath.Join(s.Dir(), "tg-mcp.db")
		writeFile(t, secret, "database")
		require.NoError(t, os.MkdirAll(filepath.Join(s.Dir(), filesDir), 0o750))
		require.NoError(t, os.Symlink(secret, s.cachePath("uid")))

		_, ok := s.Cached("uid")
		assert.False(t, ok, "Lstat, not Stat: a planted symlink must not serve what it points at")

		require.NoError(t, os.Symlink(filepath.Join(s.Dir(), "gone"), s.cachePath("dangling")))
		_, ok = s.Cached("dangling")
		assert.False(t, ok)
	})

	t.Run("no id addresses a file outside its own slot", func(t *testing.T) {
		s := testStore(t)
		writeFile(t, filepath.Join(s.Dir(), "tg-mcp.db"), "database")
		require.NoError(t, os.MkdirAll(filepath.Join(s.Dir(), filesDir), 0o750))
		writeFile(t, filepath.Join(s.Dir(), filesDir, "loose.bin"), "payload")

		// the first two address the planted regular files under any identity-like key, the rest
		// are the traversal shapes the old sanitize rule had to strip
		for _, id := range []string{"loose.bin", "../tg-mcp.db", "..", " ..", "\t..", ".", " . ", "../..", "/.."} {
			_, ok := s.Cached(id)
			assert.False(t, ok, "id %q must not resolve outside its own cache entry", id)
		}
	})

	t.Run("the id used for writing is the one that reads back", func(t *testing.T) {
		s := testStore(t)
		path := s.cachePath("a/b")
		writeFile(t, path, "payload")

		got, ok := s.Cached("a/b")
		assert.True(t, ok)
		assert.Equal(t, path, got)
	})
}

// TestStore_CachedCaseFold reproduces only on a case-insensitive filesystem such as APFS:
// file_unique_id is case-significant base64url, so ids differing only in case must not share
// cached bytes. It compares bytes rather than paths — filepath.Join is pure string work, so a
// path comparison agrees on every filesystem.
func TestStore_CachedCaseFold(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{"Ab", "aB"} {
		_, err := s.SaveFile(id, func(w io.Writer) error {
			_, _ = io.WriteString(w, "payload-"+id)
			return nil
		})
		require.NoError(t, err)
	}

	for _, id := range []string{"Ab", "aB"} {
		path, ok := s.Cached(id)
		require.True(t, ok, "id %q must be cached", id)

		data, err := os.ReadFile(path) //nolint:gosec // test path
		require.NoError(t, err)
		assert.Equal(t, "payload-"+id, string(data), "id %q served the wrong bytes", id)
	}
}

func TestStore_SaveFile(t *testing.T) {
	t.Run("written and readable through Cached", func(t *testing.T) {
		s := testStore(t)
		path, err := s.SaveFile("uid", func(w io.Writer) error {
			_, _ = io.WriteString(w, "payload")
			return nil
		})
		require.NoError(t, err)

		got, ok := s.Cached("uid")
		assert.True(t, ok)
		assert.Equal(t, path, got)

		data, err := os.ReadFile(path) //nolint:gosec // test path
		require.NoError(t, err)
		assert.Equal(t, "payload", string(data))

		entries, err := os.ReadDir(filepath.Join(s.Dir(), filesDir, tempDir))
		require.NoError(t, err)
		assert.Empty(t, entries, "a completed download leaves no temp file behind")
	})

	t.Run("failed write leaves no cache hit", func(t *testing.T) {
		s := testStore(t)
		_, err := s.SaveFile("uid", func(w io.Writer) error {
			_, _ = io.WriteString(w, "half of the")
			return errors.New("connection reset")
		})
		require.ErrorContains(t, err, "connection reset")

		_, ok := s.Cached("uid")
		assert.False(t, ok, "a truncated download must never be served as the cached file")

		entries, err := os.ReadDir(filepath.Join(s.Dir(), filesDir, tempDir))
		require.NoError(t, err)
		assert.Empty(t, entries, "the temp file is cleaned up")
	})

	t.Run("overwrites an earlier copy", func(t *testing.T) {
		s := testStore(t)
		for _, content := range []string{"first", "second"} {
			_, err := s.SaveFile("uid", func(w io.Writer) error {
				_, _ = io.WriteString(w, content)
				return nil
			})
			require.NoError(t, err)
		}

		path, ok := s.Cached("uid")
		require.True(t, ok)
		data, err := os.ReadFile(path) //nolint:gosec // test path
		require.NoError(t, err)
		assert.Equal(t, "second", string(data))
	})

	t.Run("empty file id", func(t *testing.T) {
		s := testStore(t)
		_, err := s.SaveFile("", func(io.Writer) error { return nil })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty file id")
	})

	t.Run("unusable cache root", func(t *testing.T) {
		s := testStore(t)
		writeFile(t, filepath.Join(s.Dir(), filesDir), "not a dir")

		_, err := s.SaveFile("uid", func(io.Writer) error { return nil })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create temp dir")
	})

	t.Run("unusable temp directory", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, os.MkdirAll(filepath.Join(s.Dir(), filesDir), 0o750))
		writeFile(t, filepath.Join(s.Dir(), filesDir, tempDir), "not a dir")

		_, err := s.SaveFile("uid", func(io.Writer) error { return nil })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create temp dir")
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}
