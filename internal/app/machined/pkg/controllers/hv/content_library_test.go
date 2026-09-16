// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package hv_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/siderolabs/gen/xslices"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"

	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/ctest"
	hvctrl "github.com/siderolabs/talos/internal/app/machined/pkg/controllers/hv"
	configcfg "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	hvcfg "github.com/siderolabs/talos/pkg/machinery/config/types/hv"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/hv"
)

const (
	// testLibrary is the content library name the tests use.
	testLibrary = "vm-images"
	// testVolumeID is the volume backing it.
	testVolumeID = "u-vm-images"
)

// contentLibraryControllerName is the controller's own name, used as both requester and finalizer.
const contentLibraryControllerName = "hv.ContentLibraryController"

// testRequestID mirrors the controller's naming so tests can find what it creates.
var testRequestID = contentLibraryControllerName + "/" + testLibrary

type ContentLibrarySuite struct {
	ctest.DefaultSuite

	// mountTarget stands in for the path the block subsystem mounts the volume at.
	mountTarget string
}

func TestContentLibrarySuite(t *testing.T) {
	t.Parallel()

	suite.Run(t, &ContentLibrarySuite{
		DefaultSuite: ctest.DefaultSuite{
			Timeout: 15 * time.Second,
			AfterSetup: func(suite *ctest.DefaultSuite) {
				suite.Require().NoError(suite.Runtime().RegisterController(&hvctrl.ContentLibraryController{}))
			},
		},
	})
}

func (suite *ContentLibrarySuite) SetupTest() {
	suite.DefaultSuite.SetupTest()

	suite.mountTarget = suite.T().TempDir()
}

// applyLibraries puts the given ContentLibraryConfig documents into the active machine config.
func (suite *ContentLibrarySuite) applyLibraries(docs ...*hvcfg.ContentLibraryConfigV1Alpha1) {
	cfg, err := container.New(xslices.Map(docs, func(doc *hvcfg.ContentLibraryConfigV1Alpha1) configcfg.Document { return doc })...)
	suite.Require().NoError(err)

	suite.Require().NoError(suite.State().Create(suite.Ctx(), config.NewMachineConfig(cfg)))
}

func newDoc(name string) *hvcfg.ContentLibraryConfigV1Alpha1 {
	doc := hvcfg.NewContentLibraryConfigV1Alpha1()
	doc.MetaName = name
	doc.BackingConfig.VolumeID = testVolumeID

	return doc
}

// createVolumeStatus stands in for the block subsystem having provisioned the backing volume.
func (suite *ContentLibrarySuite) createVolumeStatus(labels ...string) {
	volumeStatus := block.NewVolumeStatus(block.NamespaceName, testVolumeID)
	volumeStatus.TypedSpec().Phase = block.VolumePhaseReady

	for _, label := range labels {
		volumeStatus.Metadata().Labels().Set(label, "")
	}

	suite.Require().NoError(suite.State().Create(suite.Ctx(), volumeStatus))
}

// satisfyMount creates the VolumeMountStatus the block subsystem would produce for the request.
func (suite *ContentLibrarySuite) satisfyMount(readOnly bool) {
	status := block.NewVolumeMountStatus(block.NamespaceName, testRequestID)
	status.TypedSpec().VolumeID = testVolumeID
	status.TypedSpec().Requester = contentLibraryControllerName
	status.TypedSpec().Target = suite.mountTarget
	status.TypedSpec().ReadOnly = readOnly

	suite.Require().NoError(suite.State().Create(suite.Ctx(), status))
}

func (suite *ContentLibrarySuite) assertNotReady(expectedError string) {
	ctest.AssertResource(suite, testLibrary, func(status *hv.ContentLibraryStatus, asrt *assert.Assertions) {
		asrt.False(status.TypedSpec().Ready)
		asrt.Equal(expectedError, status.TypedSpec().Error)
		asrt.Empty(status.TypedSpec().Path)
	})
}

// TestReady covers the whole path: a configured library on a mounted volume ends up with a
// directory of its own.
func (suite *ContentLibrarySuite) TestReady() {
	suite.applyLibraries(newDoc(testLibrary))
	suite.createVolumeStatus()

	// The mount request is what the block subsystem acts on, so it has to be right before anything
	// can be mounted.
	ctest.AssertResource(suite, testRequestID, func(request *block.VolumeMountRequest, asrt *assert.Assertions) {
		asrt.Equal(testVolumeID, request.TypedSpec().VolumeID)
		asrt.Equal(contentLibraryControllerName, request.TypedSpec().Requester)
		asrt.False(request.TypedSpec().ReadOnly)
		asrt.False(request.TypedSpec().Detached)
		asrt.True(request.TypedSpec().Secure)
		asrt.True(request.TypedSpec().NoExec)
	})

	suite.assertNotReady(`waiting for volume "u-vm-images" to be mounted`)

	suite.satisfyMount(false)

	expectedPath := filepath.Join(suite.mountTarget, constants.ContentLibraryDirectory, testLibrary)

	ctest.AssertResource(suite, testLibrary, func(status *hv.ContentLibraryStatus, asrt *assert.Assertions) {
		asrt.True(status.TypedSpec().Ready, "error: %q", status.TypedSpec().Error)
		asrt.Equal(testVolumeID, status.TypedSpec().VolumeID)
		asrt.Equal(expectedPath, status.TypedSpec().Path)
		asrt.Empty(status.TypedSpec().Error)
	})

	// The directory is the library: without it there is nowhere to upload to.
	info, err := os.Stat(expectedPath)
	suite.Require().NoError(err)
	suite.Assert().True(info.IsDir())

	// The finalizer is what stops the volume being unmounted while the library is in use.
	ctest.AssertResource(suite, testRequestID, func(status *block.VolumeMountStatus, asrt *assert.Assertions) {
		asrt.True(status.Metadata().Finalizers().Has(contentLibraryControllerName))
	})
}

// TestSweepsStagedUploads covers an upload interrupted by a node going down: what it staged must be
// gone by the time the library is usable again, or the name it was taking would stay blocked.
func (suite *ContentLibrarySuite) TestSweepsStagedUploads() {
	libraryDir := filepath.Join(suite.mountTarget, constants.ContentLibraryDirectory, testLibrary)
	suite.Require().NoError(os.MkdirAll(libraryDir, 0o700))

	staged := filepath.Join(libraryDir, ".image.raw.0123456789abcdef"+constants.ContentLibraryInflightUploadSuffix)
	suite.Require().NoError(os.WriteFile(staged, []byte("partial"), 0o600))

	// A dot-prefixed file which is not a staged upload is none of the controller's business.
	unrelated := filepath.Join(libraryDir, ".keep")
	suite.Require().NoError(os.WriteFile(unrelated, nil, 0o600))

	suite.applyLibraries(newDoc(testLibrary))
	suite.createVolumeStatus()
	suite.satisfyMount(false)

	ctest.AssertResource(suite, testLibrary, func(status *hv.ContentLibraryStatus, asrt *assert.Assertions) {
		asrt.True(status.TypedSpec().Ready, "error: %q", status.TypedSpec().Error)
	})

	_, err := os.Stat(staged)
	suite.Assert().ErrorIs(err, os.ErrNotExist)

	_, err = os.Stat(unrelated)
	suite.Assert().NoError(err)
}

// TestSharedVolume covers two libraries on one volume: each gets its own directory and its own
// hold, so neither can disturb the other.
func (suite *ContentLibrarySuite) TestSharedVolume() {
	const otherLibrary = "iso-images"

	suite.applyLibraries(newDoc(testLibrary), newDoc(otherLibrary))
	suite.createVolumeStatus()
	suite.satisfyMount(false)

	otherRequestID := contentLibraryControllerName + "/" + otherLibrary

	otherStatus := block.NewVolumeMountStatus(block.NamespaceName, otherRequestID)
	otherStatus.TypedSpec().VolumeID = testVolumeID
	otherStatus.TypedSpec().Requester = contentLibraryControllerName
	otherStatus.TypedSpec().Target = suite.mountTarget

	suite.Require().NoError(suite.State().Create(suite.Ctx(), otherStatus))

	for _, libraryID := range []string{testLibrary, otherLibrary} {
		ctest.AssertResource(suite, libraryID, func(status *hv.ContentLibraryStatus, asrt *assert.Assertions) {
			asrt.True(status.TypedSpec().Ready, "error: %q", status.TypedSpec().Error)
			asrt.Equal(filepath.Join(suite.mountTarget, constants.ContentLibraryDirectory, libraryID), status.TypedSpec().Path)
		})
	}
}

// TestMissingVolume covers a library pointing at a volume which was never configured.
func (suite *ContentLibrarySuite) TestMissingVolume() {
	suite.applyLibraries(newDoc(testLibrary))

	suite.assertNotReady(`volume "u-vm-images" is not configured`)

	// Nothing may be requested from the block subsystem for a volume that does not exist.
	ctest.AssertNoResource[*block.VolumeMountRequest](suite, testRequestID)
}

// TestSystemVolume covers the config-time prefix check being bypassed: the volume ID looks like a
// user volume, but the volume turned out to be a system one.
func (suite *ContentLibrarySuite) TestSystemVolume() {
	suite.applyLibraries(newDoc(testLibrary))
	suite.createVolumeStatus(block.SystemVolumeLabel)

	suite.assertNotReady(`volume "u-vm-images" is a system volume`)

	ctest.AssertNoResource[*block.VolumeMountRequest](suite, testRequestID)
}

// TestReadOnlyMount covers another requester having got the volume mounted read-only: a library
// which cannot be written to is not usable.
func (suite *ContentLibrarySuite) TestReadOnlyMount() {
	suite.applyLibraries(newDoc(testLibrary))
	suite.createVolumeStatus()
	suite.satisfyMount(true)

	suite.assertNotReady(`volume "u-vm-images" is mounted read-only`)
}

// TestRemovedFromConfig covers a library being removed: the status goes, and so does the hold on
// the volume.
func (suite *ContentLibrarySuite) TestRemovedFromConfig() {
	suite.applyLibraries(newDoc(testLibrary))
	suite.createVolumeStatus()
	suite.satisfyMount(false)

	ctest.AssertResource(suite, testLibrary, func(status *hv.ContentLibraryStatus, asrt *assert.Assertions) {
		asrt.True(status.TypedSpec().Ready, "error: %q", status.TypedSpec().Error)
	})

	suite.Require().NoError(suite.State().Destroy(suite.Ctx(), config.NewMachineConfig(nil).Metadata()))

	ctest.AssertNoResource[*hv.ContentLibraryStatus](suite, testLibrary)

	ctest.AssertResource(suite, testRequestID, func(status *block.VolumeMountStatus, asrt *assert.Assertions) {
		asrt.False(status.Metadata().Finalizers().Has(contentLibraryControllerName))
	})

	// The request goes with it, so the block subsystem unmounts the volume.
	ctest.AssertNoResource[*block.VolumeMountRequest](suite, testRequestID)
}

// TestMountTearingDown covers the volume being unmounted under a live library: the hold has to go
// immediately, or the unmount would never finish.
func (suite *ContentLibrarySuite) TestMountTearingDown() {
	suite.applyLibraries(newDoc(testLibrary))
	suite.createVolumeStatus()
	suite.satisfyMount(false)

	ctest.AssertResource(suite, testRequestID, func(status *block.VolumeMountStatus, asrt *assert.Assertions) {
		asrt.True(status.Metadata().Finalizers().Has(contentLibraryControllerName))
	})

	_, err := suite.State().Teardown(suite.Ctx(), block.NewVolumeMountStatus(block.NamespaceName, testRequestID).Metadata())
	suite.Require().NoError(err)

	suite.assertNotReady(`volume "u-vm-images" is being unmounted`)

	ctest.AssertResource(suite, testRequestID, func(status *block.VolumeMountStatus, asrt *assert.Assertions) {
		asrt.False(status.Metadata().Finalizers().Has(contentLibraryControllerName))
	})
}
