// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package files

import (
	"context"
	"fmt"
	"os"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	configconfig "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/files"
)

// TextVolumeController renders the files of text volumes into their mounted tmpfs.
//
// The volume itself (VolumeConfig and its mount request) is managed by block.VolumeConfigController;
// this controller only writes the files once the volume is mounted, and reports them as rendered
// with a TextVolumeStatus, which containers wait for before they start.
//
// The controller doesn't hold the mount: the tmpfs and the files go away together when the volume is unmounted.
type TextVolumeController struct {
	// SkipFilesystemCheck disables the check that the volume target is a tmpfs, for tests.
	SkipFilesystemCheck bool
}

// Name implements controller.Controller interface.
func (ctrl *TextVolumeController) Name() string {
	return "files.TextVolumeController"
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
			Type:      block.VolumeMountStatusType,
			Kind:      controller.InputWeak,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *TextVolumeController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: files.TextVolumeStatusType,
			Kind: controller.OutputExclusive,
		},
	}
}

// Run implements controller.Controller interface.
func (ctrl *TextVolumeController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		}

		if err := ctrl.reconcile(ctx, r, logger); err != nil {
			return err
		}

		r.ResetRestartBackoff()
	}
}

func (ctrl *TextVolumeController) reconcile(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	cfg, err := safe.ReaderGetByID[*config.MachineConfig](ctx, r, config.ActiveID)
	if err != nil && !state.IsNotFoundError(err) {
		return fmt.Errorf("failed to get machine config: %w", err)
	}

	r.StartTrackingOutputs()

	if cfg != nil {
		for _, textVolumeConfig := range cfg.Config().TextVolumeConfigs() {
			if err = ctrl.reconcileVolume(ctx, r, logger, textVolumeConfig); err != nil {
				return err
			}
		}
	}

	return safe.CleanupOutputs[*files.TextVolumeStatus](ctx, r)
}

func (ctrl *TextVolumeController) reconcileVolume(
	ctx context.Context, r controller.Runtime, logger *zap.Logger, textVolumeConfig configconfig.TextVolumeConfig,
) error {
	volumeID := constants.TextVolumePrefix + textVolumeConfig.Name()

	// the volume mount status of the volume's own mount request, which shares the volume ID
	volumeMountStatus, err := safe.ReaderGetByID[*block.VolumeMountStatus](ctx, r, volumeID)
	if err != nil {
		if state.IsNotFoundError(err) {
			return nil
		}

		return fmt.Errorf("failed to get volume mount status %q: %w", volumeID, err)
	}

	if volumeMountStatus.Metadata().Phase() != resource.PhaseRunning {
		// being unmounted, the status goes away (and the rendered files with the tmpfs)
		return nil
	}

	target := volumeMountStatus.TypedSpec().Target

	hash, err := ctrl.render(target, textVolumeConfig.Files())
	if err != nil {
		// the controller is restarted with a backoff, so the volume is retried
		return fmt.Errorf("failed to render text volume %q at %q: %w", volumeID, target, err)
	}

	return safe.WriterModify(ctx, r, files.NewTextVolumeStatus(files.NamespaceName, volumeID), func(status *files.TextVolumeStatus) error {
		if status.TypedSpec().ContentsHash != hash {
			logger.Info("rendered text volume", zap.String("volume", volumeID), zap.String("target", target), zap.String("hash", hash))
		}

		status.TypedSpec().VolumeName = textVolumeConfig.Name()
		status.TypedSpec().Target = target
		status.TypedSpec().ContentsHash = hash
		status.TypedSpec().Files = len(textVolumeConfig.Files())

		return nil
	})
}

func (ctrl *TextVolumeController) render(target string, textFiles []configconfig.TextVolumeFile) (string, error) {
	root, err := os.OpenRoot(target)
	if err != nil {
		return "", fmt.Errorf("failed to open volume root: %w", err)
	}

	defer root.Close() //nolint:errcheck

	if !ctrl.SkipFilesystemCheck {
		// guard against writing into the directory under the mount point, if the volume was unmounted meanwhile
		if err = checkTmpfs(root); err != nil {
			return "", err
		}
	}

	return WriteTextVolume(root, textFiles)
}

func checkTmpfs(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("failed to open volume root: %w", err)
	}

	defer dir.Close() //nolint:errcheck

	var statfs unix.Statfs_t

	if err = unix.Fstatfs(int(dir.Fd()), &statfs); err != nil {
		return fmt.Errorf("failed to statfs volume root: %w", err)
	}

	if statfs.Type != unix.TMPFS_MAGIC {
		return fmt.Errorf("volume root is not a tmpfs (filesystem type %#x)", statfs.Type)
	}

	return nil
}
