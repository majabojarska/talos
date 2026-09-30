// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package container_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	hypervisorcfg "github.com/siderolabs/talos/pkg/machinery/config/types/hypervisor"
	"github.com/siderolabs/talos/pkg/machinery/config/types/meta"
	storagecfg "github.com/siderolabs/talos/pkg/machinery/config/types/storage"
	"github.com/siderolabs/talos/pkg/machinery/hypervisorhelpers"
)

// newVirtualMachineDoc builds a VirtualMachineConfig document whose disks are provisioned from the
// named content libraries, one disk per library.
func newVirtualMachineDoc(name string, libraryNames ...string) *hypervisorcfg.VirtualMachineConfigV1Alpha1 {
	doc := hypervisorcfg.NewVirtualMachineConfigV1Alpha1()
	doc.MetaName = name
	doc.CPUConfig.CPUCount = 1
	doc.MemoryConfig.MemorySize = meta.MustByteSize("1GiB")

	// Cdroms, so this fixture references content libraries and no storage pool at all: a pool
	// reference is checked separately and would be noise here.
	for i, libraryName := range libraryNames {
		doc.DisksConfig = append(doc.DisksConfig, hypervisorcfg.VirtualMachineDisk{
			DiskName: "disk" + string(rune('a'+i)),
			DiskType: hypervisorhelpers.VirtualMachineDiskTypeCDROM,
			ProvisionConfig: hypervisorcfg.VirtualMachineDiskProvision{
				FromImageConfig: &hypervisorcfg.VirtualMachineDiskFromImage{
					ImageLibrary: libraryName,
					ImageFile:    "talos.iso",
				},
			},
		})
	}

	return doc
}

// newStoragePoolDoc builds a StoragePool backed by the named volume.
func newStoragePoolDoc(name, volumeName string) *storagecfg.StoragePoolV1Alpha1 {
	doc := storagecfg.NewStoragePoolV1Alpha1()
	doc.MetaName = name
	doc.VolumeConfig.VolumeName = volumeName

	return doc
}

// newBlankDiskVirtualMachineDoc builds a VirtualMachineConfig whose single disk is blank, so it
// references no content library at all.
func newBlankDiskVirtualMachineDoc(name string) *hypervisorcfg.VirtualMachineConfigV1Alpha1 {
	doc := hypervisorcfg.NewVirtualMachineConfigV1Alpha1()
	doc.MetaName = name
	doc.CPUConfig.CPUCount = 1
	doc.MemoryConfig.MemorySize = meta.MustByteSize("1GiB")
	doc.DisksConfig = []hypervisorcfg.VirtualMachineDisk{
		{
			DiskName: "system",
			DiskPool: "pool1",
			DiskSize: meta.MustByteSize("20GiB"),
			ProvisionConfig: hypervisorcfg.VirtualMachineDiskProvision{
				BlankConfig: &hypervisorcfg.VirtualMachineDiskBlank{},
			},
		},
	}

	return doc
}

func TestVirtualMachineImageReferences(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name         string
		docs         []config.Document
		expectedErrs []string
	}{
		{
			name: "no virtual machines",
			docs: []config.Document{
				newUserVolumeDoc("vm-images"),
				newContentLibraryDoc("images", "vm-images"),
			},
		},
		{
			name: "no library referenced",
			docs: []config.Document{
				newUserVolumeDoc("vm-images"),
				newStoragePoolDoc("pool1", "vm-images"),
				newBlankDiskVirtualMachineDoc("vm1"),
			},
		},
		{
			name: "library declared",
			docs: []config.Document{
				newUserVolumeDoc("vm-images"),
				newContentLibraryDoc("images", "vm-images"),
				newVirtualMachineDoc("vm1", "images"),
			},
		},
		{
			name: "library not declared",
			docs: []config.Document{
				newVirtualMachineDoc("vm1", "images"),
			},
			expectedErrs: []string{
				`virtual machine "vm1": disks[0]: no ContentLibraryConfig declares content library "images"`,
			},
		},
		{
			name: "one disk resolves, one does not",
			docs: []config.Document{
				newUserVolumeDoc("vm-images"),
				newContentLibraryDoc("images", "vm-images"),
				newVirtualMachineDoc("vm1", "images", "nowhere"),
			},
			expectedErrs: []string{
				`virtual machine "vm1": disks[1]: no ContentLibraryConfig declares content library "nowhere"`,
			},
		},
		{
			name: "two virtual machines, same missing library",
			docs: []config.Document{
				newVirtualMachineDoc("vm1", "images"),
				newVirtualMachineDoc("vm2", "images"),
			},
			expectedErrs: []string{
				`virtual machine "vm1": disks[0]: no ContentLibraryConfig declares content library "images"`,
				`virtual machine "vm2": disks[0]: no ContentLibraryConfig declares content library "images"`,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctr, err := container.New(test.docs...)
			require.NoError(t, err)

			_, err = ctr.ValidateAsClient(validationMode{})

			if len(test.expectedErrs) == 0 {
				if err != nil {
					// The bare volume documents fail their own validation, which is not what this
					// test is about.
					assert.NotContains(t, err.Error(), "no ContentLibraryConfig declares")
					assert.NotContains(t, err.Error(), "disks[")
				}

				return
			}

			require.Error(t, err)

			for _, expectedErr := range test.expectedErrs {
				assert.ErrorContains(t, err, expectedErr)
			}
		})
	}
}

func TestVirtualMachinePoolReferences(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		docs        []config.Document
		expectedErr string
	}{
		{
			name: "pool declared",
			docs: []config.Document{
				newUserVolumeDoc("vm-images"),
				newStoragePoolDoc("pool1", "vm-images"),
				newBlankDiskVirtualMachineDoc("vm1"),
			},
		},
		{
			name: "pool not declared",
			docs: []config.Document{
				newBlankDiskVirtualMachineDoc("vm1"),
			},
			expectedErr: `virtual machine "vm1": disks[0]: no StoragePool declares storage pool "pool1"`,
		},
		{
			// A cdrom names no pool, so it must not be asked to resolve one.
			name: "cdrom needs no pool",
			docs: []config.Document{
				newUserVolumeDoc("vm-images"),
				newContentLibraryDoc("images", "vm-images"),
				newVirtualMachineDoc("vm1", "images"),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctr, err := container.New(test.docs...)
			require.NoError(t, err)

			_, err = ctr.ValidateAsClient(validationMode{})

			if test.expectedErr == "" {
				if err != nil {
					// The bare volume documents fail their own validation, which is not what this
					// test is about.
					assert.NotContains(t, err.Error(), "no StoragePool declares")
				}

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), test.expectedErr)
		})
	}
}
