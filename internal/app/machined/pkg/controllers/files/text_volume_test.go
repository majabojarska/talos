// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package files_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"golang.org/x/sys/unix"

	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/ctest"
	filesctrl "github.com/siderolabs/talos/internal/app/machined/pkg/controllers/files"
	configconfig "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	blockcfg "github.com/siderolabs/talos/pkg/machinery/config/types/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/files"
)

type TextVolumeSuite struct {
	ctest.DefaultSuite
}

func newTextVolumeDoc(name string, files map[string]string) *blockcfg.TextVolumeConfigV1Alpha1 {
	doc := blockcfg.NewTextVolumeConfigV1Alpha1()
	doc.MetaName = name
	doc.FilesConfig = map[string]blockcfg.TextVolumeFileV1Alpha1{}

	for path, contents := range files {
		doc.FilesConfig[path] = blockcfg.TextVolumeFileV1Alpha1{FileContents: contents}
	}

	return doc
}

func (suite *TextVolumeSuite) setConfig(docs ...configconfig.Document) {
	cfg, err := container.New(docs...)
	suite.Require().NoError(err)

	machineConfig := config.NewMachineConfig(cfg)

	current, err := safe.StateGetByID[*config.MachineConfig](suite.Ctx(), suite.State(), config.ActiveID)
	if err != nil {
		suite.Require().True(state.IsNotFoundError(err))
		suite.Create(machineConfig)

		return
	}

	machineConfig.Metadata().SetVersion(current.Metadata().Version())
	suite.Update(machineConfig)
}

func (suite *TextVolumeSuite) mount(volumeID, target string) *block.VolumeMountStatus {
	status := block.NewVolumeMountStatus(block.NamespaceName, volumeID)
	status.TypedSpec().VolumeID = volumeID
	status.TypedSpec().Target = target
	suite.Create(status)

	return status
}

func (suite *TextVolumeSuite) TestRender() {
	target := suite.T().TempDir()

	suite.setConfig(newTextVolumeDoc("app", map[string]string{"a.conf": "a1", "sub/b.conf": "b1"}))

	// not mounted yet
	ctest.AssertNoResource[*files.TextVolumeStatus](suite, "t-app")

	status := suite.mount("t-app", target)

	ctest.AssertResource(suite, "t-app", func(r *files.TextVolumeStatus, asrt *assert.Assertions) {
		asrt.Equal("app", r.TypedSpec().VolumeName)
		asrt.Equal(target, r.TypedSpec().Target)
		asrt.Equal(2, r.TypedSpec().Files)
		asrt.Equal(filesctrl.TextVolumeContentsHash([]configconfig.TextVolumeFile{
			{Path: "a.conf", Mode: 0o644, Contents: "a1"},
			{Path: "sub/b.conf", Mode: 0o644, Contents: "b1"},
		}), r.TypedSpec().ContentsHash)
	})

	assertFile(suite.T(), target, "a.conf", "a1", 0o644)
	assertFile(suite.T(), target, "sub/b.conf", "b1", 0o644)

	// update in place
	suite.setConfig(newTextVolumeDoc("app", map[string]string{"a.conf": "a2"}))

	ctest.AssertResource(suite, "t-app", func(r *files.TextVolumeStatus, asrt *assert.Assertions) {
		asrt.Equal(1, r.TypedSpec().Files)
	})

	assertFile(suite.T(), target, "a.conf", "a2", 0o644)
	suite.Assert().NoFileExists(filepath.Join(target, "sub", "b.conf"))

	// unmount: the status goes away
	_, err := suite.State().Teardown(suite.Ctx(), status.Metadata())
	suite.Require().NoError(err)

	ctest.AssertNoResource[*files.TextVolumeStatus](suite, "t-app")

	suite.Destroy(status)

	// mounted again on an empty tmpfs: rendered again
	target2 := suite.T().TempDir()
	suite.mount("t-app", target2)

	ctest.AssertResource(suite, "t-app", func(r *files.TextVolumeStatus, asrt *assert.Assertions) {
		asrt.Equal(target2, r.TypedSpec().Target)
	})

	assertFile(suite.T(), target2, "a.conf", "a2", 0o644)

	// config removed: the status goes away, the files are left to the unmount
	suite.setConfig()

	ctest.AssertNoResource[*files.TextVolumeStatus](suite, "t-app")

	_, err = os.Stat(filepath.Join(target2, "a.conf"))
	suite.Require().NoError(err)
}

func (suite *TextVolumeSuite) TestMultipleVolumes() {
	target1, target2 := suite.T().TempDir(), suite.T().TempDir()

	suite.setConfig(
		newTextVolumeDoc("one", map[string]string{"one.conf": "1"}),
		newTextVolumeDoc("two", map[string]string{"two.conf": "2"}),
	)

	suite.mount("t-one", target1)
	suite.mount("t-two", target2)

	ctest.AssertResources(suite, []string{"t-one", "t-two"}, func(r *files.TextVolumeStatus, asrt *assert.Assertions) {
		asrt.Equal(1, r.TypedSpec().Files)
	})

	assertFile(suite.T(), target1, "one.conf", "1", 0o644)
	assertFile(suite.T(), target2, "two.conf", "2", 0o644)

	// removing one volume leaves the other one alone
	suite.setConfig(newTextVolumeDoc("two", map[string]string{"two.conf": "2"}))

	ctest.AssertNoResource[*files.TextVolumeStatus](suite, "t-one")
	ctest.AssertResource(suite, "t-two", func(r *files.TextVolumeStatus, asrt *assert.Assertions) {
		asrt.Equal(target2, r.TypedSpec().Target)
	})
}

func (suite *TextVolumeSuite) TestOtherMountsIgnored() {
	target, otherTarget := suite.T().TempDir(), suite.T().TempDir()

	suite.setConfig(
		newTextVolumeDoc("app", map[string]string{"a.conf": "a"}),
		newTextVolumeDoc("other", map[string]string{"o.conf": "o"}),
	)

	// a mount of the volume requested by someone else (e.g. a container) is not the volume's own
	suite.mount("containers.MountController/app/t-app", target)
	// a user volume with the same name is not a text volume
	suite.mount("u-app", target)

	// once another volume is rendered, the mounts above have been seen by the controller
	suite.mount("t-other", otherTarget)

	ctest.AssertResource(suite, "t-other", func(*files.TextVolumeStatus, *assert.Assertions) {})

	ctest.AssertNoResource[*files.TextVolumeStatus](suite, "t-app")
	suite.Assert().NoFileExists(filepath.Join(target, "a.conf"))

	suite.mount("t-app", target)

	ctest.AssertResource(suite, "t-app", func(*files.TextVolumeStatus, *assert.Assertions) {})
}

func TestCheckTmpfs(t *testing.T) {
	t.Parallel()

	checkDir := func(dir string) error {
		root, err := os.OpenRoot(dir)
		require.NoError(t, err)

		defer root.Close() //nolint:errcheck

		return filesctrl.CheckTmpfs(root)
	}

	var statfs unix.Statfs_t

	require.NoError(t, unix.Statfs("/dev/shm", &statfs))

	if statfs.Type == unix.TMPFS_MAGIC {
		require.NoError(t, checkDir("/dev/shm"))
	}

	// the current directory is in the source tree, which is never on a tmpfs in practice
	require.NoError(t, unix.Statfs(".", &statfs))

	if statfs.Type == unix.TMPFS_MAGIC {
		t.Skip("the source tree is on a tmpfs")
	}

	require.ErrorContains(t, checkDir("."), "volume root is not a tmpfs")
}

func TestTextVolumeSuite(t *testing.T) {
	t.Parallel()

	suite.Run(t, &TextVolumeSuite{
		Timeout: 10 * time.Second,
		AfterSetup: func(suite *ctest.DefaultSuite) {
			suite.Require().NoError(suite.Runtime().RegisterController(&filesctrl.TextVolumeController{SkipFilesystemCheck: true}))
		},
	})
}
