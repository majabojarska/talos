// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package block_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource/rtestutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
	"golang.org/x/sys/unix"

	blockctrls "github.com/siderolabs/talos/internal/app/machined/pkg/controllers/block"
	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/ctest"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
)

type MountSuite struct {
	ctest.DefaultSuite
}

func TestMountSuite(t *testing.T) {
	t.Parallel()

	suite.Run(t, &MountSuite{
		Timeout: 30 * time.Second,
		AfterSetup: func(suite *ctest.DefaultSuite) {
			suite.Require().NoError(suite.Runtime().RegisterController(&blockctrls.MountController{}))
		},
	})
}

func (suite *MountSuite) mountVolume(volumeID string) {
	mountRequest := block.NewMountRequest(block.NamespaceName, volumeID)
	mountRequest.TypedSpec().RequesterIDs = []string{"requester1/" + volumeID}
	mountRequest.TypedSpec().Requesters = []string{"requester1"}
	mountRequest.TypedSpec().VolumeID = volumeID
	suite.Create(mountRequest)

	// wait for the mount status to be created
	ctest.AssertResource(suite, volumeID, func(*block.MountStatus, *assert.Assertions) {})
}

func (suite *MountSuite) TestSymlinkNew() {
	dir := suite.T().TempDir()
	targetPath := filepath.Join(dir, "target")

	volumeStatus := block.NewVolumeStatus(block.NamespaceName, "volume1")
	volumeStatus.TypedSpec().Type = block.VolumeTypeSymlink
	volumeStatus.TypedSpec().SymlinkSpec = block.SymlinkProvisioningSpec{
		SymlinkTargetPath: "/run",
		Force:             true,
	}
	volumeStatus.TypedSpec().MountSpec = block.MountSpec{
		TargetPath: targetPath,
	}
	volumeStatus.TypedSpec().Phase = block.VolumePhaseReady
	suite.Create(volumeStatus)

	suite.mountVolume("volume1")

	// verify symlink
	path, err := os.Readlink(targetPath)
	suite.Require().NoError(err)
	suite.Assert().Equal("/run", path)
}

func (suite *MountSuite) TestSymlinkExists() {
	dir := suite.T().TempDir()
	targetPath := filepath.Join(dir, "target")

	// symlink already exists
	suite.Require().NoError(os.Symlink("/run", targetPath))

	volumeStatus := block.NewVolumeStatus(block.NamespaceName, "volume1")
	volumeStatus.TypedSpec().Type = block.VolumeTypeSymlink
	volumeStatus.TypedSpec().SymlinkSpec = block.SymlinkProvisioningSpec{
		SymlinkTargetPath: "/run",
	}
	volumeStatus.TypedSpec().MountSpec = block.MountSpec{
		TargetPath: targetPath,
	}
	volumeStatus.TypedSpec().Phase = block.VolumePhaseReady
	suite.Create(volumeStatus)

	suite.mountVolume("volume1")

	// verify symlink
	path, err := os.Readlink(targetPath)
	suite.Require().NoError(err)
	suite.Assert().Equal("/run", path)
}

func (suite *MountSuite) TestSymlinkWrong() {
	dir := suite.T().TempDir()
	targetPath := filepath.Join(dir, "target")

	// wrong symlink target
	suite.Require().NoError(os.Symlink("/foo", targetPath))

	volumeStatus := block.NewVolumeStatus(block.NamespaceName, "volume1")
	volumeStatus.TypedSpec().Type = block.VolumeTypeSymlink
	volumeStatus.TypedSpec().SymlinkSpec = block.SymlinkProvisioningSpec{
		SymlinkTargetPath: "/run",
		Force:             true,
	}
	volumeStatus.TypedSpec().MountSpec = block.MountSpec{
		TargetPath: targetPath,
	}
	volumeStatus.TypedSpec().Phase = block.VolumePhaseReady
	suite.Create(volumeStatus)

	suite.mountVolume("volume1")

	// verify symlink
	path, err := os.Readlink(targetPath)
	suite.Require().NoError(err)
	suite.Assert().Equal("/run", path)
}

func (suite *MountSuite) TestSymlinkDirectory() {
	dir := suite.T().TempDir()
	targetPath := filepath.Join(dir, "target")

	// non-empty directory structure
	suite.Require().NoError(os.Mkdir(targetPath, 0o755))
	suite.Require().NoError(os.Mkdir(filepath.Join(targetPath, "foo"), 0o755))

	volumeStatus := block.NewVolumeStatus(block.NamespaceName, "volume1")
	volumeStatus.TypedSpec().Type = block.VolumeTypeSymlink
	volumeStatus.TypedSpec().SymlinkSpec = block.SymlinkProvisioningSpec{
		SymlinkTargetPath: "/run",
		Force:             true,
	}
	volumeStatus.TypedSpec().MountSpec = block.MountSpec{
		TargetPath: targetPath,
	}
	volumeStatus.TypedSpec().Phase = block.VolumePhaseReady
	suite.Create(volumeStatus)

	suite.mountVolume("volume1")

	// verify symlink
	path, err := os.Readlink(targetPath)
	suite.Require().NoError(err)
	suite.Assert().Equal("/run", path)
}

func (suite *MountSuite) TestUserVolumeMountPointCleanup() {
	dir := suite.T().TempDir()

	// leftover empty mount point
	suite.Require().NoError(os.Mkdir(filepath.Join(dir, "empty"), 0o755))
	// non-empty directory
	suite.Require().NoError(os.Mkdir(filepath.Join(dir, "data"), 0o755))
	suite.Require().NoError(os.WriteFile(filepath.Join(dir, "data", "file"), []byte("data"), 0o644))
	// regular file
	suite.Require().NoError(os.WriteFile(filepath.Join(dir, "file"), []byte("data"), 0o644))

	volumeStatus := block.NewVolumeStatus(block.NamespaceName, constants.UserVolumeMountPoint)
	volumeStatus.TypedSpec().Type = block.VolumeTypeDirectory
	volumeStatus.TypedSpec().MountSpec = block.MountSpec{
		TargetPath: dir,
		FileMode:   0o755,
		UID:        os.Getuid(),
		GID:        os.Getgid(),
	}
	volumeStatus.TypedSpec().Phase = block.VolumePhaseReady
	suite.Create(volumeStatus)

	suite.mountVolume(constants.UserVolumeMountPoint)

	suite.Assert().NoDirExists(filepath.Join(dir, "empty"))
	suite.Assert().FileExists(filepath.Join(dir, "data", "file"))
	suite.Assert().FileExists(filepath.Join(dir, "file"))
}

// TestUnmountBusy verifies that a busy unmount neither holds back the teardown of other mounts,
// nor waits for the controller restart backoff once released.
func (suite *MountSuite) TestUnmountBusy() {
	if os.Getuid() != 0 {
		suite.T().Skip("can't run the test as non-root")
	}

	dir := suite.T().TempDir()

	// mount requests are processed in ID order, so the busy one goes first
	for _, id := range []string{"a-busy", "b-other"} {
		volumeStatus := block.NewVolumeStatus(block.NamespaceName, id)
		volumeStatus.TypedSpec().Type = block.VolumeTypeDirectory
		volumeStatus.TypedSpec().MountSpec = block.MountSpec{
			TargetPath: filepath.Join(dir, id),
			BindTarget: new(filepath.Join(dir, id+"-bind")),
			FileMode:   0o755,
		}
		volumeStatus.TypedSpec().Phase = block.VolumePhaseReady
		suite.Create(volumeStatus)

		suite.mountVolume(id)
	}

	// an open directory keeps the bind mount busy
	busy, err := os.Open(filepath.Join(dir, "a-busy-bind"))
	suite.Require().NoError(err)

	for _, id := range []string{"a-busy", "b-other"} {
		_, err = suite.State().Teardown(suite.Ctx(), block.NewMountRequest(block.NamespaceName, id).Metadata())
		suite.Require().NoError(err)
	}

	// each busy unmount attempt blocks for ~5s retrying EBUSY, and "b-other" may wait behind two of them:
	// one in an iteration that ran before its teardown, one in the iteration handling it
	ctx, cancel := context.WithTimeout(suite.Ctx(), 20*time.Second)
	defer cancel()

	rtestutils.AssertResource(ctx, suite.T(), suite.State(), "b-other", func(mountRequest *block.MountRequest, asrt *assert.Assertions) {
		asrt.True(mountRequest.Metadata().Finalizers().Empty())
	})

	ctest.AssertResource(suite, "a-busy", func(mountRequest *block.MountRequest, asrt *assert.Assertions) {
		asrt.False(mountRequest.Metadata().Finalizers().Empty())
	})

	suite.Require().NoError(busy.Close())

	// any input event retries the unmount, well before the retry interval
	suite.Create(block.NewVolumeStatus(block.NamespaceName, "c-trigger"))

	ctest.AssertResource(suite, "a-busy", func(mountRequest *block.MountRequest, asrt *assert.Assertions) {
		asrt.True(mountRequest.Metadata().Finalizers().Empty())
	})
}

func (suite *MountSuite) TestTmpfsRequiresSize() {
	targetPath := filepath.Join(suite.T().TempDir(), "tmpfs")

	volumeStatus := block.NewVolumeStatus(block.NamespaceName, "tmpfs-volume")
	volumeStatus.TypedSpec().Type = block.VolumeTypeTmpfs
	volumeStatus.TypedSpec().MountSpec = block.MountSpec{
		TargetPath: targetPath,
	}
	volumeStatus.TypedSpec().Phase = block.VolumePhaseReady
	suite.Create(volumeStatus)

	mountRequest := block.NewMountRequest(block.NamespaceName, "tmpfs-volume")
	mountRequest.TypedSpec().RequesterIDs = []string{"requester1/tmpfs-volume"}
	mountRequest.TypedSpec().Requesters = []string{"requester1"}
	mountRequest.TypedSpec().VolumeID = "tmpfs-volume"
	suite.Create(mountRequest)

	ctest.AssertNoResource[*block.MountStatus](suite, "tmpfs-volume")

	_, err := os.Stat(targetPath)
	suite.Require().ErrorIs(err, os.ErrNotExist)
}

func (suite *MountSuite) TestTmpfsResizeLive() {
	if os.Getuid() != 0 {
		suite.T().Skip("can't run the test as non-root")
	}

	targetPath := filepath.Join(suite.T().TempDir(), "tmpfs")

	volumeStatus := block.NewVolumeStatus(block.NamespaceName, "tmpfs-volume")
	volumeStatus.TypedSpec().Type = block.VolumeTypeTmpfs
	volumeStatus.TypedSpec().MountSpec = block.MountSpec{
		TargetPath: targetPath,
		FileMode:   0o755,
		Parameters: []block.ParameterSpec{block.NewStringParameter(block.TmpfsSizeParameter, "1048576")},
	}
	volumeStatus.TypedSpec().Phase = block.VolumePhaseReady
	suite.Create(volumeStatus)

	suite.mountVolume("tmpfs-volume")

	assertSize := func(expected int64) {
		suite.Assert().EventuallyWithT(func(collect *assert.CollectT) {
			var st unix.Statfs_t

			if !assert.NoError(collect, unix.Statfs(targetPath, &st)) {
				return
			}

			assert.Equal(collect, uint32(unix.TMPFS_MAGIC), uint32(st.Type))
			assert.Equal(collect, expected, int64(st.Blocks)*st.Bsize)
		}, 10*time.Second, 50*time.Millisecond)
	}

	assertSize(1 << 20)

	// content must survive the resize: the superblock is reconfigured, not replaced
	suite.Require().NoError(os.WriteFile(filepath.Join(targetPath, "file"), []byte("content"), 0o644))

	ctest.UpdateWithConflicts(suite, volumeStatus, func(vs *block.VolumeStatus) error {
		vs.TypedSpec().MountSpec.Parameters = []block.ParameterSpec{block.NewStringParameter(block.TmpfsSizeParameter, "2097152")}

		return nil
	})

	assertSize(2 << 20)
	suite.Assert().FileExists(filepath.Join(targetPath, "file"))

	_, err := suite.State().Teardown(suite.Ctx(), block.NewMountRequest(block.NamespaceName, "tmpfs-volume").Metadata())
	suite.Require().NoError(err)

	ctest.AssertResource(suite, "tmpfs-volume", func(mountRequest *block.MountRequest, asrt *assert.Assertions) {
		asrt.True(mountRequest.Metadata().Finalizers().Empty())
	})

	// the mount point was created by the controller, so it is removed on unmount
	_, err = os.Stat(targetPath)
	suite.Require().ErrorIs(err, os.ErrNotExist)
}
