// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package block_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"

	blockctrls "github.com/siderolabs/talos/internal/app/machined/pkg/controllers/block"
	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/block/internal/textvolume"
	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/ctest"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	blockcfg "github.com/siderolabs/talos/pkg/machinery/config/types/block"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
)

const textVolumeControllerName = "block.TextVolumeController"

type TextVolumeSuite struct {
	ctest.DefaultSuite
}

func TestTextVolumeSuite(t *testing.T) {
	t.Parallel()

	suite.Run(t, &TextVolumeSuite{
		Timeout: 10 * time.Second,
		AfterSetup: func(suite *ctest.DefaultSuite) {
			suite.Require().NoError(suite.Runtime().RegisterController(&blockctrls.TextVolumeController{}))
		},
	})
}

// setFiles declares the text volume "app-config" with the given files as the active machine config.
func (suite *TextVolumeSuite) setFiles(files map[string]string) {
	doc := blockcfg.NewTextVolumeConfigV1Alpha1()
	doc.MetaName = "app-config"
	doc.FilesSpec = files

	cfg, err := container.New(doc)
	suite.Require().NoError(err)

	machineConfig := config.NewMachineConfig(cfg)

	current, err := safe.StateGetByID[*config.MachineConfig](suite.Ctx(), suite.State(), config.ActiveID)
	if err != nil {
		suite.Create(machineConfig)

		return
	}

	machineConfig.Metadata().SetVersion(current.Metadata().Version())
	suite.Require().NoError(suite.State().Update(suite.Ctx(), machineConfig))
}

func (suite *TextVolumeSuite) request(id, name string) {
	request := block.NewTextVolumeRequest(block.NamespaceName, id)
	request.TypedSpec().TextVolumeName = name
	request.TypedSpec().Requester = "test"
	suite.Create(request)
}

// mount stands in for the block mount pipeline: it reports the instance as mounted at a directory.
func (suite *TextVolumeSuite) mount(volumeID string) string {
	target := suite.T().TempDir()

	volumeMountStatus := block.NewVolumeMountStatus(block.NamespaceName, volumeID)
	volumeMountStatus.TypedSpec().VolumeID = volumeID
	volumeMountStatus.TypedSpec().Requester = textVolumeControllerName
	volumeMountStatus.TypedSpec().Target = target
	suite.Create(volumeMountStatus)

	return target
}

func (suite *TextVolumeSuite) assertFiles(target string, files map[string]string) {
	suite.Assert().EventuallyWithT(func(collect *assert.CollectT) {
		for name, content := range files {
			actual, err := os.ReadFile(filepath.Join(target, name))
			if !assert.NoError(collect, err) {
				return
			}

			assert.Equal(collect, content, string(actual))
		}
	}, 5*time.Second, 50*time.Millisecond)
}

func (suite *TextVolumeSuite) TestInstances() {
	files := map[string]string{"app.conf": "a=1\n", "conf.d/log.ini": "level=info\n"}
	suite.setFiles(files)

	suite.request("consumer/one/app-config", "app-config")
	suite.request("consumer/two/app-config", "app-config")

	volumeID1 := textvolume.InstanceVolumeID("app-config", "consumer/one/app-config")
	volumeID2 := textvolume.InstanceVolumeID("app-config", "consumer/two/app-config")
	suite.Require().NotEqual(volumeID1, volumeID2)

	// every request gets a tmpfs volume of its own
	for _, volumeID := range []string{volumeID1, volumeID2} {
		ctest.AssertResource(suite, volumeID, func(vc *block.VolumeConfig, asrt *assert.Assertions) {
			_, labeled := vc.Metadata().Labels().Get(block.TextVolumeLabel)
			asrt.True(labeled)

			asrt.Equal(block.VolumeTypeTmpfs, vc.TypedSpec().Type)
			asrt.Equal(filepath.Join(constants.TextVolumeMountPoint, volumeID), vc.TypedSpec().Mount.TargetPath)
			asrt.Equal(constants.TextVolumeSelinuxLabel, vc.TypedSpec().Mount.SelinuxLabel)

			size, ok := block.TmpfsSize(vc.TypedSpec().Mount.Parameters)
			asrt.True(ok)
			asrt.EqualValues(textvolume.MinSize, size)
		})

		ctest.AssertResource(suite, volumeID, func(vmr *block.VolumeMountRequest, asrt *assert.Assertions) {
			asrt.Equal(volumeID, vmr.TypedSpec().VolumeID)
			asrt.Equal(textVolumeControllerName, vmr.TypedSpec().Requester)
			asrt.False(vmr.TypedSpec().ReadOnly)
			asrt.True(vmr.TypedSpec().Secure)
		})
	}

	// not ready until the files are written
	ctest.AssertResource(suite, "consumer/one/app-config", func(status *block.TextVolumeStatus, asrt *assert.Assertions) {
		asrt.Equal(volumeID1, status.TypedSpec().VolumeID)
		asrt.False(status.TypedSpec().Ready)
	})

	target1 := suite.mount(volumeID1)

	suite.assertFiles(target1, files)

	ctest.AssertResource(suite, "consumer/one/app-config", func(status *block.TextVolumeStatus, asrt *assert.Assertions) {
		asrt.True(status.TypedSpec().Ready)
		asrt.Equal(textvolume.Hash(files), status.TypedSpec().ContentHash)
		asrt.Empty(status.TypedSpec().Error)
	})

	ctest.AssertResource(suite, volumeID1, func(vms *block.VolumeMountStatus, asrt *assert.Assertions) {
		asrt.True(vms.Metadata().Finalizers().Has(textVolumeControllerName))
	})

	// the other instance is not mounted yet
	ctest.AssertResource(suite, "consumer/two/app-config", func(status *block.TextVolumeStatus, asrt *assert.Assertions) {
		asrt.False(status.TypedSpec().Ready)
	})

	// an update is written in place, to the same mount
	updated := map[string]string{"app.conf": "a=2\n", "new.conf": "new\n"}
	suite.setFiles(updated)

	suite.assertFiles(target1, updated)

	_, err := os.Stat(filepath.Join(target1, "conf.d"))
	suite.Assert().ErrorIs(err, os.ErrNotExist)

	ctest.AssertResource(suite, "consumer/one/app-config", func(status *block.TextVolumeStatus, asrt *assert.Assertions) {
		asrt.True(status.TypedSpec().Ready)
		asrt.Equal(textvolume.Hash(updated), status.TypedSpec().ContentHash)
	})

	// a request going away takes its instance with it, and leaves the other one alone
	suite.Destroy(block.NewTextVolumeRequest(block.NamespaceName, "consumer/one/app-config"))

	ctest.AssertNoResource[*block.TextVolumeStatus](suite, "consumer/one/app-config")

	ctest.AssertResource(suite, volumeID1, func(vms *block.VolumeMountStatus, asrt *assert.Assertions) {
		asrt.False(vms.Metadata().Finalizers().Has(textVolumeControllerName))
	})

	// nothing else holds the volume mount request in this test, so the instance goes away completely
	ctest.AssertNoResource[*block.VolumeMountRequest](suite, volumeID1)
	ctest.AssertNoResource[*block.VolumeConfig](suite, volumeID1)

	ctest.AssertResource(suite, volumeID2, func(vc *block.VolumeConfig, asrt *assert.Assertions) {
		asrt.Equal(resource.PhaseRunning, vc.Metadata().Phase())
	})
}

func (suite *TextVolumeSuite) TestSizeOnlyGrows() {
	suite.setFiles(map[string]string{"big": string(make([]byte, textvolume.MinSize))})

	suite.request("consumer/one/app-config", "app-config")

	volumeID := textvolume.InstanceVolumeID("app-config", "consumer/one/app-config")

	pageSize := uint64(os.Getpagesize())
	grown := 2 * ((textvolume.MinSize + pageSize - 1) / pageSize * pageSize)

	assertSize := func(expected uint64) {
		ctest.AssertResource(suite, volumeID, func(vc *block.VolumeConfig, asrt *assert.Assertions) {
			size, ok := block.TmpfsSize(vc.TypedSpec().Mount.Parameters)
			asrt.True(ok)
			asrt.Equal(expected, size)
		})
	}

	assertSize(grown)

	suite.mount(volumeID)

	// shrinking the content keeps the limit, so the tmpfs is never shrunk below what it holds
	small := map[string]string{"small": "x"}
	suite.setFiles(small)

	ctest.AssertResource(suite, "consumer/one/app-config", func(status *block.TextVolumeStatus, asrt *assert.Assertions) {
		asrt.Equal(textvolume.Hash(small), status.TypedSpec().ContentHash)
	})

	assertSize(grown)
}

func (suite *TextVolumeSuite) TestNotConfigured() {
	suite.setFiles(map[string]string{"app.conf": "a=1\n"})

	suite.request("consumer/one/nope", "nope")

	ctest.AssertResource(suite, "consumer/one/nope", func(status *block.TextVolumeStatus, asrt *assert.Assertions) {
		asrt.False(status.TypedSpec().Ready)
		asrt.Equal(`no TextVolumeConfig named "nope" is configured`, status.TypedSpec().Error)
	})

	ctest.AssertNoResource[*block.VolumeConfig](suite, textvolume.InstanceVolumeID("nope", "consumer/one/nope"))
}
