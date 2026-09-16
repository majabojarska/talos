// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package hv provides controllers for the Talos hypervisor.
package hv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/block/mountholder"
	configcfg "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/hv"
)

// contentLibraryDirMode is the mode of the directories this controller creates, owner-only: nothing
// but the content library API has business reading a library.
const contentLibraryDirMode = 0o700

// ContentLibraryController resolves content libraries to directories on their backing volumes.
//
// Its side effects are the block.VolumeMountRequest resources it creates, the finalizers it holds
// on the resulting block.VolumeMountStatus, and the library directories it creates on the volume.
// The finalizer is what stops a volume being unmounted while a library on it is in use; it is
// dropped as soon as the mount starts tearing down, so a shutdown is never blocked by it.
type ContentLibraryController struct{}

// Name implements controller.Controller interface.
func (ctrl *ContentLibraryController) Name() string {
	return "hv.ContentLibraryController"
}

// holder gives back the mounts this controller has taken.
func (ctrl *ContentLibraryController) holder() mountholder.Holder {
	return mountholder.Holder{Requester: ctrl.Name()}
}

// Inputs implements controller.Controller interface.
func (ctrl *ContentLibraryController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: config.NamespaceName,
			Type:      config.MachineConfigType,
			ID:        optional.Some(config.ActiveID),
			Kind:      controller.InputWeak,
		},
		{
			// Read to tell a system volume from a user one: the config can only check the volume ID,
			// and an ID is not a guarantee of what the volume turned out to be.
			Namespace: block.NamespaceName,
			Type:      block.VolumeStatusType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: block.NamespaceName,
			Type:      block.VolumeMountStatusType,
			Kind:      controller.InputStrong,
		},
		{
			// InputDestroyReady/OutputShared: see the mountholder package.
			Namespace: block.NamespaceName,
			Type:      block.VolumeMountRequestType,
			Kind:      controller.InputDestroyReady,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *ContentLibraryController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: hv.ContentLibraryStatusType,
			Kind: controller.OutputExclusive,
		},
		{
			Type: block.VolumeMountRequestType,
			Kind: controller.OutputShared,
		},
	}
}

// Run implements controller.Controller interface.
func (ctrl *ContentLibraryController) Run(ctx context.Context, runtime controller.Runtime, logger *zap.Logger) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-runtime.EventCh():
		}

		if err := ctrl.reconcile(ctx, runtime, logger); err != nil {
			logger.Error("failed to reconcile content libraries", zap.Error(err))

			return err
		}

		runtime.ResetRestartBackoff()
	}
}

// mountRequestID builds the ID of the mount request backing one content library.
//
// Per library rather than per volume, so two libraries sharing a volume hold it independently and
// removing one does not release the other's mount.
func (ctrl *ContentLibraryController) mountRequestID(libraryID string) string {
	return ctrl.Name() + "/" + libraryID
}

func (ctrl *ContentLibraryController) reconcile(ctx context.Context, runtime controller.Runtime, logger *zap.Logger) error {
	machineConfig, err := safe.ReaderGetByID[*config.MachineConfig](ctx, runtime, config.ActiveID)
	if err != nil && !state.IsNotFoundError(err) {
		return fmt.Errorf("failed to get machine config: %w", err)
	}

	runtime.StartTrackingOutputs()

	wanted := map[string]struct{}{}

	if machineConfig != nil && machineConfig.Config() != nil {
		for _, contentLibraryConfig := range machineConfig.Config().ContentLibraryConfigs() {
			if err := ctrl.reconcileStatus(ctx, runtime, logger, contentLibraryConfig, wanted); err != nil {
				return err
			}
		}
	}

	if err := ctrl.releaseUnwanted(ctx, runtime, logger, wanted); err != nil {
		return err
	}

	if err := safe.CleanupOutputs[*hv.ContentLibraryStatus](ctx, runtime); err != nil {
		return fmt.Errorf("failed to clean up outputs: %w", err)
	}

	return nil
}

// reconcileStatus brings one content library up and reports what came of it on its status.
func (ctrl *ContentLibraryController) reconcileStatus(
	ctx context.Context,
	runtime controller.Runtime,
	logger *zap.Logger,
	contentLibraryConfig configcfg.ContentLibraryConfig,
	wanted map[string]struct{},
) error {
	libraryID := contentLibraryConfig.Name()
	volumeID := contentLibraryConfig.BackingVolumeID()

	path, reason, err := ctrl.reconcileLibrary(ctx, runtime, logger, libraryID, volumeID, wanted)
	if err != nil {
		return err
	}

	// Sweeping is only safe on the transition to ready, so the transition is noticed here, where the
	// previous status is still readable.
	var becameReady bool

	if err := safe.WriterModify(ctx, runtime,
		hv.NewContentLibraryStatus(hv.NamespaceName, libraryID),
		func(res *hv.ContentLibraryStatus) error {
			becameReady = reason == "" && !res.TypedSpec().Ready

			res.TypedSpec().VolumeID = volumeID
			res.TypedSpec().Path = path
			res.TypedSpec().Ready = reason == ""
			res.TypedSpec().Error = reason

			return nil
		},
	); err != nil {
		return fmt.Errorf("failed to write content library status %q: %w", libraryID, err)
	}

	if becameReady {
		ctrl.sweepStagedUploads(logger, libraryID, path)
	}

	return nil
}

// reconcileLibrary requests the mount one library needs and resolves it to a directory.
//
// A non-empty reason means the library is not ready, and is reported as-is on the status.
//
//nolint:gocyclo,cyclop
func (ctrl *ContentLibraryController) reconcileLibrary(
	ctx context.Context,
	runtime controller.Runtime,
	logger *zap.Logger,
	libraryID string,
	volumeID string,
	wanted map[string]struct{},
) (path, reason string, err error) {
	volumeStatus, err := safe.ReaderGetByID[*block.VolumeStatus](ctx, runtime, volumeID)
	if err != nil {
		if !state.IsNotFoundError(err) {
			return "", "", fmt.Errorf("failed to get volume status %q: %w", volumeID, err)
		}

		return "", fmt.Sprintf("volume %q is not configured", volumeID), nil
	}

	// The config-time check only sees the volume ID; this is the one that sees what the volume
	// actually is. Requesting a mount of a system volume would otherwise expose e.g. EPHEMERAL to
	// the content library API.
	if _, isSystemVolume := volumeStatus.Metadata().Labels().Get(block.SystemVolumeLabel); isSystemVolume {
		return "", fmt.Sprintf("volume %q is a system volume", volumeID), nil
	}

	requestID := ctrl.mountRequestID(libraryID)
	wanted[requestID] = struct{}{}

	if err = safe.WriterModify(ctx, runtime,
		block.NewVolumeMountRequest(block.NamespaceName, requestID),
		func(res *block.VolumeMountRequest) error {
			res.TypedSpec().VolumeID = volumeID
			res.TypedSpec().Requester = ctrl.Name()
			res.TypedSpec().ReadOnly = false
			// Detached must stay false: a detached mount is reachable only through a file descriptor
			// and never appears at its target, so there would be no path to store the library in.
			res.TypedSpec().Detached = false
			// Virtual machine images are opaque data uploaded over the API: nothing on the node is
			// meant to execute them, and no setuid bit on them is meant to be honored.
			res.TypedSpec().Secure = true
			res.TypedSpec().NoExec = true

			return nil
		},
	); err != nil {
		return "", "", fmt.Errorf("failed to write mount request %q: %w", requestID, err)
	}

	volumeMountStatus, err := safe.ReaderGetByID[*block.VolumeMountStatus](ctx, runtime, requestID)
	if err != nil {
		if !state.IsNotFoundError(err) {
			return "", "", fmt.Errorf("failed to get mount status %q: %w", requestID, err)
		}

		return "", fmt.Sprintf("waiting for volume %q to be mounted", volumeID), nil
	}

	if volumeMountStatus.Metadata().Phase() != resource.PhaseRunning {
		// The mount is on its way out, either because the volume is going away or because the node
		// is shutting down. Holding on would block that teardown forever, so the hold is dropped and
		// the library reported as not ready.
		if err = ctrl.holder().ReleaseFinalizer(ctx, runtime, logger, requestID); err != nil {
			return "", "", err
		}

		return "", fmt.Sprintf("volume %q is being unmounted", volumeID), nil
	}

	if volumeMountStatus.TypedSpec().ReadOnly {
		// Mount requests are merged per volume and end up read-only if every requester asked for
		// read-only, so another holder could leave this one unable to write.
		return "", fmt.Sprintf("volume %q is mounted read-only", volumeID), nil
	}

	if !volumeMountStatus.Metadata().Finalizers().Has(ctrl.Name()) {
		if err = runtime.AddFinalizer(ctx, volumeMountStatus.Metadata(), ctrl.Name()); err != nil {
			return "", "", fmt.Errorf("failed to add finalizer on %q: %w", requestID, err)
		}

		logger.Info("holding volume mount for content library",
			zap.String("library", libraryID),
			zap.String("volume", volumeID),
			zap.String("target", volumeMountStatus.TypedSpec().Target),
		)
	}

	path = filepath.Join(volumeMountStatus.TypedSpec().Target, constants.ContentLibraryDirectory, libraryID)

	if err = os.MkdirAll(path, contentLibraryDirMode); err != nil {
		return "", fmt.Sprintf("failed to create the library directory: %s", err), nil
	}

	return path, "", nil
}

// sweepStagedUploads removes what interrupted uploads left staged in a library.
//
// Staged names are dot-prefixed, so they are invisible to List and unaddressable by Delete: the
// content library API cannot clear them, which is why this has to happen here. Safe only on the
// transition to ready, since the API refuses a library which is not ready and so nothing can be
// staged at that moment.
func (ctrl *ContentLibraryController) sweepStagedUploads(logger *zap.Logger, libraryID, path string) {
	entries, err := os.ReadDir(path)
	if err != nil {
		logger.Warn("failed to sweep the content library", zap.String("library", libraryID), zap.Error(err))

		return
	}

	for _, entry := range entries {
		name := entry.Name()

		if !strings.HasPrefix(name, ".") || !strings.HasSuffix(name, constants.ContentLibraryInflightUploadSuffix) {
			continue
		}

		if err := os.Remove(filepath.Join(path, name)); err != nil {
			logger.Warn("failed to remove a staged upload", zap.String("library", libraryID), zap.String("name", name), zap.Error(err))

			continue
		}

		logger.Info("removed a staged upload left behind by an interrupted upload",
			zap.String("library", libraryID),
			zap.String("name", name),
		)
	}
}

// releaseUnwanted releases the mounts no content library needs any more.
func (ctrl *ContentLibraryController) releaseUnwanted(
	ctx context.Context,
	runtime controller.Runtime,
	logger *zap.Logger,
	wanted map[string]struct{},
) error {
	ownedVolumeMountRequests, err := ctrl.holder().OwnedRequests(ctx, runtime)
	if err != nil {
		return err
	}

	for _, volumeMountRequest := range ownedVolumeMountRequests {
		requestID := volumeMountRequest.Metadata().ID()

		if _, stillWanted := wanted[requestID]; stillWanted {
			continue
		}

		if err := ctrl.holder().Release(ctx, runtime, logger, requestID); err != nil {
			return err
		}
	}

	return nil
}
