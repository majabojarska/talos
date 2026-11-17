// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package container

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/hashicorp/go-multierror"

	"github.com/siderolabs/talos/pkg/machinery/config/config"
)

// validateContainerVolumeMounts checks the volumes mounted into containers.
//
// A volume mount names a UserVolumeConfig, ExistingVolumeConfig or ExternalVolumeConfig, and a text
// volume mount names a TextVolumeConfig. Both are resolved against the rest of the configuration, so a
// name nothing declares would leave the container waiting for its volume forever. A mount request is
// read-only only while every requester asks for it, so a writable mount of a volume declared read-only
// would remount the volume against what the user declared; such a mount has to say `ro`.
func validateContainerVolumeMounts(container *Container) error {
	configs := container.ContainerConfigs()

	if len(configs) == 0 {
		return nil
	}

	// Sorted by name, so a given configuration always reports errors in the same order.
	sorted := slices.SortedFunc(slices.Values(configs), func(a, b config.ContainerConfig) int {
		return cmp.Compare(a.Name(), b.Name())
	})

	var errs *multierror.Error

	for _, containerConfig := range sorted {
		for i, mount := range containerConfig.Mounts() {
			if err := validateMount(container, mount); err != nil {
				errs = multierror.Append(errs, fmt.Errorf("container %q: mounts[%d]: %w", containerConfig.Name(), i, err))
			}
		}
	}

	return errs.ErrorOrNil()
}

// validateMount checks that a volume or text volume mount names a declared volume of its kind, with the
// options it requires.
//
// A missing name is reported by the document's own validation.
func validateMount(container *Container, mount config.ContainerMountConfig) error {
	if textVolumeMount, ok := mount.TextVolume().Get(); ok && textVolumeMount.Name() != "" {
		if _, declared := config.ResolveTextVolume(container, textVolumeMount.Name()); !declared {
			return fmt.Errorf("no TextVolumeConfig declares text volume %q", textVolumeMount.Name())
		}

		return nil
	}

	volumeMount, ok := mount.Volume().Get()
	if !ok || volumeMount.Name() == "" {
		return nil
	}

	backing, resolved := config.ResolveBackingVolume(container, volumeMount.Name())

	switch {
	case !resolved:
		if _, isTextVolume := config.ResolveTextVolume(container, volumeMount.Name()); isTextVolume {
			return fmt.Errorf("volume %q is a TextVolumeConfig, so it must be mounted with textVolume", volumeMount.Name())
		}

		return fmt.Errorf("no UserVolumeConfig, ExistingVolumeConfig or ExternalVolumeConfig declares volume %q", volumeMount.Name())
	case backing.ReadOnly && !slices.Contains(volumeMount.MountOptions(), "ro"):
		return fmt.Errorf("volume %q is configured read-only, so it must be mounted with the ro option", volumeMount.Name())
	}

	return nil
}
