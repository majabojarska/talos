// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers

import (
	"context"
	"fmt"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/pkg/machinery/resources/containers"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
	timeres "github.com/siderolabs/talos/pkg/machinery/resources/time"
	"github.com/siderolabs/talos/pkg/machinery/resources/v1alpha1"
)

// StatusController aggregates the per-stage statuses into the user-facing ContainerStatus.
//
// Pure projection: it owns nothing and decides nothing. Its job is to give an operator one resource
// to look at, and to keep the coarse health value stable even as the internal states change.
type StatusController struct{}

// Name implements controller.Controller interface.
func (ctrl *StatusController) Name() string {
	return "containers.StatusController"
}

// Inputs implements controller.Controller interface.
func (ctrl *StatusController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerSpecType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerImageStatusType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerMountStatusType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerInstanceStatusType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: network.NamespaceName,
			Type:      network.StatusType,
			ID:        optional.Some(network.StatusID),
			Kind:      controller.InputWeak,
		},
		{
			Namespace: v1alpha1.NamespaceName,
			Type:      timeres.StatusType,
			ID:        optional.Some(timeres.StatusID),
			Kind:      controller.InputWeak,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *StatusController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: containers.ContainerStatusType,
			Kind: controller.OutputExclusive,
		},
	}
}

// Run implements controller.Controller interface.
func (ctrl *StatusController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		}

		if err := ctrl.reconcile(ctx, r, logger); err != nil {
			logger.Error("failed to aggregate container statuses", zap.Error(err))

			return err
		}

		r.ResetRestartBackoff()
	}
}

//nolint:gocyclo,cyclop
func (ctrl *StatusController) reconcile(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	r.StartTrackingOutputs()

	specs, err := safe.ReaderListAll[*containers.ContainerSpec](ctx, r)
	if err != nil {
		return fmt.Errorf("failed to list container specs: %w", err)
	}

	instanceStatuses, err := safe.ReaderListAll[*containers.ContainerInstanceStatus](ctx, r)
	if err != nil {
		return fmt.Errorf("failed to list instance statuses: %w", err)
	}

	// Keep only the newest instance status per container: ContainerStatus reflects the current
	// execution, while the per-execution history stays on ContainerInstanceStatus.
	newest := map[string]*containers.ContainerInstanceStatus{}

	for status := range instanceStatuses.All() {
		containerID := status.TypedSpec().ContainerID

		if existing, ok := newest[containerID]; !ok || status.TypedSpec().Generation > existing.TypedSpec().Generation {
			newest[containerID] = status
		}
	}

	gates, err := loadGates(ctx, r)
	if err != nil {
		return err
	}

	for spec := range specs.All() {
		containerID := spec.Metadata().ID()

		var before, after containers.ContainerStatusSpec

		if err := safe.WriterModify(ctx, r,
			containers.NewContainerStatus(containers.NamespaceName, containerID),
			func(res *containers.ContainerStatus) error {
				before = *res.TypedSpec()

				if err := ctrl.project(ctx, r, res.TypedSpec(), spec, newest[containerID], gates); err != nil {
					return err
				}

				after = *res.TypedSpec()

				return nil
			},
		); err != nil {
			return fmt.Errorf("failed to write container status %q: %w", containerID, err)
		}

		// This is the one place with a before-and-after view of the aggregate, so it is where a
		// state change is worth a line in the log rather than in every controller that causes one.
		logTransition(logger, containerID, before, after)
	}

	if err := safe.CleanupOutputs[*containers.ContainerStatus](ctx, r); err != nil {
		return fmt.Errorf("failed to clean up outputs: %w", err)
	}

	return nil
}

// logTransition reports a change in the aggregated state or error of one container.
func logTransition(logger *zap.Logger, containerID string, before, after containers.ContainerStatusSpec) {
	if before.State != after.State {
		fields := []zap.Field{
			zap.String("container", containerID),
			zap.Stringer("from", before.State),
			zap.Stringer("to", after.State),
			zap.Stringer("health", after.Health),
		}

		if len(after.WaitingFor) > 0 {
			fields = append(fields, zap.Strings("waitingFor", after.WaitingFor))
		}

		if after.PID != 0 {
			fields = append(fields, zap.Uint32("pid", after.PID))
		}

		if after.Error != "" {
			fields = append(fields, zap.String("error", after.Error))
		}

		logger.Info("container state changed", fields...)

		return
	}

	// A container that stays in the same state but picks up a new error has still made news.
	if before.Error != after.Error && after.Error != "" {
		logger.Warn("container reported an error",
			zap.String("container", containerID),
			zap.Stringer("state", after.State),
			zap.String("error", after.Error),
		)
	}
}

// project derives the aggregated status for one container.
//
//nolint:gocyclo,cyclop
func (ctrl *StatusController) project(
	ctx context.Context,
	r controller.Runtime,
	status *containers.ContainerStatusSpec,
	spec *containers.ContainerSpec,
	instance *containers.ContainerInstanceStatus,
	gates gateInputs,
) error {
	imageStatus, err := safe.ReaderGetByID[*containers.ContainerImageStatus](ctx, r, spec.Metadata().ID())
	if err != nil && !state.IsNotFoundError(err) {
		return fmt.Errorf("failed to get image status: %w", err)
	}

	// Report the resolved digest once it is known, so the operator sees what is actually running
	// rather than what was requested.
	status.Image = spec.TypedSpec().Image

	if imageStatus != nil && imageStatus.TypedSpec().Digest != "" {
		status.Image = imageStatus.TypedSpec().Digest
	}

	status.PID = 0
	status.ExitCode = 0
	status.Error = ""
	status.WaitingFor = nil
	status.RestartCount = 0

	if instance != nil {
		status.RestartCount = instance.TypedSpec().Generation
		status.ExitCode = instance.TypedSpec().ExitCode

		if instance.TypedSpec().Phase == containers.ContainerInstancePhaseRunning {
			status.PID = instance.TypedSpec().PID
		}
	}

	ready, waitingFor, _ := evaluateGates(ctx, r, spec, gates)

	status.State = deriveState(instance, imageStatus, ready)
	status.Health = status.State.Health()

	if status.State == containers.ContainerStatePending {
		status.WaitingFor = waitingFor
	}

	// Error precedence: the stage that actually blocked progress wins. An instance failure is the
	// most specific, then mounts, then the image.
	switch {
	case instance != nil && instance.TypedSpec().Error != "":
		status.Error = instance.TypedSpec().Error
	default:
		mountStatus, mountErr := safe.ReaderGetByID[*containers.ContainerMountStatus](ctx, r, spec.Metadata().ID())
		if mountErr != nil && !state.IsNotFoundError(mountErr) {
			return fmt.Errorf("failed to get mount status: %w", mountErr)
		}

		switch {
		case mountStatus != nil && mountStatus.TypedSpec().Error != "":
			status.Error = mountStatus.TypedSpec().Error
		case imageStatus != nil && imageStatus.TypedSpec().Error != "":
			status.Error = imageStatus.TypedSpec().Error
		}
	}

	return nil
}

// deriveState maps the observable resources onto a container state.
//
// There is no terminal state: a finished instance means a restart is pending, which is backoff.
func deriveState(
	instance *containers.ContainerInstanceStatus,
	imageStatus *containers.ContainerImageStatus,
	gatesReady bool,
) containers.ContainerState {
	if instance != nil {
		switch instance.TypedSpec().Phase {
		case containers.ContainerInstancePhaseCreated:
			return containers.ContainerStateStarting
		case containers.ContainerInstancePhaseRunning:
			return containers.ContainerStateRunning
		case containers.ContainerInstancePhaseTerminated, containers.ContainerInstancePhaseFailed:
			return containers.ContainerStateBackoff
		}
	}

	// No instance yet: the state is whichever gate is still closed.
	if imageStatus != nil {
		switch imageStatus.TypedSpec().Phase {
		case containers.ContainerImagePhasePulling:
			return containers.ContainerStatePulling
		case containers.ContainerImagePhaseFailed:
			// A failed pull is retried, so it is a form of backoff rather than a terminal state.
			return containers.ContainerStateBackoff
		case containers.ContainerImagePhasePending, containers.ContainerImagePhaseReady:
		}
	}

	if !gatesReady {
		return containers.ContainerStatePending
	}

	// Everything is satisfied; the instance controller is about to create an instance.
	return containers.ContainerStateStarting
}
