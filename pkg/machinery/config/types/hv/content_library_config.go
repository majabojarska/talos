// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package hv

//docgen:jsonschema

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/internal/registry"
	"github.com/siderolabs/talos/pkg/machinery/config/types/meta"
	"github.com/siderolabs/talos/pkg/machinery/config/validation"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

// ContentLibraryConfigKind is a config document kind.
const ContentLibraryConfigKind = "ContentLibraryConfig"

// maxNameLength is the maximum length of a content library name.
const maxNameLength = 63

// validNamePattern matches the characters a content library name may contain.
var validNamePattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// backingVolumePrefixes are the volume ID prefixes a content library may be backed by.
//
// These are the non-system volume kinds which carry a filesystem: raw and swap volumes are
// deliberately absent, as there is nothing to store files in.
var backingVolumePrefixes = []string{
	constants.UserVolumePrefix,
	constants.ExistingVolumePrefix,
	constants.ExternalVolumePrefix,
}

func init() {
	registry.Register(ContentLibraryConfigKind, func(version string) config.Document {
		switch version {
		case "v1alpha1":
			return &ContentLibraryConfigV1Alpha1{}
		default:
			return nil
		}
	})
}

// Check interfaces.
var (
	_ config.ContentLibraryConfig = &ContentLibraryConfigV1Alpha1{}
	_ config.NamedDocument        = &ContentLibraryConfigV1Alpha1{}
	_ config.Validator            = &ContentLibraryConfigV1Alpha1{}
)

// ContentLibraryConfigV1Alpha1 is a content library configuration document.
//
//	description: |
//	  ContentLibraryConfig declares a content library: a store for virtual machine images
//	  kept on a volume.
//
//	  The library is a directory named after the document, created under
//	  `content-library/` on the backing volume, so a library `my-vm-images-1` backed by the
//	  user volume `u-vm-images` lives in `/var/mnt/vm-images/content-library/my-vm-images-1`.
//	  Several libraries may share a volume.
//
//	  Contents are managed over the API with `talosctl hv content-library`, never by editing
//	  the machine configuration. Status is reported via `ContentLibraryStatus`.
//	examples:
//	  - value: exampleContentLibraryConfigV1Alpha1()
//	alias: ContentLibraryConfig
//	schemaRoot: true
//	schemaMeta: v1alpha1/ContentLibraryConfig
type ContentLibraryConfigV1Alpha1 struct {
	meta.Meta `yaml:",inline"`

	//   description: |
	//     Name of the content library.
	//
	//     Must be between 1 and 63 characters long, and can only contain ASCII letters,
	//     digits and hyphens. It names the library's directory on the backing volume, and
	//     is the ID used to address the library over the API.
	MetaName string `yaml:"name"`
	//   description: |
	//     Volume backing the content library.
	BackingConfig ContentLibraryBacking `yaml:"backing"`
}

// ContentLibraryBacking describes the storage backing a content library.
type ContentLibraryBacking struct {
	//   description: |
	//     ID of the volume storing the library's contents.
	//
	//     The volume must be declared separately, and must be a non-system volume with a
	//     filesystem: a user volume (`u-` prefix), an existing volume (`e-` prefix) or an
	//     external volume (`x-` prefix). The volume is not provisioned by this document,
	//     and the library becomes ready only once the volume is mounted.
	//   examples:
	//     - value: '"u-vm-images"'
	VolumeID string `yaml:"volume"`
}

// NewContentLibraryConfigV1Alpha1 creates a new content library config document.
func NewContentLibraryConfigV1Alpha1() *ContentLibraryConfigV1Alpha1 {
	return &ContentLibraryConfigV1Alpha1{
		Meta: meta.Meta{
			MetaKind:       ContentLibraryConfigKind,
			MetaAPIVersion: "v1alpha1",
		},
	}
}

func exampleContentLibraryConfigV1Alpha1() *ContentLibraryConfigV1Alpha1 {
	cfg := NewContentLibraryConfigV1Alpha1()
	cfg.MetaName = "my-vm-images-1"
	cfg.BackingConfig = ContentLibraryBacking{
		VolumeID: constants.UserVolumePrefix + "vm-images",
	}

	return cfg
}

// Name implements config.NamedDocument interface.
func (c *ContentLibraryConfigV1Alpha1) Name() string {
	return c.MetaName
}

// Clone implements config.Document interface.
func (c *ContentLibraryConfigV1Alpha1) Clone() config.Document {
	return c.DeepCopy()
}

// ContentLibraryConfigSignal is a signal for content library config.
func (c *ContentLibraryConfigV1Alpha1) ContentLibraryConfigSignal() {}

// BackingVolumeID implements config.ContentLibraryConfig interface.
func (c *ContentLibraryConfigV1Alpha1) BackingVolumeID() string {
	return c.BackingConfig.VolumeID
}

// Validate implements config.Validator interface.
func (c *ContentLibraryConfigV1Alpha1) Validate(validation.RuntimeMode, ...validation.Option) ([]string, error) {
	var validationErrors error

	validationErrors = errors.Join(validationErrors, c.ValidateName())
	validationErrors = errors.Join(validationErrors, c.ValidateBackingVolume())

	return nil, validationErrors
}

// ValidateName checks the content library name.
func (c *ContentLibraryConfigV1Alpha1) ValidateName() error {
	switch {
	case c.MetaName == "":
		return errors.New("name is required")
	case len(c.MetaName) > maxNameLength:
		return fmt.Errorf("name %q must be %d characters or fewer", c.MetaName, maxNameLength)
	case !validNamePattern.MatchString(c.MetaName):
		return fmt.Errorf("name %q: name can only contain ASCII letters, digits and hyphens", c.MetaName)
	}

	return nil
}

// ValidateBackingVolume checks the volume backing the content library.
func (c *ContentLibraryConfigV1Alpha1) ValidateBackingVolume() error {
	volumeID := c.BackingConfig.VolumeID

	if volumeID == "" {
		return errors.New("backing.volume is required")
	}

	for _, prefix := range backingVolumePrefixes {
		if len(volumeID) > len(prefix) && strings.HasPrefix(volumeID, prefix) {
			return nil
		}
	}

	return fmt.Errorf("backing.volume %q must be a user (%q), existing (%q) or external (%q) volume",
		volumeID, constants.UserVolumePrefix, constants.ExistingVolumePrefix, constants.ExternalVolumePrefix)
}
