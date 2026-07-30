// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/containers"
)

// MountController owns the volume mount holds for containers.
//
// Its one side effect is the block.VolumeMountRequest resources it creates and the finalizers it
// holds on the resulting block.VolumeMountStatus. Holding a finalizer is what stops a volume being
// unmounted from underneath a running container; releasing it too early would do exactly that, so
// the release is gated on no live instance remaining.
type MountController struct{}

// Name implements controller.Controller interface.
func (ctrl *MountController) Name() string {
	return "containers.MountController"
}

// Inputs implements controller.Controller interface.
func (ctrl *MountController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerSpecType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerInstanceStatusType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerLifecycleType,
			ID:        optional.Some(containers.ContainerLifecycleID),
			Kind:      controller.InputStrong,
		},
		{
			Namespace: block.NamespaceName,
			Type:      block.VolumeMountStatusType,
			Kind:      controller.InputStrong,
		},
		{
			// InputDestroyReady is what lets this controller tear down its own mount requests: it
			// wakes us when a request is tearing down with no finalizers left.
			Namespace: block.NamespaceName,
			Type:      block.VolumeMountRequestType,
			Kind:      controller.InputDestroyReady,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *MountController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: containers.ContainerMountStatusType,
			Kind: controller.OutputExclusive,
		},
		{
			// Shared: many controllers write mount requests.
			Type: block.VolumeMountRequestType,
			Kind: controller.OutputShared,
		},
	}
}

// Run implements controller.Controller interface.
func (ctrl *MountController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		}

		if err := ctrl.reconcile(ctx, r, logger); err != nil {
			logger.Error("failed to reconcile container mounts", zap.Error(err))

			return err
		}

		r.ResetRestartBackoff()
	}
}

// requesterFor builds the requester string for one container.
//
// Following the "service/<id>" convention the service runner already uses, so the owning container
// is recoverable from the request without parsing a composite ID. Container names cannot contain a
// slash, which makes the split unambiguous.
func (ctrl *MountController) requesterFor(containerID string) string {
	return ctrl.Name() + "/" + containerID
}

// containerFor recovers the container name from a requester string, if it belongs to us.
func (ctrl *MountController) containerFor(requester string) (string, bool) {
	prefix := ctrl.Name() + "/"

	if !strings.HasPrefix(requester, prefix) {
		return "", false
	}

	return strings.TrimPrefix(requester, prefix), true
}

// mountRequestID builds the ID of the mount request for one container and volume.
//
// It is per container rather than per volume so that two containers sharing a volume hold it
// independently: one stopping must not release the other's mount. The block subsystem aggregates
// requests by volume ID, so multiple requests for the same volume are expected.
func (ctrl *MountController) mountRequestID(containerID, volumeID string) string {
	return ctrl.requesterFor(containerID) + "-" + volumeID
}

//nolint:gocyclo,cyclop
func (ctrl *MountController) reconcile(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	specs, err := safe.ReaderListAll[*containers.ContainerSpec](ctx, r)
	if err != nil {
		return fmt.Errorf("failed to list container specs: %w", err)
	}

	instanceStatuses, err := safe.ReaderListAll[*containers.ContainerInstanceStatus](ctx, r)
	if err != nil {
		return fmt.Errorf("failed to list instance statuses: %w", err)
	}

	// A container with any instance that has not finished is still using its mounts.
	liveContainers := map[string]struct{}{}

	for status := range instanceStatuses.All() {
		if !status.TypedSpec().Phase.Done() {
			liveContainers[status.TypedSpec().ContainerID] = struct{}{}
		}
	}

	r.StartTrackingOutputs()

	// Track which mount requests are still wanted, so the rest can be released.
	wantedRequests := map[string]struct{}{}

	for spec := range specs.All() {
		containerID := spec.Metadata().ID()

		resolved, ready, mountErr, err := ctrl.reconcileContainer(ctx, r, logger, spec, wantedRequests)
		if err != nil {
			return err
		}

		var wasReady bool

		if err := safe.WriterModify(ctx, r,
			containers.NewContainerMountStatus(containers.NamespaceName, containerID),
			func(res *containers.ContainerMountStatus) error {
				wasReady = res.TypedSpec().Ready

				res.TypedSpec().Ready = ready
				res.TypedSpec().Mounts = resolved
				res.TypedSpec().Error = mountErr

				return nil
			},
		); err != nil {
			return fmt.Errorf("failed to write mount status %q: %w", containerID, err)
		}

		if ready != wasReady {
			if ready {
				logger.Info("container mounts are ready",
					zap.String("container", containerID),
					zap.Int("mounts", len(resolved)),
				)
			} else {
				logger.Warn("container mounts are not ready",
					zap.String("container", containerID),
					zap.String("reason", mountErr),
				)
			}
		}
	}

	if err := ctrl.releaseUnwanted(ctx, r, logger, wantedRequests, liveContainers); err != nil {
		return err
	}

	if err := safe.CleanupOutputs[*containers.ContainerMountStatus](ctx, r); err != nil {
		return fmt.Errorf("failed to clean up outputs: %w", err)
	}

	return reconcileLifecycle(ctx, r, logger, ctrl.Name(), len(wantedRequests) == 0)
}

// reconcileContainer ensures the mount requests for one container and resolves its mount list.
//
//nolint:gocyclo,cyclop
func (ctrl *MountController) reconcileContainer(
	ctx context.Context,
	r controller.Runtime,
	logger *zap.Logger,
	spec *containers.ContainerSpec,
	wantedRequests map[string]struct{},
) (resolved []containers.ResolvedMountSpec, ready bool, mountErr string, err error) {
	containerID := spec.Metadata().ID()
	ready = true

	for _, mount := range spec.TypedSpec().Mounts {
		if mount.Kind != containers.MountKindUserVolume {
			// tmpfs and hostPath need nothing from the block subsystem: the source is either
			// nothing at all or a path that must already exist.
			resolved = append(resolved, containers.ResolvedMountSpec{
				Kind:        mount.Kind,
				Source:      mount.Source,
				Destination: mount.Destination,
				Size:        mount.Size,
				Options:     mount.Options,
			})

			continue
		}

		requestID := ctrl.mountRequestID(containerID, mount.VolumeID)
		wantedRequests[requestID] = struct{}{}

		readOnly := !hasOption(mount.Options, "rw")

		if err = safe.WriterModify(ctx, r,
			block.NewVolumeMountRequest(block.NamespaceName, requestID),
			func(res *block.VolumeMountRequest) error {
				res.TypedSpec().VolumeID = mount.VolumeID
				res.TypedSpec().Requester = ctrl.requesterFor(containerID)
				res.TypedSpec().ReadOnly = readOnly
				// Detached must stay false: a detached mount is reachable only through a file
				// descriptor, so there would be no path to bind into the container.
				res.TypedSpec().Detached = false

				return nil
			},
		); err != nil {
			return nil, false, "", fmt.Errorf("failed to write mount request %q: %w", requestID, err)
		}

		mountStatus, getErr := safe.ReaderGetByID[*block.VolumeMountStatus](ctx, r, requestID)
		if getErr != nil {
			if !state.IsNotFoundError(getErr) {
				return nil, false, "", fmt.Errorf("failed to get mount status %q: %w", requestID, getErr)
			}

			ready = false
			mountErr = fmt.Sprintf("waiting for volume %q to be mounted", mount.VolumeID)

			logger.Debug("waiting for volume to be mounted",
				zap.String("container", containerID),
				zap.String("volume", mount.VolumeID),
			)

			continue
		}

		switch mountStatus.Metadata().Phase() {
		case resource.PhaseRunning:
			if !mountStatus.Metadata().Finalizers().Has(ctrl.Name()) {
				// Only ever add a finalizer while running: adding one to a tearing-down resource
				// would block that teardown forever.
				if err = r.AddFinalizer(ctx, mountStatus.Metadata(), ctrl.Name()); err != nil {
					return nil, false, "", fmt.Errorf("failed to add finalizer on %q: %w", requestID, err)
				}

				logger.Info("holding volume mount for container",
					zap.String("container", containerID),
					zap.String("volume", mount.VolumeID),
					zap.String("target", mountStatus.TypedSpec().Target),
					zap.Bool("readOnly", readOnly),
				)
			}

			resolved = append(resolved, containers.ResolvedMountSpec{
				Kind:        mount.Kind,
				Source:      mountStatus.TypedSpec().Target,
				Destination: mount.Destination,
				Options:     mount.Options,
			})
		case resource.PhaseTearingDown:
			// The volume is going away. Report not-ready so the instance controller stops the
			// container, and hold the finalizer until it has.
			ready = false
			mountErr = fmt.Sprintf("volume %q is being unmounted", mount.VolumeID)

			logger.Info("volume is tearing down, container must stop",
				zap.String("container", containerID),
				zap.String("volume", mount.VolumeID),
			)
		}
	}

	return resolved, ready, mountErr, nil
}

// releaseUnwanted releases mount requests no longer needed by any container.
func (ctrl *MountController) releaseUnwanted(
	ctx context.Context,
	r controller.Runtime,
	logger *zap.Logger,
	wantedRequests map[string]struct{},
	liveContainers map[string]struct{},
) error {
	requests, err := safe.ReaderListAll[*block.VolumeMountRequest](ctx, r)
	if err != nil {
		return fmt.Errorf("failed to list mount requests: %w", err)
	}

	for request := range requests.All() {
		// Only touch requests this controller owns.
		containerID, ours := ctrl.containerFor(request.TypedSpec().Requester)
		if !ours {
			continue
		}

		requestID := request.Metadata().ID()

		if _, wanted := wantedRequests[requestID]; wanted {
			continue
		}

		// A container with a live instance still needs its mount, even if the spec no longer lists
		// it: the running task may still have the path open.
		if _, live := liveContainers[containerID]; live {
			logger.Debug("deferring volume mount release until the container stops",
				zap.String("container", containerID),
				zap.String("request", requestID),
			)

			continue
		}

		if err := ctrl.releaseRequest(ctx, r, logger, requestID); err != nil {
			return err
		}
	}

	return nil
}

// releaseRequest drops the finalizer on the mount status and destroys the request.
//
//nolint:gocyclo
func (ctrl *MountController) releaseRequest(ctx context.Context, r controller.Runtime, logger *zap.Logger, requestID string) error {
	mountStatus, err := safe.ReaderGetByID[*block.VolumeMountStatus](ctx, r, requestID)
	if err != nil && !state.IsNotFoundError(err) {
		return fmt.Errorf("failed to get mount status %q: %w", requestID, err)
	}

	if mountStatus != nil && mountStatus.Metadata().Finalizers().Has(ctrl.Name()) {
		if err := r.RemoveFinalizer(ctx, mountStatus.Metadata(), ctrl.Name()); err != nil {
			return fmt.Errorf("failed to remove finalizer on %q: %w", requestID, err)
		}

		logger.Info("released volume mount", zap.String("request", requestID))
	}

	requestMD := block.NewVolumeMountRequest(block.NamespaceName, requestID).Metadata()

	okToDestroy, err := r.Teardown(ctx, requestMD)
	if err != nil {
		if state.IsNotFoundError(err) {
			return nil
		}

		return fmt.Errorf("failed to tear down mount request %q: %w", requestID, err)
	}

	if !okToDestroy {
		logger.Debug("waiting for the volume mount request to be released", zap.String("request", requestID))

		return nil
	}

	if err := r.Destroy(ctx, requestMD); err != nil && !state.IsNotFoundError(err) {
		return fmt.Errorf("failed to destroy mount request %q: %w", requestID, err)
	}

	logger.Debug("destroyed volume mount request", zap.String("request", requestID))

	return nil
}

func hasOption(options []string, option string) bool {
	return slices.Contains(options, option)
}
