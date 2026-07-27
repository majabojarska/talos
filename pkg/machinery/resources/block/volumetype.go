// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package block

// VolumeType describes volume type.
type VolumeType int

// Volume types.
//
//structprotogen:gen_enum
const (
	VolumeTypePartition VolumeType = iota // partition
	VolumeTypeDisk                        // disk
	VolumeTypeTmpfs                       // tmpfs
	VolumeTypeDirectory                   // directory
	VolumeTypeSymlink                     // symlink
	VolumeTypeOverlay                     // overlay
	VolumeTypeExternal                    // external
)

// IsBlockBacked returns true if volumes of this type are backed by a block device, and therefore
// get a Location in VolumeStatus.
//
// Volumes of the other types live inside a parent volume, so they never get a Location: keep this
// in sync with handleSimpleVolumeTypes in
// internal/app/machined/pkg/controllers/block/internal/volumes/locate.go.
func (t VolumeType) IsBlockBacked() bool {
	switch t {
	case VolumeTypePartition, VolumeTypeDisk, VolumeTypeExternal:
		return true
	case VolumeTypeTmpfs, VolumeTypeDirectory, VolumeTypeSymlink, VolumeTypeOverlay:
		return false
	default:
		return false
	}
}
