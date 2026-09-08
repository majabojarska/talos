// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package container

import (
	"errors"
	"fmt"

	"github.com/hashicorp/go-multierror"

	"github.com/siderolabs/talos/pkg/machinery/config/config"
)

// validateContainerTextFilesReferences checks that every textFiles mount names a TextFilesConfig
// document that is present in the same machine configuration.
func validateContainerTextFilesReferences(containerConfigs []config.ContainerConfig, textFilesConfigs []config.TextFilesConfig) error {
	if len(containerConfigs) == 0 {
		return nil
	}

	known := make(map[string]struct{}, len(textFilesConfigs))
	for _, cfg := range textFilesConfigs {
		known[cfg.Name()] = struct{}{}
	}

	var errs error

	for _, containerConfig := range containerConfigs {
		for i, mount := range containerConfig.Mounts() {
			textFiles, present := mount.TextFiles().Get()
			if !present {
				continue
			}

			if _, exists := known[textFiles.Name()]; !exists {
				errs = multierror.Append(errs,
					fmt.Errorf("container %q: mounts[%d]: textFiles %q is not configured", containerConfig.Name(), i, textFiles.Name()))
			}
		}
	}

	if errs == nil {
		return nil
	}

	if multiErr, ok := errors.AsType[*multierror.Error](errs); ok {
		return multiErr.ErrorOrNil()
	}

	return errs
}
