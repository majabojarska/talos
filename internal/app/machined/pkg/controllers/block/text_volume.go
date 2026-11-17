// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package block

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/block/internal/textvolume"
	"github.com/siderolabs/talos/internal/pkg/selinux"
	configconfig "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
)

// textVolumeRetryInterval is how long to wait before writing the files again when the tmpfs is full,
// which happens while the mount controller has not caught up with a larger size limit yet.
const textVolumeRetryInterval = time.Second

// TextVolumeController provides the instances of text volumes asked for by TextVolumeRequests.
//
// Every request gets an instance of its own: a tmpfs volume, mounted under TextVolumeMountPoint, holding
// the files of the TextVolumeConfig document. Consumers never share an inode, even with the same content.
// When the document changes, the files are replaced in place, so a consumer holding a bind of the instance
// sees the new content without being restarted.
//
// The controller holds a writable mount of each instance for itself, and reports the instance in a
// TextVolumeStatus. The consumer mounts the reported volume through a VolumeMountRequest of its own.
type TextVolumeController struct{}

// Name implements controller.Controller interface.
func (ctrl *TextVolumeController) Name() string {
	return "block.TextVolumeController"
}

// Inputs implements controller.Controller interface.
func (ctrl *TextVolumeController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: config.NamespaceName,
			Type:      config.MachineConfigType,
			ID:        optional.Some(config.ActiveID),
			Kind:      controller.InputWeak,
		},
		{
			Namespace: block.NamespaceName,
			Type:      block.TextVolumeRequestType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: block.NamespaceName,
			Type:      block.VolumeMountStatusType,
			Kind:      controller.InputStrong,
		},
		{
			Namespace: block.NamespaceName,
			Type:      block.VolumeConfigType,
			Kind:      controller.InputDestroyReady,
		},
		{
			Namespace: block.NamespaceName,
			Type:      block.VolumeMountRequestType,
			Kind:      controller.InputDestroyReady,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *TextVolumeController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: block.TextVolumeStatusType,
			Kind: controller.OutputExclusive,
		},
		{
			Type: block.VolumeConfigType,
			Kind: controller.OutputShared,
		},
		{
			Type: block.VolumeMountRequestType,
			Kind: controller.OutputShared,
		},
	}
}

// Run implements controller.Controller interface.
func (ctrl *TextVolumeController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	var retryCh <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		case <-retryCh:
		}

		retry, err := ctrl.reconcile(ctx, r, logger)
		if err != nil {
			return err
		}

		retryCh = nil

		if retry {
			retryCh = time.After(textVolumeRetryInterval)
		}

		r.ResetRestartBackoff()
	}
}

// reconcile brings the instances in line with the requests, reporting whether it has to run again
// without waiting for an event.
//
//nolint:gocyclo,cyclop
func (ctrl *TextVolumeController) reconcile(ctx context.Context, r controller.Runtime, logger *zap.Logger) (bool, error) {
	machineConfig, err := safe.ReaderGetByID[*config.MachineConfig](ctx, r, config.ActiveID)
	if err != nil && !state.IsNotFoundError(err) {
		return false, fmt.Errorf("failed to get machine config: %w", err)
	}

	var cfg configconfig.Config

	if machineConfig != nil {
		cfg = machineConfig.Config()
	}

	requests, err := safe.ReaderListAll[*block.TextVolumeRequest](ctx, r)
	if err != nil {
		return false, fmt.Errorf("failed to list text volume requests: %w", err)
	}

	r.StartTrackingOutputs()

	var retry bool

	wanted := map[string]struct{}{}

	for request := range requests.All() {
		if request.Metadata().Phase() == resource.PhaseTearingDown {
			continue
		}

		requestID := request.Metadata().ID()
		name := request.TypedSpec().TextVolumeName
		volumeID := textvolume.InstanceVolumeID(name, requestID)

		status := block.TextVolumeStatusSpec{
			TextVolumeName: name,
			VolumeID:       volumeID,
		}

		var document configconfig.TextVolumeConfig

		if cfg != nil {
			document, _ = configconfig.ResolveTextVolume(cfg, name)
		}

		if document == nil {
			// Not wanted, so the instance is torn down: the consumer is left waiting for the document to
			// be declared, as it would for a volume that has not appeared yet.
			status.Error = fmt.Sprintf("no TextVolumeConfig named %q is configured", name)
		} else {
			wanted[volumeID] = struct{}{}

			instanceRetry, instanceErr := ctrl.reconcileInstance(ctx, r, logger, requestID, volumeID, document.Files(), &status)
			if instanceErr != nil {
				return false, instanceErr
			}

			retry = retry || instanceRetry
		}

		if err = safe.WriterModify(ctx, r, block.NewTextVolumeStatus(block.NamespaceName, requestID),
			func(res *block.TextVolumeStatus) error {
				*res.TypedSpec() = status

				return nil
			},
		); err != nil {
			return false, fmt.Errorf("failed to write text volume status %q: %w", requestID, err)
		}
	}

	if err = safe.CleanupOutputs[*block.TextVolumeStatus](ctx, r); err != nil {
		return false, fmt.Errorf("failed to clean up text volume statuses: %w", err)
	}

	if err = ctrl.cleanupInstances(ctx, r, logger, wanted); err != nil {
		return false, err
	}

	return retry, nil
}

// reconcileInstance sets up one instance, and writes the files to it once it is mounted.
//
// status is filled with the outcome; the returned bool asks for another pass without waiting for an event.
func (ctrl *TextVolumeController) reconcileInstance(
	ctx context.Context,
	r controller.Runtime,
	logger *zap.Logger,
	requestID, volumeID string,
	files map[string]string,
	status *block.TextVolumeStatusSpec,
) (bool, error) {
	volumeConfig, tearingDown, err := ctrl.previousInstance(ctx, r, volumeID)
	if err != nil {
		return false, err
	}

	if tearingDown {
		// A previous instance for this request is still going away (e.g. the document was removed and
		// declared again); a fresh one is set up once it is gone.
		status.Error = "waiting for the previous instance to be torn down"

		return false, nil
	}

	if err = ctrl.setupInstance(ctx, r, volumeID, instanceSize(volumeConfig, files)); err != nil {
		return false, err
	}

	target, err := ctrl.holdMount(ctx, r, volumeID)
	if err != nil || target == "" {
		return false, err
	}

	if err = carryOverReady(ctx, r, requestID, volumeID, status); err != nil {
		return false, err
	}

	if err = writeFiles(target, files); err != nil {
		status.Error = err.Error()

		if errors.Is(err, syscall.ENOSPC) {
			// The size limit is raised together with the content, but the mount controller resizes the
			// tmpfs on its own schedule.
			logger.Debug("text volume is full, waiting for it to be resized", zap.String("volume", volumeID))
		} else {
			logger.Warn("failed to write text volume", zap.String("volume", volumeID), zap.Error(err))
		}

		return true, nil
	}

	contentHash := textvolume.Hash(files)

	if status.ContentHash != contentHash {
		logger.Info("text volume written",
			zap.String("volume", volumeID),
			zap.String("text_volume", status.TextVolumeName),
			zap.String("target", target),
			zap.String("content_hash", contentHash),
		)
	}

	status.Ready = true
	status.ContentHash = contentHash

	return false, nil
}

// previousInstance returns the volume config of an instance set up earlier (nil if there is none), and
// whether that instance is still being torn down.
func (ctrl *TextVolumeController) previousInstance(ctx context.Context, r controller.Runtime, volumeID string) (*block.VolumeConfig, bool, error) {
	volumeConfig, err := safe.ReaderGetByID[*block.VolumeConfig](ctx, r, volumeID)
	if err != nil && !state.IsNotFoundError(err) {
		return nil, false, fmt.Errorf("failed to get volume config %q: %w", volumeID, err)
	}

	volumeMountRequest, err := safe.ReaderGetByID[*block.VolumeMountRequest](ctx, r, volumeID)
	if err != nil && !state.IsNotFoundError(err) {
		return nil, false, fmt.Errorf("failed to get volume mount request %q: %w", volumeID, err)
	}

	tearingDown := (volumeConfig != nil && volumeConfig.Metadata().Phase() == resource.PhaseTearingDown) ||
		(volumeMountRequest != nil && volumeMountRequest.Metadata().Phase() == resource.PhaseTearingDown)

	return volumeConfig, tearingDown, nil
}

// instanceSize returns the tmpfs size limit of an instance holding files.
//
// The size limit only grows for the lifetime of an instance: shrinking a tmpfs below what it holds fails,
// and the content being replaced may still be there.
func instanceSize(volumeConfig *block.VolumeConfig, files map[string]string) uint64 {
	size := textvolume.Size(files)

	if volumeConfig == nil {
		return size
	}

	if current, ok := block.TmpfsSize(volumeConfig.TypedSpec().Mount.Parameters); ok {
		size = max(size, current)
	}

	return size
}

// setupInstance writes the tmpfs volume of an instance, and the mount request this controller holds for
// writing to it.
func (ctrl *TextVolumeController) setupInstance(ctx context.Context, r controller.Runtime, volumeID string, size uint64) error {
	if err := safe.WriterModify(ctx, r, block.NewVolumeConfig(block.NamespaceName, volumeID), func(vc *block.VolumeConfig) error {
		vc.Metadata().Labels().Set(block.TextVolumeLabel, "")

		vc.TypedSpec().Type = block.VolumeTypeTmpfs
		vc.TypedSpec().Mount = block.MountSpec{
			TargetPath:   filepath.Join(constants.TextVolumeMountPoint, volumeID),
			SelinuxLabel: constants.TextVolumeSelinuxLabel,
			FileMode:     textvolume.DirMode,
			Parameters: []block.ParameterSpec{
				block.NewStringParameter(block.TmpfsSizeParameter, strconv.FormatUint(size, 10)),
			},
		}

		return nil
	}); err != nil {
		return fmt.Errorf("failed to write volume config %q: %w", volumeID, err)
	}

	// A writable mount held for writing the files. Consumers only ever get read-only binds of it.
	if err := safe.WriterModify(ctx, r, block.NewVolumeMountRequest(block.NamespaceName, volumeID), func(vmr *block.VolumeMountRequest) error {
		vmr.Metadata().Labels().Set(block.TextVolumeLabel, "")

		vmr.TypedSpec().VolumeID = volumeID
		vmr.TypedSpec().Requester = ctrl.Name()
		vmr.TypedSpec().Secure = true
		vmr.TypedSpec().NoExec = true

		return nil
	}); err != nil {
		return fmt.Errorf("failed to write volume mount request %q: %w", volumeID, err)
	}

	return nil
}

// holdMount returns the path an instance is mounted at, holding the mount for this controller, or an
// empty path if the instance is not mounted (or is being unmounted).
//
// The mount is held while writing, so that it cannot go away underneath, leaving the files in the bare
// mount point directory instead.
func (ctrl *TextVolumeController) holdMount(ctx context.Context, r controller.Runtime, volumeID string) (string, error) {
	volumeMountStatus, err := safe.ReaderGetByID[*block.VolumeMountStatus](ctx, r, volumeID)
	if err != nil {
		if state.IsNotFoundError(err) {
			return "", nil
		}

		return "", fmt.Errorf("failed to get volume mount status %q: %w", volumeID, err)
	}

	if volumeMountStatus.Metadata().Phase() != resource.PhaseRunning {
		// Being unmounted: nothing is written to a mount on its way out.
		return "", ctrl.releaseMount(ctx, r, volumeID)
	}

	if !volumeMountStatus.Metadata().Finalizers().Has(ctrl.Name()) {
		if err = r.AddFinalizer(ctx, volumeMountStatus.Metadata(), ctrl.Name()); err != nil {
			return "", fmt.Errorf("failed to add finalizer to volume mount status %q: %w", volumeID, err)
		}
	}

	return volumeMountStatus.TypedSpec().Target, nil
}

// carryOverReady keeps an instance reported ready while its content is updated.
//
// The content stays readable while it is being replaced, so a consumer already using the instance is not
// held back by an update which has not landed yet.
func carryOverReady(ctx context.Context, r controller.Reader, requestID, volumeID string, status *block.TextVolumeStatusSpec) error {
	previous, err := safe.ReaderGetByID[*block.TextVolumeStatus](ctx, r, requestID)
	if err != nil {
		if state.IsNotFoundError(err) {
			return nil
		}

		return fmt.Errorf("failed to get text volume status %q: %w", requestID, err)
	}

	if previous.TypedSpec().VolumeID == volumeID && previous.TypedSpec().Ready {
		status.Ready = true
		status.ContentHash = previous.TypedSpec().ContentHash
	}

	return nil
}

// writeFiles writes the files of an instance mounted at target, and labels them.
func writeFiles(target string, files map[string]string) error {
	if err := textvolume.Write(target, files); err != nil {
		return err
	}

	if err := selinux.SetLabelRecursive(target, constants.TextVolumeSelinuxLabel); err != nil {
		return fmt.Errorf("failed to label the files: %w", err)
	}

	return nil
}

// cleanupInstances tears down the instances this controller set up which are no longer wanted.
//
// The mount goes first, and the volume once nothing mounts it.
func (ctrl *TextVolumeController) cleanupInstances(ctx context.Context, r controller.Runtime, logger *zap.Logger, wanted map[string]struct{}) error {
	if err := ctrl.cleanupMountRequests(ctx, r, wanted); err != nil {
		return err
	}

	return ctrl.cleanupVolumeConfigs(ctx, r, logger, wanted)
}

// cleanupMountRequests releases the mounts of the instances which are no longer wanted.
func (ctrl *TextVolumeController) cleanupMountRequests(ctx context.Context, r controller.Runtime, wanted map[string]struct{}) error {
	volumeMountRequests, err := safe.ReaderListAll[*block.VolumeMountRequest](ctx, r, state.WithLabelQuery(resource.LabelExists(block.TextVolumeLabel)))
	if err != nil {
		return fmt.Errorf("failed to list volume mount requests: %w", err)
	}

	for volumeMountRequest := range volumeMountRequests.All() {
		if !ctrl.isStale(volumeMountRequest.Metadata(), wanted) {
			continue
		}

		if err = ctrl.releaseMount(ctx, r, volumeMountRequest.Metadata().ID()); err != nil {
			return err
		}

		if err = ctrl.teardownAndDestroy(ctx, r, volumeMountRequest.Metadata()); err != nil {
			return err
		}
	}

	return nil
}

// cleanupVolumeConfigs tears down the volumes of the instances which are no longer wanted.
func (ctrl *TextVolumeController) cleanupVolumeConfigs(ctx context.Context, r controller.Runtime, logger *zap.Logger, wanted map[string]struct{}) error {
	volumeConfigs, err := safe.ReaderListAll[*block.VolumeConfig](ctx, r, state.WithLabelQuery(resource.LabelExists(block.TextVolumeLabel)))
	if err != nil {
		return fmt.Errorf("failed to list volume configs: %w", err)
	}

	for volumeConfig := range volumeConfigs.All() {
		if !ctrl.isStale(volumeConfig.Metadata(), wanted) {
			continue
		}

		if volumeConfig.Metadata().Phase() == resource.PhaseRunning {
			logger.Info("tearing down text volume", zap.String("volume", volumeConfig.Metadata().ID()))
		}

		if err = ctrl.teardownAndDestroy(ctx, r, volumeConfig.Metadata()); err != nil {
			return err
		}
	}

	return nil
}

// isStale reports whether a resource of an instance was set up by this controller, and is no longer wanted.
func (ctrl *TextVolumeController) isStale(md *resource.Metadata, wanted map[string]struct{}) bool {
	if md.Owner() != ctrl.Name() {
		return false
	}

	_, isWanted := wanted[md.ID()]

	return !isWanted
}

// releaseMount drops this controller's hold on the mount of an instance, if it holds one.
func (ctrl *TextVolumeController) releaseMount(ctx context.Context, r controller.Runtime, volumeID string) error {
	volumeMountStatus, err := safe.ReaderGetByID[*block.VolumeMountStatus](ctx, r, volumeID)
	if err != nil {
		if state.IsNotFoundError(err) {
			return nil
		}

		return fmt.Errorf("failed to get volume mount status %q: %w", volumeID, err)
	}

	if !volumeMountStatus.Metadata().Finalizers().Has(ctrl.Name()) {
		return nil
	}

	if err = r.RemoveFinalizer(ctx, volumeMountStatus.Metadata(), ctrl.Name()); err != nil {
		return fmt.Errorf("failed to remove finalizer from volume mount status %q: %w", volumeID, err)
	}

	return nil
}

// teardownAndDestroy tears down a resource, and destroys it once nothing holds it any more.
func (ctrl *TextVolumeController) teardownAndDestroy(ctx context.Context, r controller.Runtime, md *resource.Metadata) error {
	okToDestroy, err := r.Teardown(ctx, md)
	if err != nil {
		if state.IsNotFoundError(err) {
			return nil
		}

		return fmt.Errorf("failed to tear down %s %q: %w", md.Type(), md.ID(), err)
	}

	if !okToDestroy {
		return nil
	}

	if err = r.Destroy(ctx, md); err != nil && !state.IsNotFoundError(err) {
		return fmt.Errorf("failed to destroy %s %q: %w", md.Type(), md.ID(), err)
	}

	return nil
}
