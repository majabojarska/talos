// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"

	machineruntime "github.com/siderolabs/talos/internal/app/machined/pkg/runtime"
	"github.com/siderolabs/talos/internal/pkg/libvirt"
	libvirtstorage "github.com/siderolabs/talos/internal/pkg/libvirt/storage"
	"github.com/siderolabs/talos/pkg/machinery/hypervisorhelpers"
	"github.com/siderolabs/talos/pkg/machinery/resources/hardware"
	"github.com/siderolabs/talos/pkg/machinery/resources/hypervisor"
	"github.com/siderolabs/talos/pkg/machinery/resources/storage"
)

// rawDiskFormat is libvirt's driver type for a file attached as-is.
const rawDiskFormat = "raw"

// VirtualMachineDiskController resolves each disk of a virtual machine to a host source.
//
// A cdrom sourced from a content library is attached in place: its image is read-only, so it is
// never copied into a storage pool. A blank disk is provisioned as a volume in a storage pool.
// Anything else gets a status saying so, which keeps an unimplemented disk visible to the
// operator instead of failing a controller.
type VirtualMachineDiskController struct {
	V1Alpha1Mode machineruntime.Mode

	// Open is injectable for deterministic reconciliation tests.
	Open func(context.Context) (libvirtstorage.Client, error)
}

// Name implements controller.Controller interface.
func (ctrl *VirtualMachineDiskController) Name() string {
	return "hypervisor.VirtualMachineDiskController"
}

// Inputs implements controller.Controller interface.
func (ctrl *VirtualMachineDiskController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: hypervisor.NamespaceName,
			Type:      hypervisor.VirtualMachineSpecType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: hypervisor.NamespaceName,
			Type:      hypervisor.ContentLibraryStatusType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: storage.NamespaceName,
			Type:      storage.StoragePoolStatusType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: hardware.NamespaceName,
			Type:      hardware.SystemInformationType,
			ID:        optional.Some(hardware.SystemInformationID),
			Kind:      controller.InputWeak,
		},
		{
			Namespace: hypervisor.NamespaceName,
			Type:      hypervisor.VirtualMachineDiskStatusType,
			Kind:      controller.InputDestroyReady,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *VirtualMachineDiskController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: hypervisor.VirtualMachineDiskStatusType,
			Kind: controller.OutputExclusive,
		},
	}
}

// Run implements controller.Controller interface.
func (ctrl *VirtualMachineDiskController) Run(ctx context.Context, runtime controller.Runtime, _ *zap.Logger) error {
	// Unlike the controllers that only drive libvirt, this one keeps running in a container:
	// resolving a cdrom is plain filesystem work, and its statuses must keep being produced.
	// Blank disks are the only part that needs a host, and they fail individually instead.
	if ctrl.Open == nil {
		ctrl.Open = func(ctx context.Context) (libvirtstorage.Client, error) {
			return libvirt.New().Storage(ctx)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-runtime.EventCh():
		}

		if err := ctrl.reconcile(ctx, runtime); err != nil {
			return err
		}

		runtime.ResetRestartBackoff()
	}
}

func (ctrl *VirtualMachineDiskController) reconcile(ctx context.Context, runtime controller.Runtime) error {
	specs, err := safe.ReaderListAll[*hypervisor.VirtualMachineSpec](ctx, runtime)
	if err != nil {
		return fmt.Errorf("failed to list virtual machine specs: %w", err)
	}

	libraryStatuses, err := safe.ReaderListAll[*hypervisor.ContentLibraryStatus](ctx, runtime)
	if err != nil {
		return fmt.Errorf("failed to list content library statuses: %w", err)
	}

	libraries := make(map[string]hypervisor.ContentLibraryStatusSpec, libraryStatuses.Len())

	for library := range libraryStatuses.All() {
		libraries[library.Metadata().ID()] = *library.TypedSpec()
	}

	poolStatuses, err := safe.ReaderListAll[*storage.StoragePoolStatus](ctx, runtime)
	if err != nil {
		return fmt.Errorf("failed to list storage pool statuses: %w", err)
	}

	pools := make(map[string]storage.StoragePoolStatusSpec, poolStatuses.Len())

	for pool := range poolStatuses.All() {
		pools[pool.Metadata().ID()] = *pool.TypedSpec()
	}

	// One session serves every blank disk of this pass, and is opened only once one needs it:
	// a host with no blank disk must not depend on the storage daemon at all.
	provisioner := &blankProvisioner{open: ctrl.Open, inContainer: ctrl.V1Alpha1Mode.InContainer()}
	defer provisioner.close()

	desired := map[resource.ID]struct{}{}

	var errs []error

	for vm := range specs.All() {
		name := vm.Metadata().ID()

		for _, disk := range vm.TypedSpec().Disks {
			id := hypervisor.VirtualMachineDiskStatusID(name, disk.Name)
			desired[id] = struct{}{}

			resolved, blank, resolveErr := resolveVirtualMachineDisk(disk, libraries, pools)

			if resolveErr == nil && blank != nil {
				resolved, resolveErr = provisioner.provision(ctx, runtime, name, disk, *blank)
			}

			if err := safe.WriterModify(ctx, runtime,
				hypervisor.NewVirtualMachineDiskStatus(hypervisor.NamespaceName, id),
				func(res *hypervisor.VirtualMachineDiskStatus) error {
					*res.TypedSpec() = resolved
					res.TypedSpec().VirtualMachine = name
					res.TypedSpec().Name = disk.Name

					if resolveErr != nil {
						res.TypedSpec().Error = resolveErr.Error()
					}

					return nil
				},
			); err != nil {
				errs = append(errs, fmt.Errorf("failed to write virtual machine disk status %q: %w", id, err))
			}
		}
	}

	if provisioner.openErr != nil {
		errs = append(errs, fmt.Errorf("failed to connect to the storage daemon: %w", provisioner.openErr))
	}

	return errors.Join(append(errs,
		cleanupOutputs[*hypervisor.VirtualMachineDiskStatus](ctx, runtime, "virtual machine disk status", desired))...)
}

// blankProvisioner opens at most one storage session per reconciliation and reuses it for
// every blank disk of that pass.
type blankProvisioner struct {
	open        func(context.Context) (libvirtstorage.Client, error)
	client      libvirtstorage.Client
	openErr     error
	machineUUID uuid.UUID
	inContainer bool
}

func (p *blankProvisioner) close() {
	if p.client != nil {
		p.client.Close()
	}
}

// provision ensures the disk's volume exists and returns the status describing it.
//
// A failure here is the disk's own: it is reported on that disk's status, so a pool that is
// gone or a daemon that is down never stalls the cdroms resolved in the same pass.
func (p *blankProvisioner) provision(
	ctx context.Context, runtime controller.Runtime,
	virtualMachine string, disk hypervisor.VirtualMachineDiskSpec, blank blankVolume,
) (hypervisor.VirtualMachineDiskStatusSpec, error) {
	if p.inContainer {
		return hypervisor.VirtualMachineDiskStatusSpec{}, errors.New("a blank disk needs a storage pool, which a container has no host to define")
	}

	if p.machineUUID == uuid.Nil {
		machineUUID, err := getMachineUUID(ctx, runtime)
		if err != nil {
			return hypervisor.VirtualMachineDiskStatusSpec{}, err
		}

		p.machineUUID = machineUUID
	}

	if p.client == nil {
		client, err := p.open(ctx)
		if err != nil {
			// Remembered so the reconciliation fails as a whole: no resource event announces
			// the daemon coming back, so only restart backoff will retry this.
			p.openErr = err

			return hypervisor.VirtualMachineDiskStatusSpec{}, fmt.Errorf("failed to connect to the storage daemon: %w", err)
		}

		p.client = client
	}

	blank.volume.Name = hypervisor.VirtualMachineVolumeName(virtualMachine, disk.Name)

	path, err := p.client.EnsureVolume(
		libvirtstorage.Pool{
			Name:   blank.pool,
			UUID:   libvirtstorage.UUID(p.machineUUID, blank.pool),
			Target: blank.target,
		},
		blank.volume,
	)
	if err != nil {
		return hypervisor.VirtualMachineDiskStatusSpec{}, err
	}

	return hypervisor.VirtualMachineDiskStatusSpec{
		SourcePath: path,
		Format:     blank.volume.Format,
		Ready:      true,
	}, nil
}

// blankVolume is a resolved request for one volume in one ready storage pool.
type blankVolume struct {
	pool   string
	target string
	volume libvirtstorage.Volume
}

// resolveVirtualMachineDisk finds the host source for a disk.
//
// Everything that can be decided without libvirt is decided here. A cdrom resolves to a final
// status; a blank disk resolves to the volume the caller must then ensure exists.
//
// A failure to resolve is reported on the status rather than returned to the controller loop: one
// misconfigured disk must not stall the other disks, or the other virtual machines.
func resolveVirtualMachineDisk(
	disk hypervisor.VirtualMachineDiskSpec,
	libraries map[string]hypervisor.ContentLibraryStatusSpec,
	pools map[string]storage.StoragePoolStatusSpec,
) (hypervisor.VirtualMachineDiskStatusSpec, *blankVolume, error) {
	switch disk.Type {
	case hypervisorhelpers.VirtualMachineDiskTypeCDROM.String():
		resolved, err := resolveCDROMDisk(disk, libraries)

		return resolved, nil, err
	case hypervisorhelpers.VirtualMachineDiskTypeDisk.String():
		blank, err := resolveBlankDisk(disk, pools)

		return hypervisor.VirtualMachineDiskStatusSpec{}, blank, err
	default:
		return hypervisor.VirtualMachineDiskStatusSpec{}, nil, fmt.Errorf("unsupported disk type %q", disk.Type)
	}
}

// resolveBlankDisk names the pool volume backing a blank disk, once its pool can hold one.
//
// The spec is re-checked rather than trusted: VirtualMachineSpec is a shared output, so a
// producer other than machine configuration may have authored a disk this never validated.
func resolveBlankDisk(
	disk hypervisor.VirtualMachineDiskSpec,
	pools map[string]storage.StoragePoolStatusSpec,
) (*blankVolume, error) {
	if disk.Provision.FromImage != nil {
		return nil, errors.New("provision.fromImage is not provisioned yet for a disk; only provision.blank is")
	}

	if !disk.Provision.Blank {
		return nil, errors.New("a disk requires provision.blank")
	}

	if disk.Pool == "" {
		return nil, errors.New("a disk requires pool")
	}

	if disk.Size == 0 {
		return nil, errors.New("a disk requires a non-zero size")
	}

	if _, err := hypervisorhelpers.VirtualMachineDiskFormatString(disk.Format); err != nil {
		return nil, err
	}

	pool, found := pools[disk.Pool]
	if !found {
		return nil, fmt.Errorf("storage pool %q is not configured", disk.Pool)
	}

	if !pool.Ready {
		return nil, fmt.Errorf("storage pool %q is not ready: %s", disk.Pool, pool.Error)
	}

	return &blankVolume{
		pool:   disk.Pool,
		target: pool.TargetPath,
		volume: libvirtstorage.Volume{
			Capacity: uint64(disk.Size),
			Format:   disk.Format,
		},
	}, nil
}

// resolveCDROMDisk attaches a content library image in place; nothing copies it, so the library
// file is pinned for as long as the virtual machine refers to it.
func resolveCDROMDisk(
	disk hypervisor.VirtualMachineDiskSpec,
	libraries map[string]hypervisor.ContentLibraryStatusSpec,
) (hypervisor.VirtualMachineDiskStatusSpec, error) {
	image := disk.Provision.FromImage
	if image == nil {
		// Machine configuration validation already requires this of a cdrom; check anyway, so the
		// status is the whole truth about a disk rather than a partial one.
		return hypervisor.VirtualMachineDiskStatusSpec{}, errors.New("a cdrom requires provision.fromImage")
	}

	library, found := libraries[image.Library]
	if !found {
		return hypervisor.VirtualMachineDiskStatusSpec{}, fmt.Errorf("content library %q is not configured", image.Library)
	}

	if !library.Ready {
		return hypervisor.VirtualMachineDiskStatusSpec{}, fmt.Errorf("content library %q is not ready: %s", image.Library, library.Error)
	}

	if err := checkLibraryFile(library.Path, image.File, image.Digest); err != nil {
		return hypervisor.VirtualMachineDiskStatusSpec{}, fmt.Errorf("content library %q: file %q: %w", image.Library, image.File, err)
	}

	return hypervisor.VirtualMachineDiskStatusSpec{
		// The image is attached where it lies: nothing copies it, so the library file is pinned
		// for as long as the virtual machine refers to it.
		SourcePath: filepath.Join(library.Path, image.File),
		Format:     rawDiskFormat,
		ReadOnly:   true,
		Ready:      true,
	}, nil
}

// checkLibraryFile confirms the image exists and, when a digest is pinned, that it still hashes to
// it.
//
// The file is opened through an os.Root rooted at the library, as the API that writes into one
// does: a name is one element within the library and may not lead anywhere else.
func checkLibraryFile(libraryPath, name, expected string) error {
	root, err := os.OpenRoot(libraryPath)
	if err != nil {
		return fmt.Errorf("failed to open content library directory: %w", err)
	}

	defer root.Close() //nolint:errcheck

	f, err := root.Open(name)
	if err != nil {
		return err
	}

	defer f.Close() //nolint:errcheck

	info, err := f.Stat()
	if err != nil {
		return err
	}

	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}

	if expected == "" {
		return nil
	}

	return verifyDigest(f, expected)
}

// verifyDigest rehashes the whole file. It runs on every reconciliation: a library file is not
// immutable, and nothing else notices when it changes underneath a virtual machine.
func verifyDigest(r io.Reader, expected string) error {
	// Machine configuration validation already accepted this digest, including its algorithm.
	dgst, err := digest.Parse(expected)
	if err != nil {
		return fmt.Errorf("digest %q is invalid: %w", expected, err)
	}

	verifier := dgst.Verifier()

	if _, err := io.Copy(verifier, r); err != nil {
		return fmt.Errorf("failed to read for digest verification: %w", err)
	}

	if !verifier.Verified() {
		return fmt.Errorf("digest mismatch: expected %s", dgst)
	}

	return nil
}
