// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"

	containersctrl "github.com/siderolabs/talos/internal/app/machined/pkg/controllers/containers"
	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/ctest"
	"github.com/siderolabs/talos/pkg/machinery/resources/containers"
	"github.com/siderolabs/talos/pkg/machinery/resources/v1alpha1"
)

// fakeRunner stands in for containerd. A container runs until released, then returns the configured
// exit code, which is enough to exercise the whole lifecycle without any real processes.
type fakeRunner struct {
	mu sync.Mutex

	// existing seeds the container list, used to test the orphan sweep.
	existing []string
	removed  []string

	// results per instance ID.
	exitCodes map[string]int32
	errs      map[string]error

	// release channels per instance ID; a running container blocks on its channel.
	release map[string]chan struct{}
	running map[string]bool
	started []string
}

func newFakeRunner(existing ...string) *fakeRunner {
	return &fakeRunner{
		existing:  existing,
		exitCodes: map[string]int32{},
		errs:      map[string]error{},
		release:   map[string]chan struct{}{},
		running:   map[string]bool{},
	}
}

func (f *fakeRunner) List(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.existing), nil
}

func (f *fakeRunner) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.removed = append(f.removed, id)
	f.existing = slices.DeleteFunc(f.existing, func(s string) bool { return s == id })

	return nil
}

func (f *fakeRunner) Run(ctx context.Context, id string, _ containers.ContainerInstanceSpecSpec, started func(uint32)) (int32, error) {
	f.mu.Lock()

	if err := f.errs[id]; err != nil {
		f.mu.Unlock()

		// A setup failure never calls started, which is how "failed" is told from "terminated".
		return 0, err
	}

	ch, blocked := f.release[id]
	exitCode := f.exitCodes[id]
	f.running[id] = true
	f.started = append(f.started, id)
	f.mu.Unlock()

	started(4242)

	if blocked {
		select {
		case <-ctx.Done():
			// Stop requested: report the exit code a SIGTERM'd process would give.
			f.setStopped(id)

			return 143, nil
		case <-ch:
		}
	}

	f.setStopped(id)

	return exitCode, nil
}

func (f *fakeRunner) setStopped(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.running[id] = false
}

func (f *fakeRunner) Close() error { return nil }

func (f *fakeRunner) blockOn(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.release[id] = make(chan struct{})
}

func (f *fakeRunner) releaseID(id string, exitCode int32) {
	f.mu.Lock()
	ch := f.release[id]
	delete(f.release, id)
	f.exitCodes[id] = exitCode
	f.mu.Unlock()

	if ch != nil {
		close(ch)
	}
}

func (f *fakeRunner) failWith(id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.errs[id] = err
}

func (f *fakeRunner) wasRemoved(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Contains(f.removed, id)
}

func (f *fakeRunner) startCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0

	for _, s := range f.started {
		if s == id {
			n++
		}
	}

	return n
}

type RuntimeSuite struct {
	ctest.DefaultSuite

	runner *fakeRunner
}

func TestRuntimeSuite(t *testing.T) {
	t.Parallel()

	runner := newFakeRunner()

	suite.Run(t, &RuntimeSuite{
		runner: runner,
		DefaultSuite: ctest.DefaultSuite{
			Timeout: 15 * time.Second,
			AfterSetup: func(suite *ctest.DefaultSuite) {
				suite.Require().NoError(suite.Runtime().RegisterController(&containersctrl.RuntimeController{
					RunnerProvider: func() (containersctrl.TaskRunner, error) { return runner, nil },
				}))
			},
		},
	})
}

func (suite *RuntimeSuite) criUp() {
	service := v1alpha1.NewService("cri")
	service.TypedSpec().Running = true
	service.TypedSpec().Healthy = true

	suite.Require().NoError(suite.State().Create(suite.Ctx(), service))
}

// createInstance writes the spec the instance controller would normally produce, for the first
// generation. Restarts are the instance controller's job, so one generation is all this suite needs.
func (suite *RuntimeSuite) createInstance(containerID string) {
	const generation = 0

	spec := containers.NewContainerInstanceSpec(containers.NamespaceName, containers.InstanceID(containerID, generation))
	spec.TypedSpec().ContainerID = containerID
	spec.TypedSpec().Generation = generation
	spec.TypedSpec().Image = "docker.io/library/nginx@sha256:abc"

	suite.Require().NoError(suite.State().Create(suite.Ctx(), spec))
}

func (suite *RuntimeSuite) TestRunsAndReportsRunning() {
	suite.criUp()

	id := containers.InstanceID("nginx", 0)
	suite.runner.blockOn(id)
	suite.createInstance("nginx")

	ctest.AssertResource(suite, id, func(status *containers.ContainerInstanceStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerInstancePhaseRunning, status.TypedSpec().Phase)
		asrt.Equal(uint32(4242), status.TypedSpec().PID)
		asrt.Equal("nginx", status.TypedSpec().ContainerID)
		asrt.False(status.TypedSpec().StartedAt.IsZero())
	})

	// A finalizer must be held so the instance controller cannot destroy the spec while the task
	// is still running.
	ctest.AssertResource(suite, id, func(spec *containers.ContainerInstanceSpec, asrt *assert.Assertions) {
		asrt.True(spec.Metadata().Finalizers().Has("containers.RuntimeController"))
	})

	suite.runner.releaseID(id, 0)

	ctest.AssertResource(suite, id, func(status *containers.ContainerInstanceStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerInstancePhaseTerminated, status.TypedSpec().Phase)
		asrt.Zero(status.TypedSpec().ExitCode)
		asrt.Zero(status.TypedSpec().PID)
		asrt.False(status.TypedSpec().FinishedAt.IsZero())
	})
}

func (suite *RuntimeSuite) TestReportsNonZeroExit() {
	suite.criUp()

	id := containers.InstanceID("crasher", 0)
	suite.runner.releaseID(id, 137)
	suite.createInstance("crasher")

	ctest.AssertResource(suite, id, func(status *containers.ContainerInstanceStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerInstancePhaseTerminated, status.TypedSpec().Phase)
		asrt.Equal(int32(137), status.TypedSpec().ExitCode)
	})
}

func (suite *RuntimeSuite) TestSetupFailureIsFailedNotTerminated() {
	suite.criUp()

	id := containers.InstanceID("broken", 0)
	suite.runner.failWith(id, errors.New("no such image"))
	suite.createInstance("broken")

	ctest.AssertResource(suite, id, func(status *containers.ContainerInstanceStatus, asrt *assert.Assertions) {
		// A task that never started has no meaningful exit code, so the phase distinguishes it.
		asrt.Equal(containers.ContainerInstancePhaseFailed, status.TypedSpec().Phase)
		asrt.Contains(status.TypedSpec().Error, "no such image")
		asrt.Zero(status.TypedSpec().ExitCode)
	})
}

func (suite *RuntimeSuite) TestDoesNotRestartTerminatedInstance() {
	suite.criUp()

	id := containers.InstanceID("once", 0)
	suite.runner.releaseID(id, 0)
	suite.createInstance("once")

	ctest.AssertResource(suite, id, func(status *containers.ContainerInstanceStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerInstancePhaseTerminated, status.TypedSpec().Phase)
	})

	// An instance is one execution. Restarting is the instance controller's job via a new
	// generation, so several reconciles must not re-run this one.
	for range 3 {
		spec, err := suite.State().Get(suite.Ctx(), containers.NewContainerInstanceSpec(containers.NamespaceName, id).Metadata())
		suite.Require().NoError(err)

		updated := spec.(*containers.ContainerInstanceSpec) //nolint:forcetypeassert,errcheck
		updated.TypedSpec().WorkingDir = "/srv"

		suite.Require().NoError(suite.State().Update(suite.Ctx(), updated))
	}

	ctest.AssertResource(suite, id, func(status *containers.ContainerInstanceStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerInstancePhaseTerminated, status.TypedSpec().Phase)
	})

	suite.Assert().Equal(1, suite.runner.startCount(id))
}

func (suite *RuntimeSuite) TestStopsOnTeardownAndReleasesFinalizer() {
	suite.criUp()

	id := containers.InstanceID("stopper", 0)
	suite.runner.blockOn(id)
	suite.createInstance("stopper")

	ctest.AssertResource(suite, id, func(status *containers.ContainerInstanceStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerInstancePhaseRunning, status.TypedSpec().Phase)
	})

	// The instance controller asks for the instance to go away.
	_, err := suite.State().Teardown(suite.Ctx(), containers.NewContainerInstanceSpec(containers.NamespaceName, id).Metadata())
	suite.Require().NoError(err)

	// The finalizer must be released only after the task is stopped and its runtime state removed.
	ctest.AssertResource(suite, id, func(spec *containers.ContainerInstanceSpec, asrt *assert.Assertions) {
		asrt.False(spec.Metadata().Finalizers().Has("containers.RuntimeController"))
	})

	suite.Assert().True(suite.runner.wasRemoved(id))
}

func (suite *RuntimeSuite) TestSweepsOrphansBeforeCreating() {
	// Containerd state outlives machined, so a leftover container of the same ID must be removed
	// before a new one is created, or the two would collide.
	suite.runner.mu.Lock()
	suite.runner.existing = []string{"leftover-0", "nginx-0"}
	suite.runner.mu.Unlock()

	suite.criUp()

	id := containers.InstanceID("nginx", 0)
	suite.runner.blockOn(id)
	suite.createInstance("nginx")

	ctest.AssertResource(suite, id, func(status *containers.ContainerInstanceStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerInstancePhaseRunning, status.TypedSpec().Phase)
	})

	// The orphan with no instance spec is gone.
	suite.Assert().True(suite.runner.wasRemoved("leftover-0"))
}

func (suite *RuntimeSuite) TestNothingRunsUntilCRIIsUp() {
	id := containers.InstanceID("waiting", 0)
	suite.runner.blockOn(id)
	suite.createInstance("waiting")

	// Without the CRI service there is no runtime to talk to, so nothing is started at all.
	ctest.AssertNoResource[*containers.ContainerInstanceStatus](suite, id)

	suite.Assert().Zero(suite.runner.startCount(id))
}
