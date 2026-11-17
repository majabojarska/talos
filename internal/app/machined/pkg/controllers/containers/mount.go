// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers

import (
	"context"
	"fmt"
	"strings"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"

	configcfg "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/containers"
)

// containerLabel carries the owning container on the mount requests this controller creates.
//
// The request ID is not parsed for it: a request that is no longer wanted still has to be attributed
// to a container to decide whether releasing it is safe.
const containerLabel = "container"

// MountController resolves a container's mounts to host paths, and holds the volumes they need.
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
		// Volume names are resolved to volume IDs against the volume documents in the machine config.
		{
			Namespace: config.NamespaceName,
			Type:      config.MachineConfigType,
			ID:        optional.Some(config.ActiveID),
			Kind:      controller.InputWeak,
		},
		// Both instance types are read to decide whether a container is still using its mounts: a
		// status alone leaves a hole between an instance being created and its first status being
		// written, during which a starting container looks idle.
		{
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerInstanceSpecType,
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
			Namespace: block.NamespaceName,
			Type:      block.VolumeStatusType,
			Kind:      controller.InputStrong,
		},
		{
			Namespace: block.NamespaceName,
			Type:      block.TextVolumeStatusType,
			Kind:      controller.InputWeak,
		},
		{
			// InputDestroyReady is what lets this controller tear down its own mount requests: it
			// wakes us when one is tearing down with no finalizers left.
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
		{
			// Shared: text volumes serve other consumers as well.
			Type: block.TextVolumeRequestType,
			Kind: controller.OutputShared,
		},
	}
}

// Run implements controller.Controller interface.
func (ctrl *MountController) Run(ctx context.Context, runtime controller.Runtime, logger *zap.Logger) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-runtime.EventCh():
		}

		if err := ctrl.reconcile(ctx, runtime, logger); err != nil {
			logger.Error("failed to reconcile container mounts", zap.Error(err))

			return err
		}

		runtime.ResetRestartBackoff()
	}
}

// mountRequestID builds the ID of the mount request for one container and volume.
//
// Per container rather than per volume, so two containers sharing a volume hold it independently and
// one stopping does not release the other's mount. Slash-separated because container and volume names
// both contain hyphens, which would make a hyphen-joined ID ambiguous and let two containers collide
// on one request.
func (ctrl *MountController) mountRequestID(containerSpecID, volumeID string) string {
	return ctrl.Name() + "/" + containerSpecID + "/" + volumeID
}

//nolint:gocyclo
func (ctrl *MountController) reconcile(ctx context.Context, runtime controller.Runtime, logger *zap.Logger) error {
	containerLifecycle, err := readContainerLifecycle(ctx, runtime)
	if err != nil {
		return err
	}

	// The barrier tearing down, or being gone, is the node on its way down. Everything is released
	// here and nothing is requested again. Releasing without waiting for containers to stop is safe
	// because it does not unmount anything by itself: the stopContainers phase runs before the
	// unmount phases, so the volumes are still mounted for as long as the containers need them.
	if containerLifecycle == nil || containerLifecycle.Metadata().Phase() == resource.PhaseTearingDown {
		return ctrl.releaseForShutdown(ctx, runtime, logger, containerLifecycle)
	}

	containerSpecs, err := safe.ReaderListAll[*containers.ContainerSpec](ctx, runtime)
	if err != nil {
		return fmt.Errorf("failed to list container specs: %w", err)
	}

	live, err := ctrl.liveContainers(ctx, runtime)
	if err != nil {
		return err
	}

	machineConfig, err := safe.ReaderGetByID[*config.MachineConfig](ctx, runtime, config.ActiveID)
	if err != nil && !state.IsNotFoundError(err) {
		return fmt.Errorf("failed to get machine config: %w", err)
	}

	var cfg configcfg.Config

	if machineConfig != nil {
		cfg = machineConfig.Config()
	}

	users, err := ctrl.deviceUsers(ctx, runtime)
	if err != nil {
		return err
	}

	runtime.StartTrackingOutputs()

	wanted := map[string]struct{}{}
	wantedTextVolumes := map[string]struct{}{}
	wantedHolds := map[string]map[string]struct{}{}

	for containerSpec := range containerSpecs.All() {
		containerSpecID := containerSpec.Metadata().ID()

		_, isLive := live[containerSpecID]

		resolvedMounts, mountsReady, reason, err := ctrl.reconcileContainer(ctx, runtime, logger, cfg, containerSpec, isLive, wanted, wantedTextVolumes)
		if err != nil {
			return err
		}

		resolvedDevices, devicesReady, deviceReasons, err := ctrl.reconcileContainerDevices(ctx, runtime, logger, containerSpec, users, wantedHolds)
		if err != nil {
			return err
		}

		reason = joinReasons(reason, deviceReasons)

		if err := safe.WriterModify(ctx, runtime,
			containers.NewContainerMountStatus(containers.NamespaceName, containerSpecID),
			func(res *containers.ContainerMountStatus) error {
				res.TypedSpec().Ready = mountsReady && devicesReady
				res.TypedSpec().Mounts = resolvedMounts
				res.TypedSpec().Devices = resolvedDevices
				res.TypedSpec().Error = reason

				return nil
			},
		); err != nil {
			return fmt.Errorf("failed to write mount status %q: %w", containerSpecID, err)
		}
	}

	if err := ctrl.releaseUnwanted(ctx, runtime, logger, wanted, live); err != nil {
		return err
	}

	if _, err := ctrl.releaseUnwantedTextVolumes(ctx, runtime, logger, wantedTextVolumes, live); err != nil {
		return err
	}

	// Before the devices are released, so that a container which is gone no longer has a status
	// reporting its devices as resolved.
	if err := safe.CleanupOutputs[*containers.ContainerMountStatus](ctx, runtime); err != nil {
		return fmt.Errorf("failed to clean up outputs: %w", err)
	}

	if err := ctrl.releaseUnwantedDevices(ctx, runtime, logger, wantedHolds); err != nil {
		return err
	}

	return reconcileLifecycle(ctx, runtime, logger, containerLifecycle, ctrl.Name(), len(wanted)+len(wantedTextVolumes) == 0)
}

// releaseForShutdown releases everything this controller holds, and lets the shutdown barrier go once
// nothing is held any more.
func (ctrl *MountController) releaseForShutdown(
	ctx context.Context,
	runtime controller.Runtime,
	logger *zap.Logger,
	containerLifecycle *containers.ContainerLifecycle,
) error {
	held, err := ctrl.releaseAll(ctx, runtime, logger)
	if err != nil {
		return err
	}

	heldTextVolumes, err := ctrl.releaseUnwantedTextVolumes(ctx, runtime, logger, nil, nil)
	if err != nil {
		return err
	}

	heldDevices, err := ctrl.releaseAllDevices(ctx, runtime, logger, containerLifecycle)
	if err != nil {
		return err
	}

	return reconcileLifecycle(ctx, runtime, logger, containerLifecycle, ctrl.Name(), held+heldTextVolumes+heldDevices == 0)
}

// joinReasons appends the device reasons to the mount reason.
func joinReasons(reason string, deviceReasons []string) string {
	if reason != "" {
		deviceReasons = append([]string{reason}, deviceReasons...)
	}

	return strings.Join(deviceReasons, "; ")
}

// liveContainers returns the containers which may still be using their mounts.
func (ctrl *MountController) liveContainers(ctx context.Context, reader controller.Reader) (map[string]struct{}, error) {
	live := map[string]struct{}{}

	containerInstanceStatuses, err := safe.ReaderListAll[*containers.ContainerInstanceStatus](ctx, reader)
	if err != nil {
		return nil, fmt.Errorf("failed to list instance statuses: %w", err)
	}

	done := map[string]struct{}{}

	for containerInstanceStatus := range containerInstanceStatuses.All() {
		if containerInstanceStatus.TypedSpec().Phase.Done() {
			done[containerInstanceStatus.Metadata().ID()] = struct{}{}
		} else {
			live[containerInstanceStatus.TypedSpec().ContainerID] = struct{}{}
		}
	}

	containerInstanceSpecs, err := safe.ReaderListAll[*containers.ContainerInstanceSpec](ctx, reader)
	if err != nil {
		return nil, fmt.Errorf("failed to list instance specs: %w", err)
	}

	for containerInstanceSpec := range containerInstanceSpecs.All() {
		if _, isDone := done[containerInstanceSpec.Metadata().ID()]; isDone {
			continue
		}

		live[containerInstanceSpec.TypedSpec().ContainerID] = struct{}{}
	}

	return live, nil
}

// reconcileContainer requests the mounts one container needs and resolves them to host paths.
//
// isLive reports whether a task of the container may be using its mounts, which decides whether the
// hold on a volume being unmounted has to be kept.
//
//nolint:gocyclo,cyclop
func (ctrl *MountController) reconcileContainer(
	ctx context.Context,
	runtime controller.Runtime,
	logger *zap.Logger,
	cfg configcfg.Config,
	containerSpec *containers.ContainerSpec,
	isLive bool,
	wanted map[string]struct{},
	wantedTextVolumes map[string]struct{},
) (resolvedMounts []containers.ResolvedMountSpec, ready bool, reason string, err error) {
	containerSpecID := containerSpec.Metadata().ID()
	ready = true

	var reasons []string

	for _, containerMountSpec := range containerSpec.TypedSpec().Mounts {
		if containerMountSpec.Kind != containers.MountKindVolume && containerMountSpec.Kind != containers.MountKindTextVolume {
			// tmpfs and hostPath need nothing from the block subsystem: the source is either nothing
			// at all or a path which must already exist.
			resolvedMounts = append(resolvedMounts, containers.ResolvedMountSpec{
				Kind:        containerMountSpec.Kind,
				Source:      containerMountSpec.Source,
				Destination: containerMountSpec.Destination,
				Size:        containerMountSpec.Size,
				Options:     containerMountSpec.Options,
			})

			continue
		}

		volumeName := containerMountSpec.VolumeName

		var (
			backing       configcfg.BackingVolume
			pendingReason string
		)

		backing, pendingReason, err = ctrl.resolveVolume(ctx, runtime, cfg, containerSpecID, containerMountSpec, wantedTextVolumes)
		if err != nil {
			return nil, false, "", err
		}

		if pendingReason != "" {
			ready = false

			reasons = append(reasons, pendingReason)

			continue
		}

		volumeID := backing.ID
		requestID := ctrl.mountRequestID(containerSpecID, volumeID)
		wanted[requestID] = struct{}{}

		// Writable unless the options say otherwise; the writable default is already applied by
		// ConfigController.
		readOnly := containerMountSpec.ReadOnly()

		if err = safe.WriterModify(ctx, runtime,
			block.NewVolumeMountRequest(block.NamespaceName, requestID),
			func(res *block.VolumeMountRequest) error {
				res.Metadata().Labels().Set(containerLabel, containerSpecID)

				res.TypedSpec().VolumeID = volumeID
				res.TypedSpec().Requester = ctrl.Name()
				res.TypedSpec().ReadOnly = readOnly
				// Detached must stay false: a detached mount is reachable only through a file
				// descriptor and never appears at its target, so there would be no path to bind.
				res.TypedSpec().Detached = false
				// The volume's own security, as its own mount request asks for: requests are merged, so
				// asking for less would weaken the mount whenever this request is the last one holding
				// it, such as after the volume config is removed or swapped while the container runs.
				res.TypedSpec().Secure = backing.Secure
				res.TypedSpec().NoExec = backing.Secure

				return nil
			},
		); err != nil {
			return nil, false, "", fmt.Errorf("failed to write mount request %q: %w", requestID, err)
		}

		volumeMountStatus, getErr := safe.ReaderGetByID[*block.VolumeMountStatus](ctx, runtime, requestID)
		if getErr != nil {
			if !state.IsNotFoundError(getErr) {
				return nil, false, "", fmt.Errorf("failed to get mount status %q: %w", requestID, getErr)
			}

			ready = false

			reasons = append(reasons, fmt.Sprintf("waiting for volume %q to be mounted", volumeName))

			continue
		}

		if volumeMountStatus.Metadata().Phase() != resource.PhaseRunning {
			ready = false

			if !volumeMountStatus.Metadata().Finalizers().Has(ctrl.Name()) {
				reasons = append(reasons, fmt.Sprintf("volume %q is being unmounted", volumeName))

				continue
			}

			// isLive was read at the start of the pass, so an instance started since would be missed.
			// Look again before giving up the hold, as that is what lets the volume be unmounted.
			if !isLive {
				live, liveErr := ctrl.liveContainers(ctx, runtime)
				if liveErr != nil {
					return nil, false, "", liveErr
				}

				_, isLive = live[containerSpecID]
			}

			if isLive {
				// The running container comes first: the volume stays mounted as it is, and the
				// unmount (and any remount with new parameters) waits until the container stops.
				reasons = append(reasons, fmt.Sprintf("volume %q is pending unmount, waiting for the container to stop", volumeName))

				continue
			}

			if err = ctrl.releaseFinalizer(ctx, runtime, logger, requestID); err != nil {
				return nil, false, "", err
			}

			reasons = append(reasons, fmt.Sprintf("volume %q is being unmounted", volumeName))

			continue
		}

		if volumeMountStatus.TypedSpec().ReadOnly && !readOnly {
			// Mount requests are merged per volume and end up read-only if every requester asked for
			// read-only, so another holder can leave this one with less access than it asked for.
			ready = false

			reasons = append(reasons, fmt.Sprintf("volume %q is mounted read-only", volumeName))

			continue
		}

		if !volumeMountStatus.Metadata().Finalizers().Has(ctrl.Name()) {
			if err = runtime.AddFinalizer(ctx, volumeMountStatus.Metadata(), ctrl.Name()); err != nil {
				return nil, false, "", fmt.Errorf("failed to add finalizer on %q: %w", requestID, err)
			}

			logger.Info("holding volume mount for container",
				zap.String("container", containerSpecID),
				zap.String("volume", volumeName),
				zap.String("volumeID", volumeID),
				zap.String("target", volumeMountStatus.TypedSpec().Target),
				zap.Bool("readOnly", readOnly),
			)
		}

		resolvedMounts = append(resolvedMounts, containers.ResolvedMountSpec{
			Kind:        containerMountSpec.Kind,
			Source:      volumeMountStatus.TypedSpec().Target,
			Destination: containerMountSpec.Destination,
			Options:     containerMountSpec.Options,
			VolumeID:    volumeID,
			VolumeName:  volumeName,
		})
	}

	return resolvedMounts, ready, strings.Join(reasons, "; "), nil
}

// resolveVolume resolves the volume a volume or text volume mount names to the volume to mount.
//
// A user, existing or external volume resolves directly. A text volume is materialized for this
// container alone: the container asks for an instance with a TextVolumeRequest, and mounts the
// instance's volume once its files are written. The request ID is recorded in wantedTextVolumes.
//
// A non-empty reason means the volume cannot be mounted yet, and says why.
func (ctrl *MountController) resolveVolume(
	ctx context.Context,
	runtime controller.Runtime,
	cfg configcfg.Config,
	containerSpecID string,
	containerMountSpec containers.ContainerMountSpec,
	wantedTextVolumes map[string]struct{},
) (backing configcfg.BackingVolume, reason string, err error) {
	volumeName := containerMountSpec.VolumeName

	// Names are unique across user, existing and external volumes, so a name resolves to at most one
	// volume. Validation rejects a name nothing declares; one reaches here only while the machine
	// config is missing or validation was bypassed, and the container is held back until the volume is
	// declared.
	if containerMountSpec.Kind == containers.MountKindVolume {
		if cfg != nil {
			if backing, resolved := configcfg.ResolveBackingVolume(cfg, volumeName); resolved {
				return backing, "", nil
			}
		}

		return backing, fmt.Sprintf("no UserVolumeConfig, ExistingVolumeConfig or ExternalVolumeConfig named %q is configured", volumeName), nil
	}

	if reason = checkTextVolume(cfg, containerMountSpec); reason != "" {
		return backing, reason, nil
	}

	requestID := ctrl.textVolumeRequestID(containerSpecID, volumeName)
	wantedTextVolumes[requestID] = struct{}{}

	if err = safe.WriterModify(ctx, runtime,
		block.NewTextVolumeRequest(block.NamespaceName, requestID),
		func(res *block.TextVolumeRequest) error {
			res.Metadata().Labels().Set(containerLabel, containerSpecID)

			res.TypedSpec().TextVolumeName = volumeName
			res.TypedSpec().Requester = ctrl.Name()

			return nil
		},
	); err != nil {
		return backing, "", fmt.Errorf("failed to write text volume request %q: %w", requestID, err)
	}

	textVolumeStatus, err := safe.ReaderGetByID[*block.TextVolumeStatus](ctx, runtime, requestID)
	if err != nil {
		if state.IsNotFoundError(err) {
			return backing, fmt.Sprintf("waiting for text volume %q to be written", volumeName), nil
		}

		return backing, "", fmt.Errorf("failed to get text volume status %q: %w", requestID, err)
	}

	if !textVolumeStatus.TypedSpec().Ready {
		if textVolumeStatus.TypedSpec().Error != "" {
			return backing, fmt.Sprintf("text volume %q: %s", volumeName, textVolumeStatus.TypedSpec().Error), nil
		}

		return backing, fmt.Sprintf("waiting for text volume %q to be written", volumeName), nil
	}

	// The instance is mounted secure (nosuid, nodev, noexec) by its writer, and requests are merged:
	// asking for the same keeps the mount from being weakened while this request is the last one.
	return configcfg.BackingVolume{
		ID:       textVolumeStatus.TypedSpec().VolumeID,
		ReadOnly: true,
		Secure:   true,
	}, "", nil
}

// checkTextVolume returns why a text volume mount cannot be set up, or an empty reason if it can.
func checkTextVolume(cfg configcfg.Config, containerMountSpec containers.ContainerMountSpec) string {
	volumeName := containerMountSpec.VolumeName

	if cfg == nil {
		return notConfiguredTextVolumeReason(volumeName)
	}

	if _, resolved := configcfg.ResolveTextVolume(cfg, volumeName); !resolved {
		return notConfiguredTextVolumeReason(volumeName)
	}

	if !containerMountSpec.ReadOnly() {
		// ConfigController always adds ro. The instance is mounted writable on the host for its
		// writer, so a bind without ro would let the container change content owned by the machine
		// configuration.
		return fmt.Sprintf("text volume %q must be mounted read-only", volumeName)
	}

	return ""
}

// notConfiguredTextVolumeReason is the reason a container waits for a text volume nothing declares.
func notConfiguredTextVolumeReason(volumeName string) string {
	return fmt.Sprintf("no TextVolumeConfig named %q is configured", volumeName)
}

// textVolumeRequestID builds the ID of the text volume request for one container and text volume.
//
// Per container, so that every container gets an instance of its own. Slash-separated for the same
// reason as mountRequestID.
func (ctrl *MountController) textVolumeRequestID(containerSpecID, textVolumeName string) string {
	return ctrl.Name() + "/" + containerSpecID + "/" + textVolumeName
}

// releaseUnwanted releases the mounts no container needs any more.
func (ctrl *MountController) releaseUnwanted(
	ctx context.Context,
	runtime controller.Runtime,
	logger *zap.Logger,
	wanted map[string]struct{},
	live map[string]struct{},
) error {
	ownedVolumeMountRequests, err := ctrl.ownedRequests(ctx, runtime)
	if err != nil {
		return err
	}

	for _, volumeMountRequest := range ownedVolumeMountRequests {
		requestID := volumeMountRequest.Metadata().ID()

		if _, stillWanted := wanted[requestID]; stillWanted {
			continue
		}

		containerSpecID, _ := volumeMountRequest.Metadata().Labels().Get(containerLabel)

		// The spec may no longer list the mount while the task still has the path open, so the hold
		// outlives the request for as long as anything is running.
		if _, isLive := live[containerSpecID]; isLive {
			logger.Debug("deferring volume mount release until the container stops",
				zap.String("container", containerSpecID),
				zap.String("request", requestID),
			)

			continue
		}

		if err := ctrl.release(ctx, runtime, logger, requestID); err != nil {
			return err
		}
	}

	return nil
}

// releaseUnwantedTextVolumes drops the text volume requests no container needs any more, reporting
// how many are still held.
//
// Dropping a request makes its writer tear the instance down, which waits for the container's own mount
// of the instance to be released, so a request is kept for as long as the container may be running,
// like the mount itself. With nil wanted and live (the node going down), every request is dropped.
func (ctrl *MountController) releaseUnwantedTextVolumes(
	ctx context.Context,
	runtime controller.Runtime,
	logger *zap.Logger,
	wanted map[string]struct{},
	live map[string]struct{},
) (int, error) {
	textVolumeRequests, err := safe.ReaderListAll[*block.TextVolumeRequest](ctx, runtime)
	if err != nil {
		return 0, fmt.Errorf("failed to list text volume requests: %w", err)
	}

	held := 0

	for textVolumeRequest := range textVolumeRequests.All() {
		if textVolumeRequest.TypedSpec().Requester != ctrl.Name() {
			continue
		}

		requestID := textVolumeRequest.Metadata().ID()

		if _, stillWanted := wanted[requestID]; stillWanted {
			continue
		}

		containerSpecID, _ := textVolumeRequest.Metadata().Labels().Get(containerLabel)

		if _, isLive := live[containerSpecID]; isLive {
			held++

			continue
		}

		if err = runtime.Destroy(ctx, textVolumeRequest.Metadata()); err != nil && !state.IsNotFoundError(err) {
			return 0, fmt.Errorf("failed to destroy text volume request %q: %w", requestID, err)
		}

		logger.Info("released text volume", zap.String("container", containerSpecID), zap.String("request", requestID))
	}

	return held, nil
}

// releaseAll releases every mount this controller holds, reporting how many are still held.
func (ctrl *MountController) releaseAll(ctx context.Context, runtime controller.Runtime, logger *zap.Logger) (int, error) {
	ownedVolumeMountRequests, err := ctrl.ownedRequests(ctx, runtime)
	if err != nil {
		return 0, err
	}

	for _, volumeMountRequest := range ownedVolumeMountRequests {
		if err := ctrl.release(ctx, runtime, logger, volumeMountRequest.Metadata().ID()); err != nil {
			return 0, err
		}
	}

	// Counted after the pass: a request whose teardown is still waiting on another finalizer is
	// still held, and the shutdown barrier has to keep waiting for it.
	remaining, err := ctrl.ownedRequests(ctx, runtime)
	if err != nil {
		return 0, err
	}

	return len(remaining), nil
}

// ownedRequests returns the mount requests created by this controller.
func (ctrl *MountController) ownedRequests(ctx context.Context, reader controller.Reader) ([]*block.VolumeMountRequest, error) {
	volumeMountRequests, err := safe.ReaderListAll[*block.VolumeMountRequest](ctx, reader)
	if err != nil {
		return nil, fmt.Errorf("failed to list mount requests: %w", err)
	}

	var ownedVolumeMountRequests []*block.VolumeMountRequest

	for volumeMountRequest := range volumeMountRequests.All() {
		if volumeMountRequest.TypedSpec().Requester == ctrl.Name() {
			ownedVolumeMountRequests = append(ownedVolumeMountRequests, volumeMountRequest)
		}
	}

	return ownedVolumeMountRequests, nil
}

// release gives back one mount: the finalizer first, then the request itself.
func (ctrl *MountController) release(ctx context.Context, runtime controller.Runtime, logger *zap.Logger, requestID string) error {
	if err := ctrl.releaseFinalizer(ctx, runtime, logger, requestID); err != nil {
		return err
	}

	return ctrl.destroyRequest(ctx, runtime, logger, requestID)
}

// releaseFinalizer drops this controller's hold on the mount status, if it holds one.
func (ctrl *MountController) releaseFinalizer(ctx context.Context, runtime controller.Runtime, logger *zap.Logger, requestID string) error {
	volumeMountStatus, err := safe.ReaderGetByID[*block.VolumeMountStatus](ctx, runtime, requestID)
	if err != nil {
		if state.IsNotFoundError(err) {
			return nil
		}

		return fmt.Errorf("failed to get mount status %q: %w", requestID, err)
	}

	if !volumeMountStatus.Metadata().Finalizers().Has(ctrl.Name()) {
		return nil
	}

	if err := runtime.RemoveFinalizer(ctx, volumeMountStatus.Metadata(), ctrl.Name()); err != nil {
		return fmt.Errorf("failed to remove finalizer on %q: %w", requestID, err)
	}

	logger.Info("released volume mount", zap.String("request", requestID))

	return nil
}

// destroyRequest tears down the mount request and destroys it once nothing holds it.
func (ctrl *MountController) destroyRequest(ctx context.Context, runtime controller.Runtime, logger *zap.Logger, requestID string) error {
	requestMD := block.NewVolumeMountRequest(block.NamespaceName, requestID).Metadata()

	okToDestroy, err := runtime.Teardown(ctx, requestMD)
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

	if err := runtime.Destroy(ctx, requestMD); err != nil && !state.IsNotFoundError(err) {
		return fmt.Errorf("failed to destroy mount request %q: %w", requestID, err)
	}

	return nil
}
