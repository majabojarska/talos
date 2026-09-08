// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package container

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

// TextFilesConfigKind is a config document kind.
const TextFilesConfigKind = "TextFilesConfig"

// TextFilesWarnSize is the total payload size above which a warning is emitted.
//
// The contents live in the machine configuration and, once materialized, on a tmpfs, so a large
// document costs memory twice over. Not an error: there is no correctness boundary here, only a
// point past which a different mechanism is probably the right one.
//
// Exported so a container-level check can reuse the same threshold once it accounts for how many
// containers actually mount the document (see validateContainerTextFilesSize).
const TextFilesWarnSize = 1 << 20

// TextFilesTotalSize returns the total payload size of a file set: the byte length of every path
// plus its content, summed.
//
// Exported for the same reason as TextFilesWarnSize: a container-level check needs to compute this
// same size, multiplied by however many containers mount the document, since Validate only ever
// sees one document at a time.
func TextFilesTotalSize(files map[string]string) int {
	total := 0

	for path, content := range files {
		total += len(path) + len(content)
	}

	return total
}

func init() {
	registry.Register(TextFilesConfigKind, func(version string) config.Document {
		switch version {
		case "v1alpha1":
			return &TextFilesConfigV1Alpha1{}
		default:
			return nil
		}
	})
}

// Check interfaces.
var (
	_ config.TextFilesConfig = &TextFilesConfigV1Alpha1{}
	_ config.NamedDocument   = &TextFilesConfigV1Alpha1{}
	_ config.Validator       = &TextFilesConfigV1Alpha1{}
)

// TextFilesConfigV1Alpha1 is a set of text files to be presented to a container.
//
//	description: |
//	  TextFilesConfig declares a named set of text files, given inline as content keyed by a
//	  relative path.
//
//	  The document holds content only: it does nothing on its own until a `ContainerConfig`
//	  mounts it via a `textFiles` mount, at which point the paths are materialized as a real
//	  directory tree and bind-mounted read-only at the mount's destination. Nested paths become
//	  real subdirectories.
//
//	  The tree is rebuilt from the machine configuration on every boot and is never written to
//	  persistent storage, but the contents are stored in the machine configuration verbatim, so
//	  treat anything put here as being as sensitive as the machine configuration itself.
//	examples:
//	  - value: exampleTextFilesConfigV1Alpha1()
//	alias: TextFilesConfig
//	schemaRoot: true
//	schemaMeta: v1alpha1/TextFilesConfig
type TextFilesConfigV1Alpha1 struct {
	meta.Meta `yaml:",inline"`

	//   description: |
	//     Name of the file set.
	//
	//     Must be between 1 and 63 characters long, and can only contain lowercase ASCII
	//     letters, digits and hyphens. It is the name a `ContainerConfig` refers to, and is used
	//     as a directory name on the host.
	MetaName string `yaml:"name"`
	//   description: |
	//     Files in the set, keyed by a path relative to the mount destination.
	//
	//     A key containing a slash creates subdirectories. Keys must be relative and may not
	//     escape the set with `..`. Content must be valid UTF-8; files are created with mode
	//     `0644` and directories with `0755`.
	//   examples:
	//     - value: exampleTextFiles()
	Files map[string]string `yaml:"files"`
}

// NewTextFilesConfigV1Alpha1 creates a new text files config document.
func NewTextFilesConfigV1Alpha1() *TextFilesConfigV1Alpha1 {
	return &TextFilesConfigV1Alpha1{
		Meta: meta.Meta{
			MetaKind:       TextFilesConfigKind,
			MetaAPIVersion: "v1alpha1",
		},
	}
}

func exampleTextFilesConfigV1Alpha1() *TextFilesConfigV1Alpha1 {
	cfg := NewTextFilesConfigV1Alpha1()
	cfg.MetaName = "my-foobar-configs"
	cfg.Files = exampleTextFiles()

	return cfg
}

func exampleTextFiles() map[string]string {
	return map[string]string{
		"foo.conf":       "[section-abc]\nkey1=val1\nkey2=val2\n",
		"subdir/bar.ini": "key1=val1\n",
	}
}

// Name implements config.NamedDocument interface.
func (c *TextFilesConfigV1Alpha1) Name() string {
	return c.MetaName
}

// Clone implements config.Document interface.
func (c *TextFilesConfigV1Alpha1) Clone() config.Document {
	return c.DeepCopy()
}

// TextFilesConfigSignal is a signal for text files config.
func (c *TextFilesConfigV1Alpha1) TextFilesConfigSignal() {}

// FileContents implements config.TextFilesConfig interface.
func (c *TextFilesConfigV1Alpha1) FileContents() map[string]string {
	return c.Files
}

// Validate implements config.Validator interface.
func (c *TextFilesConfigV1Alpha1) Validate(validation.RuntimeMode, ...validation.Option) ([]string, error) {
	var (
		warnings         []string
		validationErrors error
	)

	validationErrors = errors.Join(validationErrors, ValidateDocumentName(c.MetaName))

	if len(c.Files) == 0 {
		validationErrors = errors.Join(validationErrors, errors.New("files is required and must not be empty"))
	}

	validationErrors = errors.Join(validationErrors, validateTextFilePaths(c.Files))

	// Sorted so that the reported errors are stable across runs; a map iterates in random order.
	for _, path := range slices.Sorted(maps.Keys(c.Files)) {
		content := c.Files[path]

		if !utf8.ValidString(content) {
			validationErrors = errors.Join(validationErrors, fmt.Errorf("files[%q]: content is not valid UTF-8", path))
		}

		if strings.ContainsRune(content, 0) {
			validationErrors = errors.Join(validationErrors, fmt.Errorf("files[%q]: content contains a NUL byte", path))
		}
	}

	if total := TextFilesTotalSize(c.Files); total > TextFilesWarnSize {
		warnings = append(warnings,
			fmt.Sprintf("text files %q: total content is %d bytes, which is stored in the machine configuration and in memory", c.MetaName, total))
	}

	return warnings, validationErrors
}

// validateTextFilePaths checks that every key is a usable relative path, and that no two keys
// require the same name to be both a file and a directory.
func validateTextFilePaths(files map[string]string) error {
	var validationErrors error

	// Directories implied by the keys, so that a "a/b" file can be rejected against an "a/b/c" file.
	dirs := map[string]struct{}{}

	paths := slices.Sorted(maps.Keys(files))

	for _, path := range paths {
		switch {
		case path == "":
			validationErrors = errors.Join(validationErrors, errors.New("files: path must not be empty"))

			continue
		case !filepath.IsLocal(path):
			// IsLocal is the single check covering absolute paths, "..", "." and empty components:
			// anything that could name a file outside the set.
			validationErrors = errors.Join(validationErrors, fmt.Errorf("files[%q]: path must be relative and must not escape the file set", path))

			continue
		case path != filepath.Clean(path):
			// IsLocal accepts "a//b" and "a/./b"; those name the same file as their cleaned form,
			// which would make two keys collide silently.
			validationErrors = errors.Join(validationErrors, fmt.Errorf("files[%q]: path must be in canonical form, i.e. %q", path, filepath.Clean(path)))

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
