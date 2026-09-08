// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package container

import (
	"errors"
	"fmt"
	"slices"

	"github.com/hashicorp/go-multierror"

	"github.com/siderolabs/talos/pkg/machinery/config/config"
	containertypes "github.com/siderolabs/talos/pkg/machinery/config/types/container"
)

// validateContainerTextFilesReferences checks that every textFiles mount names a TextFilesConfig
// document that is present in the same machine configuration.
//
// Rejected at apply time for the same reason as an unresolved dependsOn.containers reference: a
// container whose mount can never resolve sits in pending forever, which is much harder to diagnose
// on a live machine than a rejected apply-config.
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

// validateContainerTextFilesSize warns when a TextFilesConfig document, materialized once per
// referencing container by TextFilesController, would use enough tmpfs memory in total to be
// worth flagging.
//
// TextFilesConfig.Validate warns when a single document's own content exceeds the threshold, but it
// only ever sees one document: it cannot tell how many containers mount it, even though each mount
// costs an independent copy on disk. This container-level check has visibility into every
// ContainerConfig, so it multiplies the document's size by however many containers actually
// reference it. A document referenced by at most one container is already covered by the
// per-document warning, so it is skipped here to avoid warning twice.
func validateContainerTextFilesSize(containerConfigs []config.ContainerConfig, textFilesConfigs []config.TextFilesConfig) []string {
	if len(containerConfigs) == 0 || len(textFilesConfigs) == 0 {
		return nil
	}

	refCount := map[string]int{}

	for _, containerConfig := range containerConfigs {
		for _, mount := range containerConfig.Mounts() {
			textFiles, present := mount.TextFiles().Get()
			if !present {
				continue
			}

			refCount[textFiles.Name()]++
		}
	}

	var warnings []string

	for _, cfg := range textFilesConfigs {
		count := refCount[cfg.Name()]
		if count <= 1 {
			continue
		}

		total := containertypes.TextFilesTotalSize(cfg.FileContents())
		materialized := total * count

		if materialized > containertypes.TextFilesWarnSize {
			warnings = append(warnings, fmt.Sprintf(
				"text files %q: content is %d bytes and is mounted by %d containers, for %d bytes of tmpfs across the host",
				cfg.Name(), total, count, materialized,
			))
		}
	}

	slices.Sort(warnings)

	return warnings
}
