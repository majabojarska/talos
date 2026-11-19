// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package volumeconfig

import (
	"strconv"

	configconfig "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
)

// TextVolumeTmpfsSize is the size of the tmpfs backing a text volume.
//
// It fits the largest allowed set of files twice, as the new set of files is written before the old one is removed.
// A tmpfs only takes up memory for what it holds, so the size is merely a bound.
const TextVolumeTmpfsSize = 2 * constants.TextVolumeMaxContentsSize

// TextVolumeTransformer is the transformer for text volume configs.
func TextVolumeTransformer(c configconfig.Config) ([]VolumeResource, error) {
	if c == nil {
		return nil, nil
	}

	resources := make([]VolumeResource, 0, len(c.TextVolumeConfigs()))

	for _, textVolumeConfig := range c.TextVolumeConfigs() {
		resources = append(resources, VolumeResource{
			VolumeID: constants.TextVolumePrefix + textVolumeConfig.Name(),
			Label:    block.TextVolumeLabel,
			TransformFunc: NewBuilder().
				WithType(block.VolumeTypeTmpfs).
				WithMount(block.MountSpec{
					TargetPath:   textVolumeConfig.Name(),
					ParentID:     constants.UserVolumeMountPoint,
					SelinuxLabel: constants.EphemeralSelinuxLabel,
					FileMode:     0o755,
					UID:          0,
					GID:          0,
					Parameters: []block.ParameterSpec{
						block.NewStringParameter(block.TmpfsSizeParameter, strconv.Itoa(TextVolumeTmpfsSize)),
					},
				}).
				WriterFunc(),
			MountTransformFunc: HandleTextVolumeMountRequest,
		})
	}

	return resources, nil
}

// HandleTextVolumeMountRequest is the MountTransformFunc for text volumes.
//
// Text volumes only ever hold plain files, so the mount is always secure.
func HandleTextVolumeMountRequest(m *block.VolumeMountRequest) error {
	m.TypedSpec().DisableAccessTime = true
	m.TypedSpec().Secure = true
	m.TypedSpec().NoExec = true

	return nil
}
