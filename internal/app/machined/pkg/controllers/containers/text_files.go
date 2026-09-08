// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"

	v1alpha1runtime "github.com/siderolabs/talos/internal/app/machined/pkg/runtime"
	"github.com/siderolabs/talos/internal/pkg/selinux"
	configcfg "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/containers"
)

// textFilesDirMode is the mode of the directories making up a materialized tree.
const textFilesDirMode = 0o755

// textFilesFileMode is the mode of the files in a materialized tree.
//
// Fixed rather than configurable: the mount is read-only from the container's side anyway, and a
// per-file mode is only meaningful once the document can carry secrets, which it deliberately
// cannot yet.
const textFilesFileMode = 0o644

// TextFilesController materializes TextFilesConfig documents as directory trees on the host.
//
// One tree per (container, document) pair, so that a container going away takes its own copy with it
// and two containers mounting the same document never share an inode. The trees live on a tmpfs and
// are rebuilt from the machine configuration on every boot, so nothing here is persistent state to
// be reconciled against; the only thing being reconciled is the filesystem against the configuration.
type TextFilesController struct {
	V1Alpha1Mode v1alpha1runtime.Mode
	BaseDir      string
}

// Name implements controller.Controller interface.
func (ctrl *TextFilesController) Name() string {
	return "containers.TextFilesController"
}

// Inputs implements controller.Controller interface.
func (ctrl *TextFilesController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: config.NamespaceName,
			Type:      config.MachineConfigType,
			ID:        optional.Some(config.ActiveID),
			Kind:      controller.InputWeak,
		},
		{
			// The container specs say which documents are actually wanted, and by whom. Reading them
			// rather than materializing every document keeps an unreferenced document off the disk
			// entirely.
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerSpecType,
			Kind:      controller.InputWeak,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *TextFilesController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: containers.TextFilesStatusType,
			Kind: controller.OutputExclusive,
		},
	}
}

// Run implements controller.Controller interface.
func (ctrl *TextFilesController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	if ctrl.V1Alpha1Mode == v1alpha1runtime.ModeContainer {
		// Talos itself running in a container has no /system tmpfs to write into, and no Talos
		// containers to serve, so there is nothing to do.
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		}

		if err := ctrl.reconcile(ctx, r, logger); err != nil {
			logger.Error("failed to materialize container text files", zap.Error(err))

			return err
		}

		r.ResetRestartBackoff()
	}
}

//nolint:gocyclo
func (ctrl *TextFilesController) reconcile(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	documents, err := ctrl.readDocuments(ctx, r)
	if err != nil {
		return err
	}

	specs, err := safe.ReaderListAll[*containers.ContainerSpec](ctx, r)
	if err != nil {
		return fmt.Errorf("failed to list container specs: %w", err)
	}

	if err = os.MkdirAll(ctrl.BaseDir, textFilesDirMode); err != nil {
		return fmt.Errorf("failed to create directory %q: %w", ctrl.BaseDir, err)
	}

	r.StartTrackingOutputs()

	// Every path this pass is responsible for, so that anything else under BaseDir can be removed.
	touched := map[string]struct{}{}

	for spec := range specs.All() {
		containerID := spec.Metadata().ID()

		for _, mount := range spec.TypedSpec().Mounts {
			if mount.Kind != containers.MountKindTextFiles {
				continue
			}

			path, contentHash, materializeErr := ctrl.materialize(logger, containerID, mount.TextFilesName, documents, touched)

			var reason string

			if materializeErr != nil {
				// A missing document is a configuration problem, not a controller failure: report it
				// on the status and let the container wait, exactly as it would for a volume that has
				// not appeared yet.
				reason = materializeErr.Error()
				path = ""
				contentHash = ""
			}

			if err = safe.WriterModify(ctx, r,
				containers.NewTextFilesStatus(containers.NamespaceName, containers.TextFilesStatusID(containerID, mount.TextFilesName)),
				func(res *containers.TextFilesStatus) error {
					res.TypedSpec().ContainerID = containerID
					res.TypedSpec().DocumentName = mount.TextFilesName
					res.TypedSpec().Path = path
					res.TypedSpec().ContentHash = contentHash
					res.TypedSpec().Error = reason

					return nil
				},
			); err != nil {
				return fmt.Errorf("failed to write text files status for %q: %w", containerID, err)
			}
		}
	}

	if err = ctrl.prune(logger, touched); err != nil {
		return err
	}

	if err = safe.CleanupOutputs[*containers.TextFilesStatus](ctx, r); err != nil {
		return fmt.Errorf("failed to clean up outputs: %w", err)
	}

	return nil
}

// readDocuments returns the TextFilesConfig documents by name.
func (ctrl *TextFilesController) readDocuments(ctx context.Context, r controller.Runtime) (map[string]configcfg.TextFilesConfig, error) {
	cfg, err := safe.ReaderGetByID[*config.MachineConfig](ctx, r, config.ActiveID)
	if err != nil {
		if state.IsNotFoundError(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("failed to get machine config: %w", err)
	}

	documents := map[string]configcfg.TextFilesConfig{}

	for _, document := range cfg.Config().TextFilesConfigs() {
		documents[document.Name()] = document
	}

	return documents, nil
}

// materialize writes one document's tree for one container, returning its path and content hash.
//
// Every path it is responsible for is recorded in touched, including the ancestor directories, so
// that prune can tell them apart from leftovers.
func (ctrl *TextFilesController) materialize(
	logger *zap.Logger,
	containerID, documentName string,
	documents map[string]configcfg.TextFilesConfig,
	touched map[string]struct{},
) (string, string, error) {
	document, exists := documents[documentName]
	if !exists {
		return "", "", fmt.Errorf("text files %q is not configured", documentName)
	}

	files := document.FileContents()

	root := filepath.Join(ctrl.BaseDir, containerID, documentName)

	touched[filepath.Join(ctrl.BaseDir, containerID)] = struct{}{}
	touched[root] = struct{}{}

	if err := os.MkdirAll(root, textFilesDirMode); err != nil {
		return "", "", fmt.Errorf("failed to create directory %q: %w", root, err)
	}

	// Sorted so that a parent directory is always created before anything inside it, and so that a
	// failure reports the same file every time.
	for _, name := range slices.Sorted(maps.Keys(files)) {
		target := filepath.Join(root, name)

		if dir := filepath.Dir(target); dir != root {
			if err := os.MkdirAll(dir, textFilesDirMode); err != nil {
				return "", "", fmt.Errorf("failed to create directory %q: %w", dir, err)
			}

			// Record every directory between root and the file, not just the immediate parent.
			for d := dir; d != root; d = filepath.Dir(d) {
				touched[d] = struct{}{}
			}
		}

		if err := updateFile(target, []byte(files[name]), textFilesFileMode); err != nil {
			return "", "", fmt.Errorf("failed to write file %q: %w", target, err)
		}

		touched[target] = struct{}{}
	}

	// Relabelled on every pass rather than only after a write: SetLabel is a no-op when the label
	// already matches, and this way a tree left unlabelled by an interrupted earlier pass is fixed
	// rather than leaving the container unable to read its own files.
	if err := selinux.SetLabelRecursive(root, constants.TalosContainerTextFilesSelinuxLabel); err != nil {
		return "", "", fmt.Errorf("failed to label %q: %w", root, err)
	}

	logger.Debug("text files materialized",
		zap.String("container", containerID),
		zap.String("document", documentName),
		zap.String("path", root),
	)

	return root, hashTextFiles(files), nil
}

// prune removes everything under BaseDir that this pass is not responsible for.
func (ctrl *TextFilesController) prune(logger *zap.Logger, touched map[string]struct{}) error {
	return filepath.WalkDir(ctrl.BaseDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if path == ctrl.BaseDir {
			return nil
		}

		if _, wanted := touched[path]; wanted {
			return nil
		}

		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("failed to remove %q: %w", path, err)
		}

		logger.Info("removed stale container text files", zap.String("path", path))

		if d.IsDir() {
			// The directory is gone; walking into it would fail.
			return filepath.SkipDir
		}

		return nil
	})
}

// hashTextFiles returns a stable digest of a file set.
//
// Sorted by path, and both halves NUL-terminated, so that map iteration order cannot change the
// result and no two distinct sets can produce the same stream. NUL is safe as a separator because
// validation rejects it in content, and a path cannot contain one.
func hashTextFiles(files map[string]string) string {
	hash := sha256.New()

	for _, name := range slices.Sorted(maps.Keys(files)) {
		hash.Write([]byte(name))
		hash.Write([]byte{0})
		hash.Write([]byte(files[name]))
		hash.Write([]byte{0})
	}

	return hex.EncodeToString(hash.Sum(nil))
}

// updateFile is like os.WriteFile, but only writes when the contents differ, and replaces the file
// atomically via write-temp-then-rename.
//
// Keeps an unchanged reconcile off the disk entirely, which matters because this runs on every
// machine configuration event. Atomic replacement matters because the tree is bind-mounted
// read-only into a running container: a truncate-then-write would let that container observe a
// torn or partially-new file before InstanceController notices the ContentHash drift and replaces
// it. The temp file is created in the same directory as the target so the rename is same-filesystem
// and therefore atomic; one orphaned by a crash mid-update is harmless, since it isn't in prune's
// touched set and gets removed as stale on the next reconcile.
func updateFile(filename string, contents []byte, mode os.FileMode) error {
	oldContents, err := os.ReadFile(filename)
	if err == nil && bytes.Equal(oldContents, contents) {
		return nil
	}

	dir := filepath.Dir(filename)

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(filename)+".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file in %q: %w", dir, err)
	}

	tmpName := tmp.Name()

	defer os.Remove(tmpName) //nolint:errcheck

	if _, err = tmp.Write(contents); err != nil {
		tmp.Close() //nolint:errcheck

		return fmt.Errorf("failed to write %q: %w", tmpName, err)
	}

	if err = tmp.Chmod(mode); err != nil {
		tmp.Close() //nolint:errcheck

		return fmt.Errorf("failed to chmod %q: %w", tmpName, err)
	}

	if err = tmp.Close(); err != nil {
		return fmt.Errorf("failed to close %q: %w", tmpName, err)
	}

	if err = os.Rename(tmpName, filename); err != nil {
		return fmt.Errorf("failed to rename %q to %q: %w", tmpName, filename, err)
	}

	return nil
}
