// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package block

//docgen:jsonschema

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/internal/registry"
	"github.com/siderolabs/talos/pkg/machinery/config/types/meta"
	"github.com/siderolabs/talos/pkg/machinery/config/validation"
)

// TextVolumeConfigKind is a config document kind.
const TextVolumeConfigKind = "TextVolumeConfig"

// TextVolumeWarnSize is the total content size above which a warning is emitted.
//
// The content lives in the machine configuration, and once more in memory for every consumer of the
// volume, so a large document costs memory several times over. It is not an error: there is no
// correctness boundary here, only a point past which a different mechanism is probably the right one.
const TextVolumeWarnSize = 1 << 20

func init() {
	registry.Register(TextVolumeConfigKind, func(version string) config.Document {
		switch version {
		case "v1alpha1": //nolint:goconst
			return &TextVolumeConfigV1Alpha1{}
		default:
			return nil
		}
	})
}

// Check interfaces.
var (
	_ config.TextVolumeConfig    = &TextVolumeConfigV1Alpha1{}
	_ config.ConflictingDocument = &TextVolumeConfigV1Alpha1{}
	_ config.NamedDocument       = &TextVolumeConfigV1Alpha1{}
	_ config.Validator           = &TextVolumeConfigV1Alpha1{}
)

// TextVolumeConfigV1Alpha1 is a text volume configuration document.
//
//	description: |
//	  Text volumes hold a set of text files given inline in the machine configuration.
//
//	  The document does nothing on its own. Each consumer mounting it (e.g. a `ContainerConfig`
//	  `textVolume` mount) gets its own memory-backed copy of the files: no two consumers share an
//	  inode, even though the content is the same. The copy is always read-only, and its files
//	  cannot be executed directly. It is updated in place when the document changes, without
//	  restarting the consumer.
//
//	  Unlike user, existing and external volumes, a text volume has no shared mount on the host
//	  (there is nothing at `/var/mnt/<name>`), so it has no mount settings.
//
//	  The content is never written to persistent storage, but it is stored in the machine
//	  configuration verbatim, so treat it as being as sensitive as the machine configuration itself.
//
//	  The name must not be taken by a user, existing or external volume.
//	examples:
//	  - value: exampleTextVolumeConfigV1Alpha1()
//	alias: TextVolumeConfig
//	schemaRoot: true
//	schemaMeta: v1alpha1/TextVolumeConfig
type TextVolumeConfigV1Alpha1 struct {
	meta.Meta `yaml:",inline"`

	//   description: |
	//     Name of the volume.
	//
	//     Name can only contain:
	//     lowercase and uppercase ASCII letters, digits, and hyphens.
	MetaName string `yaml:"name"`
	//   description: |
	//     Files in the volume, keyed by a path relative to the volume root.
	//
	//     A path containing a slash creates subdirectories. Paths must be relative and may not
	//     escape the volume with `..`, and names starting with `.talos-` are reserved. Content must
	//     be valid UTF-8; files are created with mode `0644` and directories with `0755`.
	//   examples:
	//     - value: exampleTextVolumeFiles()
	FilesSpec map[string]string `yaml:"files"`
}

// NewTextVolumeConfigV1Alpha1 creates a new text volume config document.
func NewTextVolumeConfigV1Alpha1() *TextVolumeConfigV1Alpha1 {
	return &TextVolumeConfigV1Alpha1{
		Meta: meta.Meta{
			MetaKind:       TextVolumeConfigKind,
			MetaAPIVersion: "v1alpha1",
		},
	}
}

func exampleTextVolumeConfigV1Alpha1() *TextVolumeConfigV1Alpha1 {
	cfg := NewTextVolumeConfigV1Alpha1()
	cfg.MetaName = "app-config"
	cfg.FilesSpec = exampleTextVolumeFiles()

	return cfg
}

func exampleTextVolumeFiles() map[string]string {
	return map[string]string{
		"app.conf":       "[server]\nlisten=:8080\n",
		"conf.d/log.ini": "level=info\n",
	}
}

// Name implements config.NamedDocument interface.
func (s *TextVolumeConfigV1Alpha1) Name() string {
	return s.MetaName
}

// Clone implements config.Document interface.
func (s *TextVolumeConfigV1Alpha1) Clone() config.Document {
	return s.DeepCopy()
}

// ConflictsWithKinds implements config.ConflictingDocument interface.
func (s *TextVolumeConfigV1Alpha1) ConflictsWithKinds() []string {
	return conflictingVolumeKinds(TextVolumeConfigKind)
}

// TextVolumeConfigSignal is a signal for text volume config.
func (s *TextVolumeConfigV1Alpha1) TextVolumeConfigSignal() {}

// Files implements config.TextVolumeConfig interface.
func (s *TextVolumeConfigV1Alpha1) Files() map[string]string {
	return s.FilesSpec
}

// Validate implements config.Validator interface.
func (s *TextVolumeConfigV1Alpha1) Validate(validation.RuntimeMode, ...validation.Option) ([]string, error) {
	var (
		warnings         []string
		validationErrors error
	)

	validationErrors = errors.Join(
		validationErrors,
		validateTextVolumeName(s.MetaName),
		validateTextVolumePaths(s.FilesSpec),
		validateTextVolumeContent(s.FilesSpec),
	)

	if total := TextVolumeContentSize(s.FilesSpec); total > TextVolumeWarnSize {
		warnings = append(warnings,
			fmt.Sprintf("text volume %q: total content is %d bytes, which is kept in the machine configuration and in memory for every consumer", s.MetaName, total))
	}

	return warnings, validationErrors
}

// validateTextVolumeName checks the name of a text volume.
func validateTextVolumeName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}

	if strings.ContainsFunc(name, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z':
			return false
		case r >= 'A' && r <= 'Z':
			return false
		case r >= '0' && r <= '9':
			return false
		case r == '-':
			return false
		default: // invalid symbol
			return true
		}
	}) {
		return errors.New("name can only contain lowercase and uppercase ASCII letters, digits, and hyphens")
	}

	return nil
}

// validateTextVolumeContent checks that every file has text content.
func validateTextVolumeContent(files map[string]string) error {
	if len(files) == 0 {
		return errors.New("files is required and must not be empty")
	}

	var validationErrors error

	// Sorted so that the reported errors are stable across runs; a map iterates in random order.
	for _, path := range slices.Sorted(maps.Keys(files)) {
		content := files[path]

		if !utf8.ValidString(content) {
			validationErrors = errors.Join(validationErrors, fmt.Errorf("files[%q]: content is not valid UTF-8", path))
		}

		if strings.ContainsRune(content, 0) {
			validationErrors = errors.Join(validationErrors, fmt.Errorf("files[%q]: content contains a NUL byte", path))
		}
	}

	return validationErrors
}

// TextVolumeContentSize returns the total content size of a file set: the byte length of every path
// plus its content, summed.
func TextVolumeContentSize(files map[string]string) int {
	total := 0

	for path, content := range files {
		total += len(path) + len(content)
	}

	return total
}

// validateTextVolumePaths checks that every key is a usable relative path, and that no two keys
// require the same name to be both a file and a directory.
func validateTextVolumePaths(files map[string]string) error {
	var validationErrors error

	// Directories implied by the keys, so that an "a/b" file can be rejected against an "a/b/c" file.
	dirs := map[string]struct{}{}

	paths := slices.Sorted(maps.Keys(files))

	for _, path := range paths {
		switch {
		case path == "":
			validationErrors = errors.Join(validationErrors, errors.New("files: path must not be empty"))

			continue
		case !filepath.IsLocal(path):
			// IsLocal is the single check covering absolute paths, "..", "." and empty components:
			// anything that could name a file outside the volume.
			validationErrors = errors.Join(validationErrors, fmt.Errorf("files[%q]: path must be relative and must not escape the volume", path))

			continue
		case path != filepath.Clean(path):
			// IsLocal accepts "a//b" and "a/./b"; those name the same file as their cleaned form,
			// which would make two keys collide silently.
			validationErrors = errors.Join(validationErrors, fmt.Errorf("files[%q]: path must be in canonical form, i.e. %q", path, filepath.Clean(path)))

			continue
		case slices.ContainsFunc(strings.Split(path, "/"), func(component string) bool {
			return strings.HasPrefix(component, TextVolumeReservedPrefix)
		}):
			validationErrors = errors.Join(validationErrors, fmt.Errorf("files[%q]: names starting with %q are reserved", path, TextVolumeReservedPrefix))

			continue
		}

		for dir := filepath.Dir(path); dir != "."; dir = filepath.Dir(dir) {
			dirs[dir] = struct{}{}
		}
	}

	for _, path := range paths {
		if _, isDir := dirs[path]; isDir {
			validationErrors = errors.Join(validationErrors, fmt.Errorf("files[%q]: path is used as both a file and a directory", path))
		}
	}

	return validationErrors
}

// TextVolumeReservedPrefix is the file name prefix reserved for the files the text volume writer
// creates next to the files it replaces.
const TextVolumeReservedPrefix = ".talos-"
