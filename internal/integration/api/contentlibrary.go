// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

//go:build integration_api

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource/rtestutils"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/siderolabs/talos/internal/integration/base"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/cel"
	"github.com/siderolabs/talos/pkg/machinery/cel/celenv"
	"github.com/siderolabs/talos/pkg/machinery/client"
	blockcfg "github.com/siderolabs/talos/pkg/machinery/config/types/block"
	hvcfg "github.com/siderolabs/talos/pkg/machinery/config/types/hv"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/hv"
)

// ContentLibrarySuite verifies the content library: the ContentLibraryConfig document, the
// ContentLibraryStatus resource and the machine.ContentLibraryService API.
type ContentLibrarySuite struct {
	base.APISuite

	ctx       context.Context //nolint:containedctx
	ctxCancel context.CancelFunc
}

// SuiteName ...
func (suite *ContentLibrarySuite) SuiteName() string {
	return "api.ContentLibrarySuite"
}

// SetupTest ...
func (suite *ContentLibrarySuite) SetupTest() {
	if !suite.Capabilities().SupportsVolumes {
		suite.T().Skip("cluster doesn't support volumes")
	}

	suite.ctx, suite.ctxCancel = context.WithTimeout(context.Background(), 5*time.Minute)
}

// TearDownTest ...
func (suite *ContentLibrarySuite) TearDownTest() {
	if suite.ctxCancel != nil {
		suite.ctxCancel()
	}
}

// TestContentLibrary covers the whole feature end to end: a library declared on a user volume
// becomes ready, and its contents can be uploaded, listed and deleted over the API.
//
//nolint:gocyclo
func (suite *ContentLibrarySuite) TestContentLibrary() {
	if testing.Short() {
		suite.T().Skip("skipping test in short mode.")
	}

	if suite.Cluster == nil || suite.Cluster.Provisioner() != base.ProvisionerQEMU {
		suite.T().Skip("skipping test for non-qemu provisioner")
	}

	node := suite.RandomDiscoveredNodeInternalIP()

	userDisks := suite.UserDisks(suite.ctx, node)

	if len(userDisks) < 1 {
		suite.T().Skipf("skipping test, not enough user disks available on node %s: %q", node, userDisks)
	}

	ctx := client.WithNode(suite.ctx, node)

	disk, err := safe.StateGetByID[*block.Disk](ctx, suite.Client.COSI, filepath.Base(userDisks[0]))
	suite.Require().NoError(err)

	// Randomized so repeated runs against the same cluster don't collide on a leftover volume.
	name := fmt.Sprintf("cl-%04x", rand.Int31())
	volumeID := constants.UserVolumePrefix + name

	suite.T().Logf("testing the content library %q on node %s with disk %s", name, node, userDisks[0])

	volumeDoc := blockcfg.NewUserVolumeConfigV1Alpha1()
	volumeDoc.MetaName = name
	volumeDoc.ProvisioningSpec.DiskSelectorSpec.Match = cel.MustExpression(
		cel.ParseBooleanExpression(fmt.Sprintf("'%s' in disk.symlinks", disk.TypedSpec().Symlinks[0]), celenv.DiskLocator()),
	)
	volumeDoc.ProvisioningSpec.ProvisioningMinSize = blockcfg.MustByteSize("100MiB")
	volumeDoc.ProvisioningSpec.ProvisioningMaxSize = blockcfg.MustSize("1GiB")

	libraryDoc := hvcfg.NewContentLibraryConfigV1Alpha1()
	libraryDoc.MetaName = name
	libraryDoc.BackingConfig.VolumeID = volumeID

	suite.PatchMachineConfig(ctx, volumeDoc, libraryDoc)

	defer func() {
		suite.RemoveMachineConfigDocumentsByName(client.WithNode(suite.ctx, node), hvcfg.ContentLibraryConfigKind, name)
		suite.RemoveMachineConfigDocumentsByName(client.WithNode(suite.ctx, node), blockcfg.UserVolumeConfigKind, name)

		rtestutils.AssertNoResource[*hv.ContentLibraryStatus](client.WithNode(suite.ctx, node), suite.T(), suite.Client.COSI, name)
	}()

	rtestutils.AssertResources(ctx, suite.T(), suite.Client.COSI, []string{volumeID},
		func(vs *block.VolumeStatus, asrt *assert.Assertions) {
			asrt.Equal(block.VolumePhaseReady, vs.TypedSpec().Phase)
		},
	)

	expectedPath := filepath.Join(constants.UserVolumeMountPoint, name, constants.ContentLibraryDirectory, name)

	rtestutils.AssertResources(ctx, suite.T(), suite.Client.COSI, []string{name},
		func(cls *hv.ContentLibraryStatus, asrt *assert.Assertions) {
			asrt.True(cls.TypedSpec().Ready, "error: %q", cls.TypedSpec().Error)
			asrt.Equal(volumeID, cls.TypedSpec().VolumeID)
			asrt.Equal(expectedPath, cls.TypedSpec().Path)
		},
	)

	// An empty library lists nothing, rather than failing.
	suite.Assert().Empty(suite.list(ctx, name))

	contents := bytes.Repeat([]byte("talos"), 1024)

	resp, err := suite.Client.ContentLibraryUpload(ctx, name, "image.raw", false, bytes.NewReader(contents))
	suite.Require().NoError(err)
	suite.Assert().Equal("image.raw", resp.GetName())
	suite.Assert().Equal(uint64(len(contents)), resp.GetSize())

	files := suite.list(ctx, name)
	suite.Require().Len(files, 1)
	suite.Assert().Equal("image.raw", files[0].GetName())
	suite.Assert().Equal(uint64(len(contents)), files[0].GetSize())

	// The file really is on the volume, under the path the status reports.
	suite.Assert().Equal(string(contents), suite.ReadFile(ctx, filepath.Join(expectedPath, "image.raw")))

	// An upload which would replace an existing file is refused unless it says so.
	_, err = suite.Client.ContentLibraryUpload(ctx, name, "image.raw", false, bytes.NewReader(contents))
	suite.Assert().Equal(codes.AlreadyExists, status.Code(err))

	replacement := bytes.Repeat([]byte("sidero"), 512)

	_, err = suite.Client.ContentLibraryUpload(ctx, name, "image.raw", true, bytes.NewReader(replacement))
	suite.Require().NoError(err)

	suite.Assert().Equal(string(replacement), suite.ReadFile(ctx, filepath.Join(expectedPath, "image.raw")))

	// Names which would escape the library are rejected, not created.
	for _, badName := range []string{"../escape.raw", "/etc/passwd", "sub/dir.raw", ".hidden", ""} {
		_, err = suite.Client.ContentLibraryUpload(ctx, name, badName, true, bytes.NewReader(contents))
		suite.Assert().Equalf(codes.InvalidArgument, status.Code(err), "uploading %q should have been rejected", badName)

		_, err = suite.Client.ContentLibraryClient.Delete(ctx, &machineapi.ContentLibraryServiceDeleteRequest{
			LibraryId: name,
			Name:      badName,
		})
		suite.Assert().Equalf(codes.InvalidArgument, status.Code(err), "deleting %q should have been rejected", badName)
	}

	// A library which isn't configured is not found, rather than a path on the node.
	_, err = suite.Client.ContentLibraryClient.Delete(ctx, &machineapi.ContentLibraryServiceDeleteRequest{
		LibraryId: name + "-nope",
		Name:      "image.raw",
	})
	suite.Assert().Equal(codes.NotFound, status.Code(err))

	_, err = suite.Client.ContentLibraryClient.Delete(ctx, &machineapi.ContentLibraryServiceDeleteRequest{
		LibraryId: name,
		Name:      "image.raw",
	})
	suite.Require().NoError(err)

	suite.Assert().Empty(suite.list(ctx, name))

	// Deleting what is no longer there is an error, not a silent success.
	_, err = suite.Client.ContentLibraryClient.Delete(ctx, &machineapi.ContentLibraryServiceDeleteRequest{
		LibraryId: name,
		Name:      "image.raw",
	})
	suite.Assert().Equal(codes.NotFound, status.Code(err))
}

// list returns the contents of a content library on a single node.
func (suite *ContentLibrarySuite) list(nodeCtx context.Context, libraryID string) []*machineapi.ContentLibraryServiceListResponse {
	cli, err := suite.Client.ContentLibraryClient.List(nodeCtx, &machineapi.ContentLibraryServiceListRequest{
		LibraryId: libraryID,
	})
	suite.Require().NoError(err)

	var files []*machineapi.ContentLibraryServiceListResponse

	for {
		resp, err := cli.Recv()
		if errors.Is(err, io.EOF) {
			break
		}

		suite.Require().NoError(err)

		files = append(files, resp)
	}

	return files
}

func init() {
	allSuites = append(allSuites, new(ContentLibrarySuite))
}
