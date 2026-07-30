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
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
	timeres "github.com/siderolabs/talos/pkg/machinery/resources/time"
)

type InstanceSuite struct {
	ctest.DefaultSuite
}

func TestInstanceSuite(t *testing.T) {
	t.Parallel()

	suite.Run(t, &InstanceSuite{
		DefaultSuite: ctest.DefaultSuite{
			Timeout: 15 * time.Second,
			AfterSetup: func(suite *ctest.DefaultSuite) {
				suite.Require().NoError(suite.Runtime().RegisterController(&containersctrl.InstanceController{}))
			},
		},
	})
}

// createSpec creates a ContainerSpec with the given dependsOn settings.
func (suite *InstanceSuite) createSpec(name string, mutate ...func(*containers.ContainerSpecSpec)) {
	spec := containers.NewContainerSpec(containers.NamespaceName, name)
	spec.TypedSpec().Image = "docker.io/library/nginx:latest"

	for _, m := range mutate {
		m(spec.TypedSpec())
	}

	suite.Require().NoError(suite.State().Create(suite.Ctx(), spec))
}

// markImageReady makes the image gate pass.
func (suite *InstanceSuite) markImageReady(name, digest string) {
	status := containers.NewContainerImageStatus(containers.NamespaceName, name)
	status.TypedSpec().Phase = containers.ContainerImagePhaseReady
	status.TypedSpec().Image = "docker.io/library/nginx:latest"
	status.TypedSpec().Digest = digest

	suite.Require().NoError(suite.State().Create(suite.Ctx(), status))
}

// markMountsReady makes the mount gate pass.
func (suite *InstanceSuite) markMountsReady(name string) {
	status := containers.NewContainerMountStatus(containers.NamespaceName, name)
	status.TypedSpec().Ready = true

	suite.Require().NoError(suite.State().Create(suite.Ctx(), status))
}

// setInstancePhase writes the status the runtime controller would normally produce.
func (suite *InstanceSuite) setInstancePhase(id string, containerID string, generation uint64, phase containers.ContainerInstancePhase, finishedAt time.Time) {
	status := containers.NewContainerInstanceStatus(containers.NamespaceName, id)
	status.TypedSpec().ContainerID = containerID
	status.TypedSpec().Generation = generation
	status.TypedSpec().Phase = phase
	status.TypedSpec().FinishedAt = finishedAt

	if err := suite.State().Create(suite.Ctx(), status); err != nil {
		existing, getErr := suite.State().Get(suite.Ctx(), status.Metadata())
		suite.Require().NoError(getErr)

		status.Metadata().SetVersion(existing.Metadata().Version())
		suite.Require().NoError(suite.State().Update(suite.Ctx(), status))
	}
}

func (suite *InstanceSuite) TestNoInstanceUntilImageAndMountsReady() {
	suite.createSpec("nginx")

	// Neither gate is satisfied yet, so nothing should be created.
	ctest.AssertNoResource[*containers.ContainerInstanceSpec](suite, containers.InstanceID("nginx", 0))

	suite.markImageReady("nginx", "docker.io/library/nginx@sha256:abc")

	// Image alone is not enough.
	ctest.AssertNoResource[*containers.ContainerInstanceSpec](suite, containers.InstanceID("nginx", 0))

	suite.markMountsReady("nginx")

	ctest.AssertResource(suite, containers.InstanceID("nginx", 0), func(instance *containers.ContainerInstanceSpec, asrt *assert.Assertions) {
		asrt.Equal("nginx", instance.TypedSpec().ContainerID)
		asrt.Equal(uint64(0), instance.TypedSpec().Generation)
		// The resolved digest is what runs, not the mutable tag.
		asrt.Equal("docker.io/library/nginx@sha256:abc", instance.TypedSpec().Image)
	})
}

func (suite *InstanceSuite) TestGatesOnNetworkAndTime() {
	suite.createSpec("nginx", func(spec *containers.ContainerSpecSpec) {
		spec.DependsOn.Networks = []string{"addresses"}
		spec.DependsOn.Time = true
	})
	suite.markImageReady("nginx", "sha256:abc")
	suite.markMountsReady("nginx")

	ctest.AssertNoResource[*containers.ContainerInstanceSpec](suite, containers.InstanceID("nginx", 0))

	networkStatus := network.NewStatus(network.NamespaceName, network.StatusID)
	networkStatus.TypedSpec().AddressReady = true
	suite.Require().NoError(suite.State().Create(suite.Ctx(), networkStatus))

	// Network alone is not enough while time is also required.
	ctest.AssertNoResource[*containers.ContainerInstanceSpec](suite, containers.InstanceID("nginx", 0))

	timeStatus := timeres.NewStatus()
	timeStatus.TypedSpec().Synced = true
	suite.Require().NoError(suite.State().Create(suite.Ctx(), timeStatus))

	ctest.AssertResource(suite, containers.InstanceID("nginx", 0), func(*containers.ContainerInstanceSpec, *assert.Assertions) {})
}

func (suite *InstanceSuite) TestTimeSyncDisabledSatisfiesTimeGate() {
	suite.createSpec("nginx", func(spec *containers.ContainerSpecSpec) {
		spec.DependsOn.Time = true
	})
	suite.markImageReady("nginx", "sha256:abc")
	suite.markMountsReady("nginx")

	// A node with time sync disabled would otherwise wait forever.
	timeStatus := timeres.NewStatus()
	timeStatus.TypedSpec().SyncDisabled = true
	suite.Require().NoError(suite.State().Create(suite.Ctx(), timeStatus))

	ctest.AssertResource(suite, containers.InstanceID("nginx", 0), func(*containers.ContainerInstanceSpec, *assert.Assertions) {})
}

func (suite *InstanceSuite) TestGatesOnPeerContainer() {
	for _, name := range []string{"first", "second"} {
		suite.markImageReady(name, "sha256:abc")
		suite.markMountsReady(name)
	}

	suite.createSpec("first")
	suite.createSpec("second", func(spec *containers.ContainerSpecSpec) {
		spec.DependsOn.Containers = []string{"first"}
	})

	// "first" has no dependencies, "second" waits on it.
	ctest.AssertResource(suite, containers.InstanceID("first", 0), func(*containers.ContainerInstanceSpec, *assert.Assertions) {})
	ctest.AssertNoResource[*containers.ContainerInstanceSpec](suite, containers.InstanceID("second", 0))

	// Only a running peer opens the gate.
	status := containers.NewContainerStatus(containers.NamespaceName, "first")
	status.TypedSpec().State = containers.ContainerStateRunning
	suite.Require().NoError(suite.State().Create(suite.Ctx(), status))

	ctest.AssertResource(suite, containers.InstanceID("second", 0), func(*containers.ContainerInstanceSpec, *assert.Assertions) {})
}

func (suite *InstanceSuite) TestRestartsAfterTermination() {
	suite.createSpec("nginx")
	suite.markImageReady("nginx", "sha256:abc")
	suite.markMountsReady("nginx")

	ctest.AssertResource(suite, containers.InstanceID("nginx", 0), func(*containers.ContainerInstanceSpec, *assert.Assertions) {})

	// Terminate generation 0 in the past, so the restart interval has already elapsed and the next
	// generation is due immediately. This keeps the test off the wall clock.
	suite.setInstancePhase(containers.InstanceID("nginx", 0), "nginx", 0,
		containers.ContainerInstancePhaseTerminated, time.Now().Add(-2*containersctrl.RestartInterval))

	ctest.AssertResource(suite, containers.InstanceID("nginx", 1), func(instance *containers.ContainerInstanceSpec, asrt *assert.Assertions) {
		asrt.Equal(uint64(1), instance.TypedSpec().Generation)
	})

	// The terminated instance is retained, which is what makes generation numbering work without a
	// persisted counter and what makes crash history visible.
	ctest.AssertResource(suite, containers.InstanceID("nginx", 0), func(*containers.ContainerInstanceSpec, *assert.Assertions) {})
}

func (suite *InstanceSuite) TestDoesNotRestartBeforeInterval() {
	suite.createSpec("nginx")
	suite.markImageReady("nginx", "sha256:abc")
	suite.markMountsReady("nginx")

	ctest.AssertResource(suite, containers.InstanceID("nginx", 0), func(*containers.ContainerInstanceSpec, *assert.Assertions) {})

	// Terminated just now: the next generation must not appear yet.
	suite.setInstancePhase(containers.InstanceID("nginx", 0), "nginx", 0,
		containers.ContainerInstancePhaseTerminated, time.Now())

	ctest.AssertNoResource[*containers.ContainerInstanceSpec](suite, containers.InstanceID("nginx", 1))

	// Backdating the termination past the interval must then produce it. Without this second half
	// the assertion above would pass even if the controller never restarted anything at all, so it
	// is what makes the first half mean something.
	suite.setInstancePhase(containers.InstanceID("nginx", 0), "nginx", 0,
		containers.ContainerInstancePhaseTerminated, time.Now().Add(-2*containersctrl.RestartInterval))

	ctest.AssertResource(suite, containers.InstanceID("nginx", 1), func(*containers.ContainerInstanceSpec, *assert.Assertions) {})
}

func (suite *InstanceSuite) TestSpecChangeReplacesInstance() {
	suite.createSpec("nginx")
	suite.markImageReady("nginx", "sha256:abc")
	suite.markMountsReady("nginx")

	ctest.AssertResource(suite, containers.InstanceID("nginx", 0), func(*containers.ContainerInstanceSpec, *assert.Assertions) {})

	// Change the spec: the live instance must be destroyed rather than mutated.
	spec, err := suite.State().Get(suite.Ctx(), containers.NewContainerSpec(containers.NamespaceName, "nginx").Metadata())
	suite.Require().NoError(err)

	updated := spec.(*containers.ContainerSpec) //nolint:forcetypeassert,errcheck
	updated.TypedSpec().Args = []string{"--verbose"}

	suite.Require().NoError(suite.State().Update(suite.Ctx(), updated))

	ctest.AssertNoResource[*containers.ContainerInstanceSpec](suite, containers.InstanceID("nginx", 0))
}

func (suite *InstanceSuite) TestRemovesInstancesWhenSpecGoesAway() {
	suite.createSpec("nginx")
	suite.markImageReady("nginx", "sha256:abc")
	suite.markMountsReady("nginx")

	ctest.AssertResource(suite, containers.InstanceID("nginx", 0), func(*containers.ContainerInstanceSpec, *assert.Assertions) {})

	suite.Require().NoError(suite.State().Destroy(suite.Ctx(),
		containers.NewContainerSpec(containers.NamespaceName, "nginx").Metadata()))

	ctest.AssertNoResource[*containers.ContainerInstanceSpec](suite, containers.InstanceID("nginx", 0))
}
