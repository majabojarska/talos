// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"

	containersctrl "github.com/siderolabs/talos/internal/app/machined/pkg/controllers/containers"
	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/ctest"
	"github.com/siderolabs/talos/pkg/machinery/resources/containers"
)

type StatusSuite struct {
	ctest.DefaultSuite
}

func TestStatusSuite(t *testing.T) {
	t.Parallel()

	suite.Run(t, &StatusSuite{
		DefaultSuite: ctest.DefaultSuite{
			Timeout: 10 * time.Second,
			AfterSetup: func(suite *ctest.DefaultSuite) {
				suite.Require().NoError(suite.Runtime().RegisterController(&containersctrl.StatusController{}))
			},
		},
	})
}

// statusContainer is the container every case in this suite is about. Status projection does not
// depend on the name, so one container keeps each case to its point.
const statusContainer = "nginx"

func (suite *StatusSuite) spec(mutate ...func(*containers.ContainerSpecSpec)) {
	spec := containers.NewContainerSpec(containers.NamespaceName, statusContainer)
	spec.TypedSpec().Image = "docker.io/library/nginx:latest"

	for _, m := range mutate {
		m(spec.TypedSpec())
	}

	suite.Require().NoError(suite.State().Create(suite.Ctx(), spec))
}

func (suite *StatusSuite) imageStatus(phase containers.ContainerImagePhase, digest, errText string) {
	status := containers.NewContainerImageStatus(containers.NamespaceName, statusContainer)
	status.TypedSpec().Phase = phase
	status.TypedSpec().Image = "docker.io/library/nginx:latest"
	status.TypedSpec().Digest = digest
	status.TypedSpec().Error = errText

	suite.Require().NoError(suite.State().Create(suite.Ctx(), status))
}

func (suite *StatusSuite) mountStatus(ready bool, errText string) {
	status := containers.NewContainerMountStatus(containers.NamespaceName, statusContainer)
	status.TypedSpec().Ready = ready
	status.TypedSpec().Error = errText

	suite.Require().NoError(suite.State().Create(suite.Ctx(), status))
}

func (suite *StatusSuite) instanceStatus(generation uint64, phase containers.ContainerInstancePhase, mutate ...func(*containers.ContainerInstanceStatusSpec)) {
	status := containers.NewContainerInstanceStatus(containers.NamespaceName, containers.InstanceID(statusContainer, generation))
	status.TypedSpec().ContainerID = statusContainer
	status.TypedSpec().Generation = generation
	status.TypedSpec().Phase = phase

	for _, m := range mutate {
		m(status.TypedSpec())
	}

	suite.Require().NoError(suite.State().Create(suite.Ctx(), status))
}

func (suite *StatusSuite) TestPendingWhenGatesClosed() {
	suite.spec(func(spec *containers.ContainerSpecSpec) {
		spec.DependsOn.Networks = []string{"addresses"}
	})
	suite.imageStatus(containers.ContainerImagePhaseReady, "sha256:abc", "")
	suite.mountStatus(true, "")

	ctest.AssertResource(suite, "nginx", func(status *containers.ContainerStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerStatePending, status.TypedSpec().State)
		asrt.Equal(containers.ContainerHealthPending, status.TypedSpec().Health)
		// waitingFor names the specific unmet condition, using the same evaluation the instance
		// controller gates on, so the two can never disagree.
		asrt.Contains(status.TypedSpec().WaitingFor, "network: addresses")
	})
}

func (suite *StatusSuite) TestPullingWhileImagePulls() {
	suite.spec()
	suite.imageStatus(containers.ContainerImagePhasePulling, "", "")
	suite.mountStatus(true, "")

	ctest.AssertResource(suite, "nginx", func(status *containers.ContainerStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerStatePulling, status.TypedSpec().State)
		asrt.Equal(containers.ContainerHealthPulling, status.TypedSpec().Health)
	})
}

func (suite *StatusSuite) TestRunningReportsPIDAndDigest() {
	suite.spec()
	suite.imageStatus(containers.ContainerImagePhaseReady, "docker.io/library/nginx@sha256:abc", "")
	suite.mountStatus(true, "")
	suite.instanceStatus(0, containers.ContainerInstancePhaseRunning, func(spec *containers.ContainerInstanceStatusSpec) {
		spec.PID = 4242
	})

	ctest.AssertResource(suite, "nginx", func(status *containers.ContainerStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerStateRunning, status.TypedSpec().State)
		asrt.Equal(containers.ContainerHealthHealthy, status.TypedSpec().Health)
		asrt.Equal(uint32(4242), status.TypedSpec().PID)
		// The digest is reported, not the mutable tag that was requested.
		asrt.Equal("docker.io/library/nginx@sha256:abc", status.TypedSpec().Image)
	})
}

func (suite *StatusSuite) TestTerminatedIsBackoffNotTerminal() {
	suite.spec()
	suite.imageStatus(containers.ContainerImagePhaseReady, "sha256:abc", "")
	suite.mountStatus(true, "")
	suite.instanceStatus(3, containers.ContainerInstancePhaseTerminated, func(spec *containers.ContainerInstanceStatusSpec) {
		spec.ExitCode = 137
		spec.Error = "killed"
	})

	ctest.AssertResource(suite, "nginx", func(status *containers.ContainerStatus, asrt *assert.Assertions) {
		// Containers restart unconditionally, so a finished instance means a restart is pending.
		asrt.Equal(containers.ContainerStateBackoff, status.TypedSpec().State)
		asrt.Equal(containers.ContainerHealthDegraded, status.TypedSpec().Health)
		asrt.Equal(int32(137), status.TypedSpec().ExitCode)
		asrt.Equal("killed", status.TypedSpec().Error)
		// restartCount is the current generation, derived rather than accumulated.
		asrt.Equal(uint64(3), status.TypedSpec().RestartCount)
		// PID is cleared once the task is gone.
		asrt.Zero(status.TypedSpec().PID)
	})
}

func (suite *StatusSuite) TestReportsNewestInstanceOnly() {
	suite.spec()
	suite.imageStatus(containers.ContainerImagePhaseReady, "sha256:abc", "")
	suite.mountStatus(true, "")

	// Two retained generations: only the newer one should shape ContainerStatus.
	suite.instanceStatus(0, containers.ContainerInstancePhaseTerminated, func(spec *containers.ContainerInstanceStatusSpec) {
		spec.ExitCode = 1
	})
	suite.instanceStatus(1, containers.ContainerInstancePhaseRunning, func(spec *containers.ContainerInstanceStatusSpec) {
		spec.PID = 99
	})

	ctest.AssertResource(suite, "nginx", func(status *containers.ContainerStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerStateRunning, status.TypedSpec().State)
		asrt.Equal(uint32(99), status.TypedSpec().PID)
		asrt.Equal(uint64(1), status.TypedSpec().RestartCount)
	})
}

func (suite *StatusSuite) TestErrorPrecedence() {
	suite.spec()
	// Both the image and the mounts failed; the more specific stage wins.
	suite.imageStatus(containers.ContainerImagePhaseFailed, "", "pull denied")
	suite.mountStatus(false, "volume missing")

	ctest.AssertResource(suite, "nginx", func(status *containers.ContainerStatus, asrt *assert.Assertions) {
		asrt.Equal("volume missing", status.TypedSpec().Error)
	})
}

func (suite *StatusSuite) TestRemovesStatusWhenSpecGoesAway() {
	suite.spec()
	suite.imageStatus(containers.ContainerImagePhaseReady, "sha256:abc", "")
	suite.mountStatus(true, "")

	ctest.AssertResource(suite, "nginx", func(*containers.ContainerStatus, *assert.Assertions) {})

	suite.Require().NoError(suite.State().Destroy(suite.Ctx(),
		containers.NewContainerSpec(containers.NamespaceName, "nginx").Metadata()))

	// A removed document has its status destroyed rather than parked in a final value.
	ctest.AssertNoResource[*containers.ContainerStatus](suite, "nginx")
}
