// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package textvolume materializes the files of text volumes.
package textvolume

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

	blockcfg "github.com/siderolabs/talos/pkg/machinery/config/types/block"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

const (
	// DirMode is the mode of the directories in a text volume.
	DirMode = 0o755

	// FileMode is the mode of the files in a text volume.
	FileMode = 0o644

	// MinSize is the smallest size limit of a text volume tmpfs, in bytes.
	//
	// A tmpfs only takes the memory its content uses, so the limit costs nothing; a floor keeps small
	// edits from needing a resize at all.
	MinSize = 1 << 20
)

// InstanceVolumeID returns the ID of the volume holding the instance of a text volume serving a request.
//
// Derived from the request ID, so that every request gets an instance of its own, and the same request
// always maps to the same instance, also across controller restarts.
func InstanceVolumeID(textVolumeName, requestID string) string {
	digest := sha256.Sum256([]byte(requestID))

	return constants.TextVolumePrefix + textVolumeName + "-" + hex.EncodeToString(digest[:6])
}

// Hash returns a stable digest of a file set.
//
// Sorted by path, and both halves NUL-terminated, so that map iteration order cannot change the result
// and no two distinct sets can produce the same stream. NUL is safe as a separator because validation
// rejects it in content, and a path cannot contain one.
func Hash(files map[string]string) string {
	hash := sha256.New()

	for _, name := range slices.Sorted(maps.Keys(files)) {
		hash.Write([]byte(name))
		hash.Write([]byte{0})
		hash.Write([]byte(files[name]))
		hash.Write([]byte{0})
	}

	return hex.EncodeToString(hash.Sum(nil))
}

// Size returns the tmpfs size limit needed to hold files, in bytes.
//
// A file is replaced by writing a new copy next to it, so while the content is updated the old and the
// new copies coexist: twice the space the files take, rounded up to whole pages as tmpfs accounts
// them, covers any update from content that also fit the limit.
func Size(files map[string]string) uint64 {
	pageSize := uint64(os.Getpagesize())

	var size uint64

	for _, content := range files {
		size += (uint64(len(content)) + pageSize - 1) / pageSize * pageSize
	}

	return max(2*size, MinSize)
}

// Write makes the tree under root hold exactly files.
//
// Every file is replaced atomically (write a temporary file, then rename it over the old one), so a
// consumer reading the volume while it is updated sees either the old or the new content of a file,
// never a mix. Files and directories not in the set are removed first, so that a name changing from a
// file to a directory (or back) does not get in the way of the new content.
//
// Unchanged files are not rewritten, so an unchanged set leaves the tree as it is.
func Write(root string, files map[string]string) error {
	wantFiles := map[string]struct{}{}
	wantDirs := map[string]struct{}{}

	for name := range files {
		target := filepath.Join(root, name)

		wantFiles[target] = struct{}{}

		for dir := filepath.Dir(target); dir != root; dir = filepath.Dir(dir) {
			wantDirs[dir] = struct{}{}
		}
	}

	if err := prune(root, wantFiles, wantDirs); err != nil {
		return err
	}

	// Sorted so that a failure reports the same file every time.
	for _, name := range slices.Sorted(maps.Keys(files)) {
		target := filepath.Join(root, name)

		if err := os.MkdirAll(filepath.Dir(target), DirMode); err != nil {
			return fmt.Errorf("failed to create directory for %q: %w", name, err)
		}

		if err := updateFile(target, []byte(files[name])); err != nil {
			return fmt.Errorf("failed to write %q: %w", name, err)
		}
	}

	return nil
}

// prune removes everything under root which is not wanted, or is wanted as the other kind (a file
// where a directory is wanted, or the other way around).
//
// Temporary files left behind by an interrupted update are never wanted, so they are removed as well.
func prune(root string, wantFiles, wantDirs map[string]struct{}) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if path == root {
			return nil
		}

		_, isWantedFile := wantFiles[path]
		_, isWantedDir := wantDirs[path]

		switch {
		case d.IsDir() && isWantedDir:
			return nil
		case d.Type().IsRegular() && isWantedFile:
			return nil
		}

		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("failed to remove %q: %w", path, err)
		}

		if d.IsDir() {
			// The directory is gone; walking into it would fail.
			return filepath.SkipDir
		}

		return nil
	})
}

// updateFile replaces the content of filename atomically, unless it is unchanged already.
//
// The temporary file is created in the same directory as the target, so the rename stays on one
// filesystem, and is therefore atomic.
func updateFile(filename string, contents []byte) error {
	oldContents, err := os.ReadFile(filename)
	if err == nil && bytes.Equal(oldContents, contents) {
		return ensureMode(filename)
	}

	dir := filepath.Dir(filename)

	tmp, err := os.CreateTemp(dir, blockcfg.TextVolumeReservedPrefix+"tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary file in %q: %w", dir, err)
	}

	tmpName := tmp.Name()

	defer os.Remove(tmpName) //nolint:errcheck

	_, err = tmp.Write(contents)

	err = errors.Join(err, tmp.Chmod(FileMode), tmp.Close())
	if err != nil {
		return fmt.Errorf("failed to write %q: %w", tmpName, err)
	}

	if err = os.Rename(tmpName, filename); err != nil {
		return fmt.Errorf("failed to rename %q to %q: %w", tmpName, filename, err)
	}

	return nil
}

// ensureMode restores the mode of an unchanged file.
func ensureMode(filename string) error {
	st, err := os.Stat(filename)
	if err != nil {
		return err
	}

	if st.Mode().Perm() == FileMode {
		return nil
	}

	return os.Chmod(filename, FileMode)
}
