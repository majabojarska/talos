// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package hypervisor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"

	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/ctest"
	hypervisorctrl "github.com/siderolabs/talos/internal/app/machined/pkg/controllers/hypervisor"
	libvirtstorage "github.com/siderolabs/talos/internal/pkg/libvirt/storage"
	"github.com/siderolabs/talos/pkg/machinery/resources/hardware"
	"github.com/siderolabs/talos/pkg/machinery/resources/hypervisor"
	"github.com/siderolabs/talos/pkg/machinery/resources/storage"
)

const (
	isoContents = "not really an ISO, but it hashes like one"

	// Every case in this suite works on one virtual machine drawing from one content library.
	vmName      = "vm"
	libraryName = "images"
	poolName    = "pool1"
)

// volumeClient records the volumes the controller asks for. It never deletes: the controller
// has no way to ask it to, and these tests assert that stays true.
type volumeClient struct {
	volumes map[string]uint64
	events  []string
	openErr error
	mu      sync.Mutex
}

func (c *volumeClient) open(context.Context) (libvirtstorage.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.openErr != nil {
		return nil, c.openErr
	}

	return c, nil
}

func (c *volumeClient) EnsureVolume(pool libvirtstorage.Pool, vol libvirtstorage.Volume) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, found := c.volumes[vol.Name]; found {
		if existing != vol.Capacity {
			return "", errors.New("resizing is not supported")
		}

		return "/pools/" + pool.Name + "/" + vol.Name, nil
	}

	if c.volumes == nil {
		c.volumes = map[string]uint64{}
	}

	c.volumes[vol.Name] = vol.Capacity
	c.events = append(c.events, "create:"+pool.Name+"/"+vol.Name)

	return "/pools/" + pool.Name + "/" + vol.Name, nil
}

func (c *volumeClient) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.events...)
}

func (c *volumeClient) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.openErr = err
}

// reset clears state between test methods, which share one client.
func (c *volumeClient) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.volumes = nil
	c.events = nil
	c.openErr = nil
}

func (c *volumeClient) Pools() ([]libvirtstorage.Pool, error)                  { return nil, nil }
func (c *volumeClient) Ensure(libvirtstorage.Pool, string, func() error) error { return nil }
func (c *volumeClient) Remove(libvirtstorage.Pool) error                       { return nil }
func (c *volumeClient) Stop(libvirtstorage.Pool) error                         { return nil }
func (c *volumeClient) Close()                                                 {}

type VirtualMachineDiskSuite struct {
	ctest.DefaultSuite

	client *volumeClient
}

func TestVirtualMachineDiskSuite(t *testing.T) {
	t.Parallel()

	client := &volumeClient{}
	s := &VirtualMachineDiskSuite{client: client}
	s.DefaultSuite = ctest.DefaultSuite{
		Timeout: 15 * time.Second,
		AfterSetup: func(suite *ctest.DefaultSuite) {
			client.reset()
			suite.Require().NoError(suite.Runtime().RegisterController(&hypervisorctrl.VirtualMachineDiskController{
				Open: client.open,
			}))
		},
	}

	suite.Run(t, s)
}

// systemInformation publishes the machine UUID the storage pool identity is derived from.
func (suite *VirtualMachineDiskSuite) systemInformation() {
	suite.T().Helper()

	info := hardware.NewSystemInformation(hardware.SystemInformationID)
	info.TypedSpec().UUID = machineUUID
	suite.Create(info)
}

// storagePool publishes a storage pool status the blank disks draw from.
func (suite *VirtualMachineDiskSuite) storagePool(ready bool) *storage.StoragePoolStatus {
	suite.T().Helper()

	suite.systemInformation()

	status := storage.NewStoragePoolStatus(storage.NamespaceName, poolName)
	*status.TypedSpec() = storage.StoragePoolStatusSpec{
		VolumeID:   "u-vms",
		TargetPath: "/var/mnt/u-vms/" + poolName,
		Ready:      ready,
	}

	if !ready {
		status.TypedSpec().Error = "volume is not mounted"
	}

	suite.Create(status)

	return status
}

// blankDiskSpec is a writable disk provisioned into the suite's pool.
func blankDiskSpec(name string, size uint64) hypervisor.VirtualMachineDiskSpec {
	return hypervisor.VirtualMachineDiskSpec{
		Name: name, Pool: poolName, Size: size, Format: "qcow2", Bus: "virtio", Type: "disk",
		Provision: hypervisor.VirtualMachineDiskProvisionSpec{Blank: true},
	}
}

// library writes an ISO into a fresh directory and publishes a ready content library over it.
func (suite *VirtualMachineDiskSuite) library() string {
	suite.T().Helper()

	path := suite.T().TempDir()
	suite.Require().NoError(os.WriteFile(filepath.Join(path, "talos.iso"), []byte(isoContents), 0o600))

	status := hypervisor.NewContentLibraryStatus(hypervisor.NamespaceName, libraryName)
	*status.TypedSpec() = hypervisor.ContentLibraryStatusSpec{
		VolumeID: "u-" + libraryName,
		Path:     path,
		Ready:    true,
	}
	suite.Create(status)

	return path
}

func (suite *VirtualMachineDiskSuite) createVM(disks ...hypervisor.VirtualMachineDiskSpec) {
	suite.T().Helper()

	spec := hypervisor.NewVirtualMachineSpec(hypervisor.NamespaceName, vmName)
	*spec.TypedSpec() = hypervisor.VirtualMachineSpecSpec{
		CPU:        hypervisor.VirtualMachineCPUSpec{Count: 1},
		Memory:     hypervisor.VirtualMachineMemorySpec{Size: 1 << 30},
		PowerState: "running",
		Firmware:   hypervisor.VirtualMachineFirmwareSpec{Type: "uefi"},
		Disks:      disks,
	}
	suite.Create(spec)
}

func cdromDiskSpec(name, library, file, dgst string) hypervisor.VirtualMachineDiskSpec {
	return hypervisor.VirtualMachineDiskSpec{
		Name: name,
		Bus:  "sata",
		Type: "cdrom",
		Provision: hypervisor.VirtualMachineDiskProvisionSpec{
			FromImage: &hypervisor.VirtualMachineDiskFromImageSpec{
				Library: library,
				File:    file,
				Digest:  dgst,
			},
		},
	}
}

func (suite *VirtualMachineDiskSuite) assertDisk(disk string, check func(hypervisor.VirtualMachineDiskStatusSpec, *assert.Assertions)) {
	suite.T().Helper()

	ctest.AssertResource(suite, hypervisor.VirtualMachineDiskStatusID(vmName, disk),
		func(res *hypervisor.VirtualMachineDiskStatus, asrt *assert.Assertions) {
			asrt.Equal(vmName, res.TypedSpec().VirtualMachine)
			asrt.Equal(disk, res.TypedSpec().Name)

			check(*res.TypedSpec(), asrt)
		})
}

func (suite *VirtualMachineDiskSuite) TestResolvesCDROMInPlace() {
	path := suite.library()
	suite.createVM(cdromDiskSpec("install", libraryName, "talos.iso", ""))

	suite.assertDisk("install", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.True(spec.Ready)
		asrt.Empty(spec.Error)
		asrt.Equal(filepath.Join(path, "talos.iso"), spec.SourcePath)
		asrt.Equal("raw", spec.Format)
		asrt.True(spec.ReadOnly)
	})
}

func (suite *VirtualMachineDiskSuite) TestVerifiesDigest() {
	suite.library()
	suite.createVM(cdromDiskSpec("install", libraryName, "talos.iso", digest.FromString(isoContents).String()))

	suite.assertDisk("install", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.True(spec.Ready)
		asrt.Empty(spec.Error)
	})
}

func (suite *VirtualMachineDiskSuite) TestRejectsDigestMismatch() {
	suite.library()
	suite.createVM(cdromDiskSpec("install", libraryName, "talos.iso", digest.FromString("something else").String()))

	suite.assertDisk("install", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.False(spec.Ready)
		asrt.Contains(spec.Error, "digest mismatch")
		asrt.Empty(spec.SourcePath)
	})
}

func (suite *VirtualMachineDiskSuite) TestReportsMissingFile() {
	suite.library()
	suite.createVM(cdromDiskSpec("install", libraryName, "absent.iso", ""))

	suite.assertDisk("install", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.False(spec.Ready)
		asrt.Contains(spec.Error, "absent.iso")
	})
}

func (suite *VirtualMachineDiskSuite) TestRejectsDirectory() {
	path := suite.library()
	suite.Require().NoError(os.Mkdir(filepath.Join(path, "nested"), 0o700))
	suite.createVM(cdromDiskSpec("install", libraryName, "nested", ""))

	suite.assertDisk("install", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.False(spec.Ready)
		asrt.Contains(spec.Error, "not a regular file")
	})
}

func (suite *VirtualMachineDiskSuite) TestReportsUnconfiguredLibrary() {
	suite.createVM(cdromDiskSpec("install", "absent", "talos.iso", ""))

	suite.assertDisk("install", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.False(spec.Ready)
		asrt.Contains(spec.Error, `content library "absent" is not configured`)
	})
}

func (suite *VirtualMachineDiskSuite) TestWaitsForLibraryToBecomeReady() {
	status := hypervisor.NewContentLibraryStatus(hypervisor.NamespaceName, libraryName)
	*status.TypedSpec() = hypervisor.ContentLibraryStatusSpec{
		VolumeID: "u-" + libraryName,
		Error:    "volume is not mounted",
	}
	suite.Create(status)
	suite.createVM(cdromDiskSpec("install", libraryName, "talos.iso", ""))

	suite.assertDisk("install", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.False(spec.Ready)
		asrt.Contains(spec.Error, "volume is not mounted")
	})

	path := suite.T().TempDir()
	suite.Require().NoError(os.WriteFile(filepath.Join(path, "talos.iso"), []byte(isoContents), 0o600))

	ctest.UpdateWithConflicts(suite, status, func(res *hypervisor.ContentLibraryStatus) error {
		res.TypedSpec().Path = path
		res.TypedSpec().Ready = true
		res.TypedSpec().Error = ""

		return nil
	})

	suite.assertDisk("install", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.True(spec.Ready)
		asrt.Equal(filepath.Join(path, "talos.iso"), spec.SourcePath)
	})
}

// A disk this slice does not provision is reported, not silently dropped: the operator sees why
// the virtual machine never gets a domain.
func (suite *VirtualMachineDiskSuite) TestReportsUnsupportedDisks() {
	suite.createVM(
		hypervisor.VirtualMachineDiskSpec{
			Name: "system", Pool: "pool1", Size: 20 << 30, Format: "qcow2", Bus: "virtio", Type: "disk",
			Provision: hypervisor.VirtualMachineDiskProvisionSpec{
				FromImage: &hypervisor.VirtualMachineDiskFromImageSpec{Library: libraryName, File: "talos.qcow2"},
			},
		},
	)

	suite.assertDisk("system", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.False(spec.Ready)
		asrt.Contains(spec.Error, "provision.fromImage is not provisioned yet")
	})
}

func (suite *VirtualMachineDiskSuite) TestRemovesStatusesWhenDisksDisappear() {
	suite.library()
	suite.createVM(
		cdromDiskSpec("install", libraryName, "talos.iso", ""),
		cdromDiskSpec("rescue", libraryName, "talos.iso", ""),
	)

	for _, disk := range []string{"install", "rescue"} {
		suite.assertDisk(disk, func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
			asrt.True(spec.Ready)
		})
	}

	spec, err := ctest.Get[*hypervisor.VirtualMachineSpec](suite, hypervisor.NewVirtualMachineSpec(hypervisor.NamespaceName, vmName).Metadata())
	suite.Require().NoError(err)

	ctest.UpdateWithConflicts(suite, spec, func(res *hypervisor.VirtualMachineSpec) error {
		res.TypedSpec().Disks = res.TypedSpec().Disks[:1]

		return nil
	})

	ctest.AssertNoResource[*hypervisor.VirtualMachineDiskStatus](suite, hypervisor.VirtualMachineDiskStatusID(vmName, "rescue"))
	suite.assertDisk("install", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.True(spec.Ready)
	})

	suite.Destroy(spec)

	ctest.AssertNoResource[*hypervisor.VirtualMachineDiskStatus](suite, hypervisor.VirtualMachineDiskStatusID(vmName, "install"))
}

func (suite *VirtualMachineDiskSuite) TestProvisionsBlankDisk() {
	suite.storagePool(true)
	suite.createVM(blankDiskSpec("system", 8<<30))

	suite.assertDisk("system", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.True(spec.Ready)
		asrt.Empty(spec.Error)
		asrt.Equal("/pools/pool1/vm.system", spec.SourcePath)
		// The cdrom path hardcodes raw; a blank disk must keep its own format.
		asrt.Equal("qcow2", spec.Format)
		asrt.False(spec.ReadOnly)
	})

	suite.Assert().Equal([]string{"create:pool1/vm.system"}, suite.client.recorded())
}

func (suite *VirtualMachineDiskSuite) TestBlankDiskWaitsForItsPool() {
	status := suite.storagePool(false)
	suite.createVM(blankDiskSpec("system", 8<<30))

	suite.assertDisk("system", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.False(spec.Ready)
		asrt.Contains(spec.Error, "volume is not mounted")
		asrt.Empty(spec.SourcePath)
	})

	suite.Assert().Empty(suite.client.recorded(), "an unready pool must not be written to")

	ctest.UpdateWithConflicts(suite, status, func(res *storage.StoragePoolStatus) error {
		res.TypedSpec().Ready = true
		res.TypedSpec().Error = ""

		return nil
	})

	suite.assertDisk("system", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.True(spec.Ready)
		asrt.Equal("/pools/pool1/vm.system", spec.SourcePath)
	})
}

func (suite *VirtualMachineDiskSuite) TestReportsUnconfiguredPool() {
	suite.systemInformation()
	suite.createVM(blankDiskSpec("system", 8<<30))

	suite.assertDisk("system", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.False(spec.Ready)
		asrt.Contains(spec.Error, `storage pool "pool1" is not configured`)
	})
}

func (suite *VirtualMachineDiskSuite) TestBlankDiskWaitsForStorageDaemon() {
	suite.client.fail(errors.New("connection refused"))
	suite.storagePool(true)
	suite.createVM(blankDiskSpec("system", 8<<30))

	suite.assertDisk("system", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.False(spec.Ready)
		asrt.Contains(spec.Error, "failed to connect to the storage daemon")
	})

	suite.client.fail(nil)

	suite.assertDisk("system", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.True(spec.Ready)
	})
}

// A removed disk loses its status but keeps its data: nothing in this controller can delete a
// volume, so re-declaring the disk must find the volume it left behind.
func (suite *VirtualMachineDiskSuite) TestRemovedBlankDiskKeepsItsVolume() {
	suite.storagePool(true)
	suite.createVM(blankDiskSpec("system", 8<<30), blankDiskSpec("data", 4<<30))

	for _, disk := range []string{"system", "data"} {
		suite.assertDisk(disk, func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
			asrt.True(spec.Ready)
		})
	}

	created := suite.client.recorded()
	suite.Require().Len(created, 2)

	spec, err := ctest.Get[*hypervisor.VirtualMachineSpec](suite, hypervisor.NewVirtualMachineSpec(hypervisor.NamespaceName, vmName).Metadata())
	suite.Require().NoError(err)

	ctest.UpdateWithConflicts(suite, spec, func(res *hypervisor.VirtualMachineSpec) error {
		res.TypedSpec().Disks = res.TypedSpec().Disks[:1]

		return nil
	})

	ctest.AssertNoResource[*hypervisor.VirtualMachineDiskStatus](suite, hypervisor.VirtualMachineDiskStatusID(vmName, "data"))
	suite.Assert().Equal(created, suite.client.recorded(), "removing a disk must not touch its volume")

	ctest.UpdateWithConflicts(suite, spec, func(res *hypervisor.VirtualMachineSpec) error {
		res.TypedSpec().Disks = append(res.TypedSpec().Disks, blankDiskSpec("data", 4<<30))

		return nil
	})

	suite.assertDisk("data", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.True(spec.Ready)
		asrt.Equal("/pools/pool1/vm.data", spec.SourcePath)
	})

	suite.Assert().Equal(created, suite.client.recorded(), "a re-added disk must reuse its volume")
}

func (suite *VirtualMachineDiskSuite) TestRejectsInvalidBlankDisks() {
	suite.storagePool(true)

	bad := blankDiskSpec("system", 8<<30)
	bad.Size = 0
	suite.createVM(bad)

	suite.assertDisk("system", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.False(spec.Ready)
		asrt.Contains(spec.Error, "non-zero size")
	})

	suite.Assert().Empty(suite.client.recorded(), "an invalid disk must not reach libvirt")
}

// One unprovisionable disk must not stall the others, across the libvirt boundary too.
func (suite *VirtualMachineDiskSuite) TestOneBadDiskDoesNotStallAnother() {
	suite.library()
	suite.storagePool(true)

	broken := blankDiskSpec("broken", 8<<30)
	broken.Pool = "absent"

	suite.createVM(cdromDiskSpec("install", libraryName, "talos.iso", ""), broken)

	suite.assertDisk("install", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.True(spec.Ready)
	})
	suite.assertDisk("broken", func(spec hypervisor.VirtualMachineDiskStatusSpec, asrt *assert.Assertions) {
		asrt.False(spec.Ready)
	})
}
