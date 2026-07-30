package store

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_CachePath(t *testing.T) {
	tests := []struct {
		name     string
		uniqueID string
		file     string
		want     string
	}{
		{name: "plain", uniqueID: "AgADuQ", file: "server.log", want: "AgADuQ/server.log"},
		{name: "path separators stripped", uniqueID: "a/b", file: "../../etc/passwd", want: "a_b/_.._etc_passwd"},
		{name: "backslashes stripped", uniqueID: "x", file: `c:\tmp\a.txt`, want: `x/c:_tmp_a.txt`},
		{name: "empty name", uniqueID: "x", file: "", want: "x/file"},
		{name: "dot name", uniqueID: "x", file: ".", want: "x/file"},
		{name: "empty unique id", uniqueID: "", file: "a.txt", want: "file/a.txt"},
		{name: "padded traversal", uniqueID: " ..", file: " ..", want: "file/file"},
		{name: "tab padded traversal", uniqueID: "\t..", file: "a.txt", want: "file/a.txt"},
		{name: "dotted traversal", uniqueID: ". ..", file: "a.txt", want: "file/a.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testStore(t)
			got, err := s.CachePath(tt.uniqueID, tt.file)
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(s.Dir(), filesDir, filepath.FromSlash(tt.want)), got)

			info, err := os.Stat(filepath.Dir(got))
			require.NoError(t, err, "cache directory must be created")
			assert.True(t, info.IsDir())
		})
	}

	t.Run("undreadable cache root", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, os.MkdirAll(filepath.Join(s.Dir(), filesDir), 0o750))
		writeFile(t, filepath.Join(s.Dir(), filesDir, "uid"), "not a dir")

		_, err := s.CachePath("uid", "a.txt")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create cache dir")
	})
}

func TestStore_Cached(t *testing.T) {
	t.Run("hit", func(t *testing.T) {
		s := testStore(t)
		path, err := s.CachePath("uid", "server.log")
		require.NoError(t, err)
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

	t.Run("miss on empty directory", func(t *testing.T) {
		s := testStore(t)
		_, err := s.CachePath("uid", "server.log")
		require.NoError(t, err)

		_, ok := s.Cached("uid")
		assert.False(t, ok, "a created but unpopulated directory is not a cache hit")
	})

	t.Run("skips subdirectories", func(t *testing.T) {
		s := testStore(t)
		path, err := s.CachePath("uid", "server.log")
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Join(filepath.Dir(path), "aaa-subdir"), 0o750))
		writeFile(t, path, "payload")

		got, ok := s.Cached("uid")
		assert.True(t, ok)
		assert.Equal(t, path, got)
	})

	t.Run("traversal id cannot escape the cache", func(t *testing.T) {
		s := testStore(t)
		writeFile(t, filepath.Join(s.Dir(), "tg-mcp.db"), "database")
		require.NoError(t, os.MkdirAll(filepath.Join(s.Dir(), filesDir), 0o750))
		writeFile(t, filepath.Join(s.Dir(), filesDir, "loose.bin"), "payload")

		for _, id := range []string{"..", " ..", "\t..", ".", " . ", "../..", "/.."} {
			_, ok := s.Cached(id)
			assert.False(t, ok, "id %q must not resolve outside its own cache directory", id)
		}
	})

	t.Run("sanitized id matches the one used for writing", func(t *testing.T) {
		s := testStore(t)
		path, err := s.CachePath("a/b", "x.bin")
		require.NoError(t, err)
		writeFile(t, path, "payload")

		got, ok := s.Cached("a/b")
		assert.True(t, ok)
		assert.Equal(t, path, got)
	})
}

func TestStore_SaveFile(t *testing.T) {
	t.Run("written and readable through Cached", func(t *testing.T) {
		s := testStore(t)
		path, err := s.SaveFile("uid", "server.log", func(w io.Writer) error {
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
	})

	t.Run("failed write leaves no cache hit", func(t *testing.T) {
		s := testStore(t)
		_, err := s.SaveFile("uid", "server.log", func(w io.Writer) error {
			_, _ = io.WriteString(w, "half of the")
			return errors.New("connection reset")
		})
		require.ErrorContains(t, err, "connection reset")

		_, ok := s.Cached("uid")
		assert.False(t, ok, "a truncated download must never be served as the cached file")

		entries, err := os.ReadDir(filepath.Join(s.Dir(), filesDir, "uid"))
		require.NoError(t, err)
		assert.Empty(t, entries, "the temp file is cleaned up")
	})

	t.Run("overwrites an earlier copy", func(t *testing.T) {
		s := testStore(t)
		for _, content := range []string{"first", "second"} {
			_, err := s.SaveFile("uid", "server.log", func(w io.Writer) error {
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

	t.Run("unusable cache directory", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, os.MkdirAll(filepath.Join(s.Dir(), filesDir), 0o750))
		writeFile(t, filepath.Join(s.Dir(), filesDir, "uid"), "not a dir")

		_, err := s.SaveFile("uid", "a.txt", func(io.Writer) error { return nil })
		require.Error(t, err)
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}
