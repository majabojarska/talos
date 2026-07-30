// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/channel"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/internal/app/machined/pkg/runtime"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/containers"
	"github.com/siderolabs/talos/pkg/machinery/resources/v1alpha1"
)

// TaskRunner runs one container execution against a container runtime.
//
// Everything platform-specific lives behind this interface: the containerd client, the cgroup, the
// OCI spec, the signal sequence on teardown. The controller above it only orchestrates. That split
// is also what makes the controller testable without containerd.
type TaskRunner interface {
	// List returns the IDs of containers currently present in the namespace.
	//
	// Used for the orphan sweep: containerd's state is persistent, so containers can outlive the
	// process that created them.
	List(ctx context.Context) ([]string, error)

	// Remove deletes a container along with its snapshot, tolerating absence.
	Remove(ctx context.Context, id string) error

	// Run creates the container and blocks until its task exits.
	//
	// started is called once with the task PID. Canceling ctx must stop the task gracefully
	// (SIGTERM, grace period, SIGKILL) and clean up everything it created, using a context that
	// outlives the cancellation.
	Run(ctx context.Context, id string, spec containers.ContainerInstanceSpecSpec, started func(pid uint32)) (exitCode int32, err error)

	// Close releases the underlying client.
	Close() error
}

// RuntimeController runs container instances.
//
// It is the only controller that touches a process, and it holds one goroutine per running instance
// for the duration of the task. That much is irreducible: a live task is a process with an open wait
// stream, not a piece of data.
//
// It is also the only component that talks to the container runtime about containers, which is what
// lets the orphan sweep and the create path be ordered against each other inside a single reconcile.
type RuntimeController struct {
	// Runtime provides the logging manager for container logs.
	Runtime runtime.Runtime

	// RunnerProvider is overridable for testing.
	RunnerProvider func() (TaskRunner, error)

	instances map[string]*instanceRunState
	swept     bool
}

// Name implements controller.Controller interface.
func (ctrl *RuntimeController) Name() string {
	return "containers.RuntimeController"
}

// Inputs implements controller.Controller interface.
func (ctrl *RuntimeController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerInstanceSpecType,
			Kind:      controller.InputStrong,
		},
		{
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerLifecycleType,
			ID:        optional.Some(containers.ContainerLifecycleID),
			Kind:      controller.InputStrong,
		},
		{
			Namespace: v1alpha1.NamespaceName,
			Type:      v1alpha1.ServiceType,
			ID:        optional.Some(criServiceID),
			Kind:      controller.InputWeak,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *RuntimeController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: containers.ContainerInstanceStatusType,
			Kind: controller.OutputExclusive,
		},
	}
}

// instanceRunState tracks one running execution.
type instanceRunState struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu         sync.Mutex
	phase      containers.ContainerInstancePhase
	pid        uint32
	exitCode   int32
	err        error
	startedAt  time.Time
	finishedAt time.Time
}

func (s *instanceRunState) snapshot() containers.ContainerInstanceStatusSpec {
	s.mu.Lock()
	defer s.mu.Unlock()

	spec := containers.ContainerInstanceStatusSpec{
		Phase:      s.phase,
		PID:        s.pid,
		ExitCode:   s.exitCode,
		StartedAt:  s.startedAt,
		FinishedAt: s.finishedAt,
	}

	if s.err != nil {
		spec.Error = s.err.Error()
	}

	return spec
}

func (s *instanceRunState) setStarted(pid uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.phase = containers.ContainerInstancePhaseRunning
	s.pid = pid
	s.startedAt = time.Now()
}

func (s *instanceRunState) setFinished(exitCode int32, err error, everStarted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// A task that never started is a setup failure, which is a different thing from a task that ran
	// and exited: the exit code is meaningless in the first case.
	if everStarted {
		s.phase = containers.ContainerInstancePhaseTerminated
		s.exitCode = exitCode
	} else {
		s.phase = containers.ContainerInstancePhaseFailed
	}

	s.pid = 0
	s.err = err
	s.finishedAt = time.Now()
}

func (s *instanceRunState) stop() {
	s.cancel()
	s.wg.Wait()
}

// Run implements controller.Controller interface.
//
//nolint:gocyclo,cyclop
func (ctrl *RuntimeController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	if ctrl.RunnerProvider == nil {
		ctrl.RunnerProvider = ctrl.defaultRunnerProvider
	}

	ctrl.instances = map[string]*instanceRunState{}

	notifyCh := make(chan struct{}, 1)

	var runner TaskRunner

	defer func() {
		// Stop every task before releasing the client, so nothing is left running with no owner.
		for _, instance := range ctrl.instances {
			instance.stop()
		}

		if runner != nil {
			runner.Close() //nolint:errcheck
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		case <-notifyCh:
		}

		criUp, err := ctrl.criIsUp(ctx, r)
		if err != nil {
			return err
		}

		if criUp && runner == nil {
			if runner, err = ctrl.RunnerProvider(); err != nil {
				logger.Error("failed to create the container task runner", zap.Error(err))

				return fmt.Errorf("failed to create task runner: %w", err)
			}

			logger.Info("connected to the container runtime, containers can now be started")
		}

		if runner == nil {
			logger.Debug("waiting for the container runtime")
		} else if err := ctrl.reconcile(ctx, r, logger, runner, notifyCh); err != nil {
			logger.Error("failed to reconcile container instances", zap.Error(err))

			return err
		}

		r.ResetRestartBackoff()
	}
}

func (ctrl *RuntimeController) criIsUp(ctx context.Context, r controller.Runtime) (bool, error) {
	service, err := safe.ReaderGetByID[*v1alpha1.Service](ctx, r, criServiceID)
	if err != nil {
		if state.IsNotFoundError(err) {
			return false, nil
		}

		return false, fmt.Errorf("failed to get %q service: %w", criServiceID, err)
	}

	return service.TypedSpec().Running && service.TypedSpec().Healthy, nil
}

//nolint:gocyclo,cyclop
func (ctrl *RuntimeController) reconcile(
	ctx context.Context,
	r controller.Runtime,
	logger *zap.Logger,
	runner TaskRunner,
	notifyCh chan struct{},
) error {
	specs, err := safe.ReaderListAll[*containers.ContainerInstanceSpec](ctx, r)
	if err != nil {
		return fmt.Errorf("failed to list instance specs: %w", err)
	}

	// Sweep before creating anything. Instance resources are in-memory and gone after a machined
	// restart, but the container runtime's are not, so generations restart from zero and a leftover
	// container would collide with a new one of the same ID. Deleting first makes that a non-event,
	// and it only works because one controller owns both halves.
	if !ctrl.swept {
		if err := ctrl.sweepOrphans(ctx, logger, runner, specs); err != nil {
			return err
		}

		ctrl.swept = true
	}

	r.StartTrackingOutputs()

	live := map[string]struct{}{}

	for spec := range specs.All() {
		id := spec.Metadata().ID()

		switch spec.Metadata().Phase() {
		case resource.PhaseRunning:
			live[id] = struct{}{}

			if !spec.Metadata().Finalizers().Has(ctrl.Name()) {
				// The finalizer is the handshake with the instance controller: it will not destroy
				// this instance until the task is stopped and cleaned up.
				if err := r.AddFinalizer(ctx, spec.Metadata(), ctrl.Name()); err != nil {
					return fmt.Errorf("failed to add finalizer on %q: %w", id, err)
				}
			}

			instance, exists := ctrl.instances[id]
			if !exists {
				instance = ctrl.start(ctx, logger, runner, spec, notifyCh)
				ctrl.instances[id] = instance
			}

			if err := ctrl.writeStatus(ctx, r, spec, instance); err != nil {
				return err
			}
		case resource.PhaseTearingDown:
			instance, exists := ctrl.instances[id]
			if exists {
				logger.Info("stopping container instance", zap.String("instance", id))

				// Stopping is synchronous: the task must be gone, and its runtime state cleaned up,
				// before the instance controller is allowed to destroy the resource.
				instance.stop()
				delete(ctrl.instances, id)

				logger.Info("container instance stopped", zap.String("instance", id))
			}

			if err := runner.Remove(ctx, id); err != nil {
				return fmt.Errorf("failed to remove container %q: %w", id, err)
			}

			if spec.Metadata().Finalizers().Has(ctrl.Name()) {
				if err := r.RemoveFinalizer(ctx, spec.Metadata(), ctrl.Name()); err != nil {
					return fmt.Errorf("failed to remove finalizer on %q: %w", id, err)
				}

				logger.Debug("released the container instance for destruction", zap.String("instance", id))
			}
		}
	}

	// Any goroutine whose spec vanished outright.
	for id, instance := range ctrl.instances {
		if _, exists := live[id]; exists {
			continue
		}

		logger.Info("instance spec is gone, stopping the container", zap.String("instance", id))

		instance.stop()
		delete(ctrl.instances, id)
	}

	if err := safe.CleanupOutputs[*containers.ContainerInstanceStatus](ctx, r); err != nil {
		return fmt.Errorf("failed to clean up outputs: %w", err)
	}

	return reconcileLifecycle(ctx, r, logger, ctrl.Name(), len(ctrl.instances) == 0)
}

// sweepOrphans removes containers with no corresponding instance spec.
func (ctrl *RuntimeController) sweepOrphans(
	ctx context.Context,
	logger *zap.Logger,
	runner TaskRunner,
	specs safe.List[*containers.ContainerInstanceSpec],
) error {
	existing, err := runner.List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	wanted := map[string]struct{}{}

	for spec := range specs.All() {
		wanted[spec.Metadata().ID()] = struct{}{}
	}

	for _, id := range existing {
		if _, exists := wanted[id]; exists {
			continue
		}

		logger.Info("removing orphaned container", zap.String("container", id))

		if err := runner.Remove(ctx, id); err != nil {
			return fmt.Errorf("failed to remove orphaned container %q: %w", id, err)
		}
	}

	return nil
}

// start launches the goroutine that runs one instance to completion.
func (ctrl *RuntimeController) start(
	ctx context.Context,
	logger *zap.Logger,
	runner TaskRunner,
	spec *containers.ContainerInstanceSpec,
	notifyCh chan struct{},
) *instanceRunState {
	id := spec.Metadata().ID()

	instance := &instanceRunState{
		phase: containers.ContainerInstancePhaseCreated,
	}

	// Deliberately derived from the controller context: canceling it is how the task is stopped.
	// The runner is responsible for using a context that outlives the cancellation for its own
	// teardown, or the stop sequence would be canceled before it could run.
	runCtx, cancel := context.WithCancel(ctx)
	instance.cancel = cancel

	instanceSpec := *spec.TypedSpec()

	instance.wg.Go(func() {
		var everStarted bool

		defer func() {
			if p := recover(); p != nil {
				// One bad container must not take down machined.
				instance.setFinished(0, fmt.Errorf("panic: %v", p), everStarted)

				logger.Error("container run panicked", zap.Stack("stack"), zap.String("instance", id))
			}

			// Wake the controller so the terminal status is published even if nothing else changes.
			channel.SendWithContext(context.WithoutCancel(runCtx), notifyCh, struct{}{})
		}()

		logger.Info("starting container",
			zap.String("instance", id),
			zap.String("image", instanceSpec.Image),
			// Where to look for the output; the buffer outlives the container, so this stays valid
			// after it exits.
			zap.String("logs", constants.TalosContainersLogPrefix+instanceSpec.ContainerID),
		)

		exitCode, err := runner.Run(runCtx, id, instanceSpec, func(pid uint32) {
			everStarted = true

			instance.setStarted(pid)

			logger.Info("container started", zap.String("instance", id), zap.Uint32("pid", pid))

			channel.SendWithContext(runCtx, notifyCh, struct{}{})
		})

		instance.setFinished(exitCode, err, everStarted)

		switch {
		case err != nil:
			logger.Error("container run failed", zap.String("instance", id), zap.Error(err))
		case exitCode != 0:
			logger.Warn("container exited non-zero", zap.String("instance", id), zap.Int32("exitCode", exitCode))
		default:
			logger.Info("container exited", zap.String("instance", id))
		}
	})

	return instance
}

func (ctrl *RuntimeController) writeStatus(
	ctx context.Context,
	r controller.Runtime,
	spec *containers.ContainerInstanceSpec,
	instance *instanceRunState,
) error {
	snapshot := instance.snapshot()

	if err := safe.WriterModify(ctx, r,
		containers.NewContainerInstanceStatus(containers.NamespaceName, spec.Metadata().ID()),
		func(res *containers.ContainerInstanceStatus) error {
			status := res.TypedSpec()

			status.ContainerID = spec.TypedSpec().ContainerID
			status.Generation = spec.TypedSpec().Generation
			status.Phase = snapshot.Phase
			status.PID = snapshot.PID
			status.ExitCode = snapshot.ExitCode
			status.Error = snapshot.Error
			status.StartedAt = snapshot.StartedAt
			status.FinishedAt = snapshot.FinishedAt

			return nil
		},
	); err != nil {
		return fmt.Errorf("failed to write instance status %q: %w", spec.Metadata().ID(), err)
	}

	return nil
}

func (ctrl *RuntimeController) defaultRunnerProvider() (TaskRunner, error) {
	return newContainerdRunner(ctrl.Runtime.Logging())
}
