// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package block

import "github.com/siderolabs/gen/xslices"

// conflictingVolumeKinds returns the volume document kinds which share a name namespace with
// selfKind.
//
// These are the kinds a consumer (e.g. a container volume mount) refers to by name: two documents of
// different kinds under one name would make the name no longer identify a single volume, and the
// user, existing and external ones would claim the same mount point at `/var/mnt/<name>`. Raw and swap
// volumes are deliberately absent, as they are never mounted by name.
func conflictingVolumeKinds(selfKind string) []string {
	return xslices.Filter([]string{
		UserVolumeConfigKind,
		ExistingVolumeConfigKind,
		ExternalVolumeConfigKind,
		TextVolumeConfigKind,
	}, func(kind string) bool {
		return kind != selfKind
	})
}
