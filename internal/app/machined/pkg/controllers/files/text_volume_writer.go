// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package files

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	configconfig "github.com/siderolabs/talos/pkg/machinery/config/config"
)

// Reserved names in the root of a text volume.
//
// Text volume configs reject path components starting with "..", so these never collide with a file.
const (
	// textVolumeDataLink points to the directory holding the current set of files.
	textVolumeDataLink = "..data"
	// textVolumeDataLinkTmp is the new data link, before it is renamed over textVolumeDataLink.
	textVolumeDataLinkTmp = "..data_tmp"
	// textVolumeDataDirPrefix is the prefix of a directory holding one set of files.
	textVolumeDataDirPrefix = "..data-"
	// textVolumeEntryTmpPrefix is the prefix of a new top-level symlink, before it is renamed into place.
	textVolumeEntryTmpPrefix = "..tmp-"
)

// TextVolumeContentsHash returns the hash identifying a set of text volume files.
func TextVolumeContentsHash(files []configconfig.TextVolumeFile) string {
	hasher := sha256.New()

	for _, file := range files {
		// length-prefixed, so that no two different sets of files hash the same
		for _, field := range []string{file.Path, file.Contents} {
			hasher.Write(binary.BigEndian.AppendUint64(nil, uint64(len(field))))
			hasher.Write([]byte(field))
		}

		hasher.Write(binary.BigEndian.AppendUint32(nil, uint32(file.Mode.Perm())))
	}

	return hex.EncodeToString(hasher.Sum(nil))[:16]
}

// WriteTextVolume renders the files of a text volume into the root.
//
// The files are laid out like in a Kubernetes ConfigMap volume:
//   - each set of files is written to its own `..data-<hash>` directory;
//   - the `..data` symlink points to the current one, and it is swapped with a rename, so the whole set
//     of files changes atomically;
//   - each top-level entry (file or directory) is a symlink into `..data`.
//
// Rendering the set of files which is already current only repairs the top-level symlinks.
// Returns the hash of the rendered files.
//
//nolint:gocyclo
func WriteTextVolume(root *os.Root, files []configconfig.TextVolumeFile) (string, error) {
	hash := TextVolumeContentsHash(files)
	dataDir := textVolumeDataDirPrefix + hash

	current, err := root.Readlink(textVolumeDataLink)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("failed to read data link: %w", err)
	}

	if current != dataDir {
		if err = writeTextVolumeDataDir(root, dataDir, files); err != nil {
			return "", err
		}

		if err = replaceSymlink(root, dataDir, textVolumeDataLink, textVolumeDataLinkTmp); err != nil {
			return "", fmt.Errorf("failed to swap data link: %w", err)
		}
	}

	wantedEntries := map[string]struct{}{}

	for _, file := range files {
		entry, _, _ := strings.Cut(file.Path, "/")
		wantedEntries[entry] = struct{}{}
	}

	for entry := range wantedEntries {
		target := path.Join(textVolumeDataLink, entry)

		if link, linkErr := root.Readlink(entry); linkErr == nil && link == target {
			continue
		}

		if err = replaceSymlink(root, target, entry, textVolumeEntryTmpPrefix+entry); err != nil {
			return "", fmt.Errorf("failed to link %q: %w", entry, err)
		}
	}

	// clean up whatever is left of the previous sets of files, and the entries no longer in the volume
	dir, err := root.Open(".")
	if err != nil {
		return "", fmt.Errorf("failed to open volume root: %w", err)
	}

	names, err := dir.Readdirnames(-1)
	dir.Close() //nolint:errcheck

	if err != nil {
		return "", fmt.Errorf("failed to list volume root: %w", err)
	}

	for _, name := range names {
		if _, wanted := wantedEntries[name]; wanted || name == textVolumeDataLink || name == dataDir {
			continue
		}

		if err = root.RemoveAll(name); err != nil {
			return "", fmt.Errorf("failed to remove stale %q: %w", name, err)
		}
	}

	return hash, nil
}

// writeTextVolumeDataDir writes one set of files into a fresh directory.
func writeTextVolumeDataDir(root *os.Root, dataDir string, files []configconfig.TextVolumeFile) error {
	// a leftover of an interrupted write
	if err := root.RemoveAll(dataDir); err != nil {
		return fmt.Errorf("failed to clean up %q: %w", dataDir, err)
	}

	if err := root.Mkdir(dataDir, 0o755); err != nil {
		return fmt.Errorf("failed to create %q: %w", dataDir, err)
	}

	for _, file := range files {
		filePath := path.Join(dataDir, file.Path)

		if err := root.MkdirAll(path.Dir(filePath), 0o755); err != nil {
			return fmt.Errorf("failed to create directory for %q: %w", file.Path, err)
		}

		if err := root.WriteFile(filePath, []byte(file.Contents), file.Mode.Perm()); err != nil {
			return fmt.Errorf("failed to write %q: %w", file.Path, err)
		}

		// the mode passed on creation is subject to the umask
		if err := root.Chmod(filePath, file.Mode.Perm()); err != nil {
			return fmt.Errorf("failed to set mode of %q: %w", file.Path, err)
		}
	}

	return nil
}

// replaceSymlink atomically points the symlink name to target, via a temporary symlink renamed into place.
func replaceSymlink(root *os.Root, target, name, tmpName string) error {
	if err := root.Remove(tmpName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	if err := root.Symlink(target, tmpName); err != nil {
		return err
	}

	if err := root.Rename(tmpName, name); err != nil {
		// rename can't replace a directory, which is never created by the writer, but might be left behind
		if removeErr := root.RemoveAll(name); removeErr != nil {
			return errors.Join(err, removeErr)
		}

		return root.Rename(tmpName, name)
	}

	return nil
}
