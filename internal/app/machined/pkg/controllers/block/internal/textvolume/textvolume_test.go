// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package textvolume_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/block/internal/textvolume"
)

// tree returns the regular files under root with their content, and the directories with an empty content.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()

	result := map[string]string{}

	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)

		if path == root {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		require.NoError(t, err)

		if d.IsDir() {
			result[rel+"/"] = ""

			return nil
		}

		content, err := os.ReadFile(path)
		require.NoError(t, err)

		info, err := d.Info()
		require.NoError(t, err)
		assert.Equal(t, fs.FileMode(textvolume.FileMode), info.Mode().Perm(), rel)

		result[rel] = string(content)

		return nil
	}))

	return result
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()

	st, err := os.Stat(path)
	require.NoError(t, err)

	return st.Sys().(*syscall.Stat_t).Ino
}

func TestWrite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	require.NoError(t, textvolume.Write(root, map[string]string{
		"a.conf":       "a",
		"dir/b.conf":   "b",
		"dir/sub/c":    "c",
		"other/d.conf": "d",
	}))

	assert.Equal(t, map[string]string{
		"a.conf":       "a",
		"dir/":         "",
		"dir/b.conf":   "b",
		"dir/sub/":     "",
		"dir/sub/c":    "c",
		"other/":       "",
		"other/d.conf": "d",
	}, tree(t, root))

	unchangedInode := inode(t, filepath.Join(root, "dir/b.conf"))
	changedInode := inode(t, filepath.Join(root, "a.conf"))

	// leftovers of an interrupted update
	require.NoError(t, os.WriteFile(filepath.Join(root, "dir", ".talos-tmp-123"), nil, 0o600))

	require.NoError(t, textvolume.Write(root, map[string]string{
		"a.conf":     "a2",          // changed
		"dir/b.conf": "b",           // unchanged
		"dir/sub":    "now a file",  // directory turned into a file
		"other":      "also a file", // directory with content turned into a file
		"x/y":        "new",         // new
	}))

	assert.Equal(t, map[string]string{
		"a.conf":     "a2",
		"dir/":       "",
		"dir/b.conf": "b",
		"dir/sub":    "now a file",
		"other":      "also a file",
		"x/":         "",
		"x/y":        "new",
	}, tree(t, root))

	// an unchanged file is left alone, a changed one is replaced by a new inode
	assert.Equal(t, unchangedInode, inode(t, filepath.Join(root, "dir/b.conf")))
	assert.NotEqual(t, changedInode, inode(t, filepath.Join(root, "a.conf")))

	// a file turned back into a directory
	require.NoError(t, textvolume.Write(root, map[string]string{
		"other/z": "z",
	}))

	assert.Equal(t, map[string]string{
		"other/":  "",
		"other/z": "z",
	}, tree(t, root))
}

func TestWriteRestoresMode(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	files := map[string]string{"a": "a"}

	require.NoError(t, textvolume.Write(root, files))
	require.NoError(t, os.Chmod(filepath.Join(root, "a"), 0o600))
	require.NoError(t, textvolume.Write(root, files))

	assert.Equal(t, map[string]string{"a": "a"}, tree(t, root))
}

func TestInstanceVolumeID(t *testing.T) {
	t.Parallel()

	id1 := textvolume.InstanceVolumeID("app-config", "containers.MountController/web/app-config")
	id2 := textvolume.InstanceVolumeID("app-config", "containers.MountController/api/app-config")

	assert.True(t, strings.HasPrefix(id1, "t-app-config-"), id1)
	assert.NotEqual(t, id1, id2)
	assert.Equal(t, id1, textvolume.InstanceVolumeID("app-config", "containers.MountController/web/app-config"))
}

func TestHash(t *testing.T) {
	t.Parallel()

	hashes := []string{
		textvolume.Hash(map[string]string{"a": "b"}),
		textvolume.Hash(map[string]string{"a": "c"}),
		textvolume.Hash(map[string]string{"ab": ""}),
		textvolume.Hash(map[string]string{"a": "", "b": ""}),
	}

	slices.Sort(hashes)
	assert.Len(t, slices.Compact(hashes), 4)

	assert.Equal(t, textvolume.Hash(map[string]string{"a": "1", "b": "2"}), textvolume.Hash(map[string]string{"b": "2", "a": "1"}))
}

func TestSize(t *testing.T) {
	t.Parallel()

	page := uint64(os.Getpagesize())

	assert.Equal(t, uint64(textvolume.MinSize), textvolume.Size(map[string]string{"a": "b"}))

	files := map[string]string{
		"big":   strings.Repeat("x", textvolume.MinSize),
		"small": "x",
	}

	assert.Equal(t, 2*(textvolume.MinSize+page), textvolume.Size(files))
}
