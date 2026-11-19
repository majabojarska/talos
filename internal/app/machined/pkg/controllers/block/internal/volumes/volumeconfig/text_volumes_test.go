// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package volumeconfig_test

import (
	"io/fs"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/block/internal/volumes/volumeconfig"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	blockcfg "github.com/siderolabs/talos/pkg/machinery/config/types/block"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
)

func TestTextVolumeTransformer(t *testing.T) {
	t.Parallel()

	resources, err := volumeconfig.TextVolumeTransformer(nil)
	require.NoError(t, err)
	require.Empty(t, resources)

	doc := blockcfg.NewTextVolumeConfigV1Alpha1()
	doc.MetaName = "app-config"
	doc.FilesConfig = map[string]blockcfg.TextVolumeFileV1Alpha1{
		"app.yaml": {FileContents: "a: b\n"},
	}

	cfg, err := container.New(doc)
	require.NoError(t, err)

	resources, err = volumeconfig.TextVolumeTransformer(cfg)
	require.NoError(t, err)
	require.Len(t, resources, 1)

	assert.Equal(t, constants.TextVolumePrefix+"app-config", resources[0].VolumeID)
	assert.Equal(t, block.TextVolumeLabel, resources[0].Label)

	testTransformFunc(t, resources[0].TransformFunc, func(t *testing.T, vc *block.VolumeConfig, err error) {
		require.NoError(t, err)

		assert.Equal(t, block.VolumeTypeTmpfs, vc.TypedSpec().Type)
		assert.Equal(t, "app-config", vc.TypedSpec().Mount.TargetPath)
		assert.Equal(t, constants.UserVolumeMountPoint, vc.TypedSpec().Mount.ParentID)
		assert.Equal(t, constants.EphemeralSelinuxLabel, vc.TypedSpec().Mount.SelinuxLabel)
		assert.Equal(t, fs.FileMode(0o755), vc.TypedSpec().Mount.FileMode)
		assert.Equal(t,
			[]block.ParameterSpec{block.NewStringParameter(block.TmpfsSizeParameter, strconv.Itoa(2*constants.TextVolumeMaxContentsSize))},
			vc.TypedSpec().Mount.Parameters,
		)
		require.NoError(t, block.ValidateTmpfsParameters(vc.TypedSpec().Mount.Parameters))
	})

	testMountTransformFunc(t, resources[0].MountTransformFunc, func(t *testing.T, m *block.VolumeMountRequest, err error) {
		require.NoError(t, err)

		assert.True(t, m.TypedSpec().Secure)
		assert.True(t, m.TypedSpec().NoExec)
		assert.True(t, m.TypedSpec().DisableAccessTime)
		assert.False(t, m.TypedSpec().ReadOnly)
	})
}
