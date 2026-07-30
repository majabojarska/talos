// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers_test

import (
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"

	containersctrl "github.com/siderolabs/talos/internal/app/machined/pkg/controllers/containers"
	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/ctest"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/containers"
)

type MountSuite struct {
	ctest.DefaultSuite
}

func TestMountSuite(t *testing.T) {
	t.Parallel()

	suite.Run(t, &MountSuite{
		DefaultSuite: ctest.DefaultSuite{
			Timeout: 15 * time.Second,
			AfterSetup: func(suite *ctest.DefaultSuite) {
				suite.Require().NoError(suite.Runtime().RegisterController(&containersctrl.MountController{}))
			},
		},
	})
}

// mountRequestID mirrors the controller's naming so tests can find the resources it creates.
func mountRequestID(containerID, volumeID string) string {
	return "containers.MountController/" + containerID + "-" + volumeID
}

func (suite *MountSuite) createSpecWithMounts(name string, mounts ...containers.ContainerMountSpec) {
	spec := containers.NewContainerSpec(containers.NamespaceName, name)
	spec.TypedSpec().Image = "docker.io/library/nginx:latest"
	spec.TypedSpec().Mounts = mounts

	suite.Require().NoError(suite.State().Create(suite.Ctx(), spec))
}

// satisfyMount creates the VolumeMountStatus the block subsystem would produce.
func (suite *MountSuite) satisfyMount(requestID, target string) {
	status := block.NewVolumeMountStatus(block.NamespaceName, requestID)
	status.TypedSpec().Target = target

	suite.Require().NoError(suite.State().Create(suite.Ctx(), status))
}

func (suite *MountSuite) TestPassesThroughTmpfsAndHostPath() {
	suite.createSpecWithMounts("nginx",
		containers.ContainerMountSpec{
			Kind:        containers.MountKindTmpfs,
			Destination: "/tmp",
			Size:        64 << 20,
			Options:     []string{"ro"},
		},
		containers.ContainerMountSpec{
			Kind:        containers.MountKindHostPath,
			Source:      "/dev",
			Destination: "/dev",
			Options:     []string{"ro"},
		},
	)

	// Neither kind needs anything from the block subsystem, so the container is immediately ready.
	ctest.AssertResource(suite, "nginx", func(status *containers.ContainerMountStatus, asrt *assert.Assertions) {
		asrt.True(status.TypedSpec().Ready)

		// Guard the index: this closure is retried until it holds, so it must not panic on an
		// intermediate state.
		if !asrt.Len(status.TypedSpec().Mounts, 2) {
			return
		}

		asrt.Equal(uint64(64<<20), status.TypedSpec().Mounts[0].Size)
		asrt.Equal("/dev", status.TypedSpec().Mounts[1].Source)
	})
}

func (suite *MountSuite) TestRequestsAndResolvesUserVolume() {
	suite.createSpecWithMounts("nginx", containers.ContainerMountSpec{
		Kind:        containers.MountKindUserVolume,
		VolumeID:    "u-web-content",
		Destination: "/usr/share/nginx/html",
		Options:     []string{"ro"},
	})

	requestID := mountRequestID("nginx", "u-web-content")

	// The controller must ask the block subsystem for the mount.
	ctest.AssertResource(suite, requestID, func(request *block.VolumeMountRequest, asrt *assert.Assertions) {
		asrt.Equal("u-web-content", request.TypedSpec().VolumeID)
		asrt.Equal("containers.MountController/nginx", request.TypedSpec().Requester)
		asrt.True(request.TypedSpec().ReadOnly)
		// Detached would give a file descriptor with no path to bind into the container.
		asrt.False(request.TypedSpec().Detached)
	})

	// Until the volume is mounted, the container is not ready and the reason is recorded.
	ctest.AssertResource(suite, "nginx", func(status *containers.ContainerMountStatus, asrt *assert.Assertions) {
		asrt.False(status.TypedSpec().Ready)
		asrt.Contains(status.TypedSpec().Error, "u-web-content")
	})

	suite.satisfyMount(requestID, "/var/mnt/web-content")

	ctest.AssertResource(suite, "nginx", func(status *containers.ContainerMountStatus, asrt *assert.Assertions) {
		asrt.True(status.TypedSpec().Ready)

		if !asrt.Len(status.TypedSpec().Mounts, 1) {
			return
		}

		// The host path comes from the mount status target, which is only known once mounted.
		asrt.Equal("/var/mnt/web-content", status.TypedSpec().Mounts[0].Source)
		asrt.Equal("/usr/share/nginx/html", status.TypedSpec().Mounts[0].Destination)
	})

	// A finalizer must be held, or the volume could be unmounted from under the container.
	ctest.AssertResource(suite, requestID, func(status *block.VolumeMountStatus, asrt *assert.Assertions) {
		asrt.True(status.Metadata().Finalizers().Has("containers.MountController"))
	})
}

func (suite *MountSuite) TestReadWriteMountRequestsReadWrite() {
	suite.createSpecWithMounts("nginx", containers.ContainerMountSpec{
		Kind:        containers.MountKindUserVolume,
		VolumeID:    "u-data",
		Destination: "/data",
		Options:     []string{"rw"},
	})

	ctest.AssertResource(suite, mountRequestID("nginx", "u-data"), func(request *block.VolumeMountRequest, asrt *assert.Assertions) {
		asrt.False(request.TypedSpec().ReadOnly)
	})
}

func (suite *MountSuite) TestTwoContainersHoldTheSameVolumeIndependently() {
	for _, name := range []string{"first", "second"} {
		suite.createSpecWithMounts(name, containers.ContainerMountSpec{
			Kind:        containers.MountKindUserVolume,
			VolumeID:    "u-shared",
			Destination: "/shared",
			Options:     []string{"ro"},
		})
	}

	firstID := mountRequestID("first", "u-shared")
	secondID := mountRequestID("second", "u-shared")

	// Separate requests, so one container stopping cannot release the other's mount.
	ctest.AssertResource(suite, firstID, func(*block.VolumeMountRequest, *assert.Assertions) {})
	ctest.AssertResource(suite, secondID, func(*block.VolumeMountRequest, *assert.Assertions) {})

	suite.satisfyMount(firstID, "/var/mnt/shared")
	suite.satisfyMount(secondID, "/var/mnt/shared")

	ctest.AssertResource(suite, "first", func(status *containers.ContainerMountStatus, asrt *assert.Assertions) {
		asrt.True(status.TypedSpec().Ready)
	})

	// Removing one container must leave the other's request untouched.
	suite.Require().NoError(suite.State().Destroy(suite.Ctx(),
		containers.NewContainerSpec(containers.NamespaceName, "first").Metadata()))

	ctest.AssertResource(suite, secondID, func(*block.VolumeMountRequest, *assert.Assertions) {})
	ctest.AssertResource(suite, "second", func(status *containers.ContainerMountStatus, asrt *assert.Assertions) {
		asrt.True(status.TypedSpec().Ready)
	})
}

func (suite *MountSuite) TestReportsNotReadyWhileVolumeTearsDown() {
	suite.createSpecWithMounts("nginx", containers.ContainerMountSpec{
		Kind:        containers.MountKindUserVolume,
		VolumeID:    "u-going",
		Destination: "/data",
		Options:     []string{"ro"},
	})

	requestID := mountRequestID("nginx", "u-going")
	suite.satisfyMount(requestID, "/var/mnt/going")

	ctest.AssertResource(suite, "nginx", func(status *containers.ContainerMountStatus, asrt *assert.Assertions) {
		asrt.True(status.TypedSpec().Ready)
	})

	// Someone wants the volume gone.
	_, err := suite.State().Teardown(suite.Ctx(), block.NewVolumeMountStatus(block.NamespaceName, requestID).Metadata())
	suite.Require().NoError(err)

	// Not-ready is the signal the instance controller acts on to stop the container. The finalizer
	// stays held until it has, which is what stops the volume vanishing under a live task.
	ctest.AssertResource(suite, "nginx", func(status *containers.ContainerMountStatus, asrt *assert.Assertions) {
		asrt.False(status.TypedSpec().Ready)
		asrt.Contains(status.TypedSpec().Error, "unmounted")
	})

	ctest.AssertResource(suite, requestID, func(status *block.VolumeMountStatus, asrt *assert.Assertions) {
		asrt.Equal(resource.PhaseTearingDown, status.Metadata().Phase())
		asrt.True(status.Metadata().Finalizers().Has("containers.MountController"))
	})
}

func (suite *MountSuite) TestKeepsMountWhileInstanceIsLive() {
	suite.createSpecWithMounts("nginx", containers.ContainerMountSpec{
		Kind:        containers.MountKindUserVolume,
		VolumeID:    "u-busy",
		Destination: "/data",
		Options:     []string{"ro"},
	})

	requestID := mountRequestID("nginx", "u-busy")
	suite.satisfyMount(requestID, "/var/mnt/busy")

	ctest.AssertResource(suite, "nginx", func(status *containers.ContainerMountStatus, asrt *assert.Assertions) {
		asrt.True(status.TypedSpec().Ready)
	})

	// A running instance means the task may still have the path open.
	instance := containers.NewContainerInstanceStatus(containers.NamespaceName, containers.InstanceID("nginx", 0))
	instance.TypedSpec().ContainerID = "nginx"
	instance.TypedSpec().Phase = containers.ContainerInstancePhaseRunning
	suite.Require().NoError(suite.State().Create(suite.Ctx(), instance))

	// Remove the spec: the mount must NOT be released while the instance is still live.
	suite.Require().NoError(suite.State().Destroy(suite.Ctx(),
		containers.NewContainerSpec(containers.NamespaceName, "nginx").Metadata()))

	ctest.AssertResource(suite, requestID, func(*block.VolumeMountRequest, *assert.Assertions) {})

	// Once the instance finishes, the mount is released.
	instance.TypedSpec().Phase = containers.ContainerInstancePhaseTerminated
	suite.Require().NoError(suite.State().Update(suite.Ctx(), instance))

	ctest.AssertNoResource[*block.VolumeMountRequest](suite, requestID)
}
