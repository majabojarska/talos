// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package block

//docgen:jsonschema

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/internal/registry"
	"github.com/siderolabs/talos/pkg/machinery/config/types/meta"
	"github.com/siderolabs/talos/pkg/machinery/config/validation"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

// TextVolumeConfigKind is a config document kind.
const TextVolumeConfigKind = "TextVolumeConfig"

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
	_ config.SecretDocument      = &TextVolumeConfigV1Alpha1{}
	_ config.Validator           = &TextVolumeConfigV1Alpha1{}
)

// textVolumeDefaultFileMode is the mode of a text volume file which does not set one.
const textVolumeDefaultFileMode = 0o644

// TextVolumeFileMode represents a text volume file's permissions.
type TextVolumeFileMode fs.FileMode

// String converts file mode to octal string.
func (mode TextVolumeFileMode) String() string {
	return "0o" + strconv.FormatUint(uint64(mode), 8)
}

// MarshalYAML encodes as an octal value.
func (mode TextVolumeFileMode) MarshalYAML() (any, error) {
	return &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!int",
		Value: mode.String(),
	}, nil
}

// TextVolumeConfigV1Alpha1 is a text volume configuration document.
//
//	description: |
//	  Text volume is a set of text files kept in memory (`tmpfs`) and mounted under `/var/mnt/<name>`.
//	  It can be mounted into Talos containers with a `volume` mount in `ContainerConfig`, similar to a Kubernetes ConfigMap.
//	  The mount must set the `ro` option, as text volumes are read-only for containers.
//	  The name must not be taken by a user, existing or external volume.
//
//	  Changes to the files are applied in place, without restarting the containers which mount the volume:
//	  the files are replaced atomically as a whole, by swapping the `..data` symbolic link
//	  to a directory holding the new set of files.
//	  Each top-level entry of the volume is a symbolic link into `..data`.
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
	//     Name might be between 1 and 34 characters long and can only contain:
	//     lowercase and uppercase ASCII letters, digits, and hyphens.
	//   schemaRequired: true
	MetaName string `yaml:"name"`
	//   description: |
	//     Marks the file contents as secret.
	//
	//     Contents of a secret volume are replaced when the secrets of the machine configuration are redacted,
	//     e.g. when the configuration is shown to a user without access to secrets.
	//     The files are still rendered in full into the volume.
	//
	//     Defaults to `false`.
	SecretConfig bool `yaml:"secret,omitempty"`
	//   description: |
	//     Files of the volume, keyed by the path relative to the volume root.
	//
	//     Paths may contain subdirectories, which are created automatically.
	//     Path components must not start with `..`.
	//     The total size of the file contents must not exceed 1 MiB.
	//   schemaRequired: true
	FilesConfig map[string]TextVolumeFileV1Alpha1 `yaml:"files"`
}

// TextVolumeFileV1Alpha1 describes a single file of a text volume.
type TextVolumeFileV1Alpha1 struct {
	//   description: |
	//     The file's permissions in octal.
	//
	//     Defaults to `0o644`.
	//   schema:
	//     type: integer
	FileMode TextVolumeFileMode `yaml:"mode,omitempty"`
	//   description: |
	//     The contents of the file.
	FileContents string `yaml:"contents"`
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
	cfg.FilesConfig = map[string]TextVolumeFileV1Alpha1{
		"app.yaml": {
			FileContents: "listen: :8080\n",
		},
		"conf.d/extra.ini": {
			FileMode:     0o600,
			FileContents: "[section]\nkey=value\n",
		},
	}

	return cfg
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

// Redact implements config.SecretDocument interface.
//
// Only the contents of secret volumes are redacted.
func (s *TextVolumeConfigV1Alpha1) Redact(replacement string) {
	if !s.SecretConfig {
		return
	}

	for path, file := range s.FilesConfig {
		if file.FileContents != "" {
			file.FileContents = replacement
			s.FilesConfig[path] = file
		}
	}
}

// TextVolumeConfigSignal is a signal for text volume config.
func (s *TextVolumeConfigV1Alpha1) TextVolumeConfigSignal() {}

// Files implements config.TextVolumeConfig interface.
func (s *TextVolumeConfigV1Alpha1) Files() []config.TextVolumeFile {
	files := make([]config.TextVolumeFile, 0, len(s.FilesConfig))

	for _, path := range slices.Sorted(maps.Keys(s.FilesConfig)) {
		file := s.FilesConfig[path]

		mode := fs.FileMode(file.FileMode)
		if mode == 0 {
			mode = textVolumeDefaultFileMode
		}

		files = append(files, config.TextVolumeFile{
			Path:     path,
			Mode:     mode,
			Contents: file.FileContents,
		})
	}

	return files
}

// Validate implements config.Validator interface.
func (s *TextVolumeConfigV1Alpha1) Validate(validation.RuntimeMode, ...validation.Option) ([]string, error) {
	validationErrors := validateVolumeName(s.MetaName, constants.TextVolumePrefix)

	if len(s.FilesConfig) == 0 {
		validationErrors = errors.Join(validationErrors, errors.New("at least one file is required"))
	}

	paths := slices.Sorted(maps.Keys(s.FilesConfig))

	var totalSize int

	for _, path := range paths {
		file := s.FilesConfig[path]

		if err := validateTextVolumePath(path); err != nil {
			validationErrors = errors.Join(validationErrors, err)
		}

		for dir := filepath.Dir(path); dir != "." && dir != "/" && dir != ""; dir = filepath.Dir(dir) {
			if _, isFile := s.FilesConfig[dir]; isFile {
				validationErrors = errors.Join(validationErrors, fmt.Errorf("file %q conflicts with file %q: a path can't be both a file and a directory", dir, path))
			}
		}

		if fs.FileMode(file.FileMode)&^fs.ModePerm != 0 {
			validationErrors = errors.Join(validationErrors, fmt.Errorf("file %q: mode %s may only contain permission bits", path, file.FileMode))
		}

		totalSize += len(file.FileContents)
	}

	if totalSize > constants.TextVolumeMaxContentsSize {
		validationErrors = errors.Join(validationErrors,
			fmt.Errorf("total size of the file contents %d exceeds the limit of %d bytes", totalSize, constants.TextVolumeMaxContentsSize))
	}

	return nil, validationErrors
}

func validateTextVolumePath(path string) error {
	if path == "" {
		return errors.New("file path can't be empty")
	}

	if !filepath.IsLocal(path) || path != filepath.Clean(path) {
		return fmt.Errorf("file path %q must be a clean relative path", path)
	}

	for component := range strings.SplitSeq(path, "/") {
		if strings.HasPrefix(component, "..") {
			return fmt.Errorf("file path %q: path components starting with \"..\" are reserved", path)
		}
	}

	return nil
}
