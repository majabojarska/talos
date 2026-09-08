// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"

	containersctrl "github.com/siderolabs/talos/internal/app/machined/pkg/controllers/containers"
	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/ctest"
	configcfg "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	containercfg "github.com/siderolabs/talos/pkg/machinery/config/types/container"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/containers"
)

// textFilesContainer is the container name these tests materialize trees for.
const textFilesContainer = "director"

// textFilesDocument is the TextFilesConfig document name these tests reference.
const textFilesDocument = "director-configs"

type TextFilesSuite struct {
	ctest.DefaultSuite

	baseDir string
}

func TestTextFilesSuite(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()

	suite.Run(t, &TextFilesSuite{
		baseDir: baseDir,
		DefaultSuite: ctest.DefaultSuite{
			Timeout: 15 * time.Second,
			AfterSetup: func(suite *ctest.DefaultSuite) {
				suite.Require().NoError(suite.Runtime().RegisterController(&containersctrl.TextFilesController{
					BaseDir: baseDir,
				}))
			},
		},
	})
}

// applyDocuments replaces the active machine configuration with the given documents.
func (suite *TextFilesSuite) applyDocuments(docs ...configcfg.Document) {
	cfg, err := container.New(docs...)
	suite.Require().NoError(err)

	machineConfig := config.NewMachineConfig(cfg)

	oldConfig, err := suite.State().Get(suite.Ctx(), machineConfig.Metadata())
	if err == nil {
		machineConfig.Metadata().SetVersion(oldConfig.Metadata().Version())

		suite.Require().NoError(suite.State().Update(suite.Ctx(), machineConfig))

		return
	}

	suite.Require().NoError(suite.State().Create(suite.Ctx(), machineConfig))
}

// createSpec creates a ContainerSpec declaring a textFiles mount of the given document.
func (suite *TextFilesSuite) createSpec(containerID, documentName string) {
	spec := containers.NewContainerSpec(containers.NamespaceName, containerID)
	spec.TypedSpec().Image = containers.ContainerImageSpec{Ref: "docker.io/library/nginx:latest"}
	spec.TypedSpec().Mounts = []containers.ContainerMountSpec{
		{
			Kind:          containers.MountKindTextFiles,
			TextFilesName: documentName,
			Destination:   "/etc/director",
			Options:       []string{"ro"},
		},
	}

	suite.Require().NoError(suite.State().Create(suite.Ctx(), spec))
}

// textFilesDoc builds a TextFilesConfig document with the given files.
func textFilesDoc(name string, files map[string]string) *containercfg.TextFilesConfigV1Alpha1 {
	doc := containercfg.NewTextFilesConfigV1Alpha1()
	doc.MetaName = name
	doc.Files = files

	return doc
}

// treePath is the host directory a (container, document) pair materializes into.
func (suite *TextFilesSuite) treePath(containerID, documentName string) string {
	return filepath.Join(suite.baseDir, containerID, documentName)
}

func (suite *TextFilesSuite) TestMaterializesNestedTree() {
	suite.applyDocuments(textFilesDoc(textFilesDocument, map[string]string{
		"foo.conf":       "[section-abc]\nkey1=val1\n",
		"subdir/baz.ini": "key1=val1\n",
	}))
	suite.createSpec(textFilesContainer, textFilesDocument)

	root := suite.treePath(textFilesContainer, textFilesDocument)

	var hash string

	ctest.AssertResource(suite, containers.TextFilesStatusID(textFilesContainer, textFilesDocument),
		func(status *containers.TextFilesStatus, asrt *assert.Assertions) {
			asrt.Empty(status.TypedSpec().Error)
			asrt.Equal(root, status.TypedSpec().Path)
			asrt.NotEmpty(status.TypedSpec().ContentHash)
			asrt.Equal(textFilesContainer, status.TypedSpec().ContainerID)
			asrt.Equal(textFilesDocument, status.TypedSpec().DocumentName)

			hash = status.TypedSpec().ContentHash
		})

	// A key with a slash becomes a real subdirectory, not a flattened name.
	suite.assertFile(filepath.Join(root, "foo.conf"), "[section-abc]\nkey1=val1\n")
	suite.assertFile(filepath.Join(root, "subdir", "baz.ini"), "key1=val1\n")

	info, err := os.Stat(filepath.Join(root, "subdir"))
	suite.Require().NoError(err)
	suite.Assert().True(info.IsDir())
	suite.Assert().Equal(os.FileMode(0o755), info.Mode().Perm())

	info, err = os.Stat(filepath.Join(root, "foo.conf"))
	suite.Require().NoError(err)
	suite.Assert().Equal(os.FileMode(0o644), info.Mode().Perm())

	suite.Assert().NotEmpty(hash)
}

func (suite *TextFilesSuite) TestHashChangesWithContentOnly() {
	suite.applyDocuments(textFilesDoc(textFilesDocument, map[string]string{"foo.conf": "one\n"}))
	suite.createSpec(textFilesContainer, textFilesDocument)

	statusID := containers.TextFilesStatusID(textFilesContainer, textFilesDocument)
	root := suite.treePath(textFilesContainer, textFilesDocument)

	var first string

	ctest.AssertResource(suite, statusID, func(status *containers.TextFilesStatus, asrt *assert.Assertions) {
		asrt.NotEmpty(status.TypedSpec().ContentHash)

		first = status.TypedSpec().ContentHash
	})

	// An unchanged reconcile must not rewrite the file: a running container's mount would otherwise
	// see a new mtime for content that did not change.
	before, err := os.Stat(filepath.Join(root, "foo.conf"))
	suite.Require().NoError(err)

	suite.applyDocuments(textFilesDoc(textFilesDocument, map[string]string{"foo.conf": "one\n"}))

	ctest.AssertResource(suite, statusID, func(status *containers.TextFilesStatus, asrt *assert.Assertions) {
		asrt.Equal(first, status.TypedSpec().ContentHash)
	})

	after, err := os.Stat(filepath.Join(root, "foo.conf"))
	suite.Require().NoError(err)
	suite.Assert().Equal(before.ModTime(), after.ModTime())

	// Editing the content changes the hash, which is what makes the container restart.
	suite.applyDocuments(textFilesDoc(textFilesDocument, map[string]string{"foo.conf": "two\n"}))

	ctest.AssertResource(suite, statusID, func(status *containers.TextFilesStatus, asrt *assert.Assertions) {
		asrt.NotEqual(first, status.TypedSpec().ContentHash)
	})

	suite.assertFile(filepath.Join(root, "foo.conf"), "two\n")

	// The update is write-temp-then-rename, not truncate-in-place: no leftover temp file should
	// remain in the tree, and the directory should hold exactly the one file.
	entries, err := os.ReadDir(root)
	suite.Require().NoError(err)
	suite.Require().Len(entries, 1)
	suite.Assert().Equal("foo.conf", entries[0].Name())
}

func (suite *TextFilesSuite) TestRemovesStaleFiles() {
	suite.applyDocuments(textFilesDoc(textFilesDocument, map[string]string{
		"keep.conf":       "keep\n",
		"drop.conf":       "drop\n",
		"subdir/drop.ini": "drop\n",
	}))
	suite.createSpec(textFilesContainer, textFilesDocument)

	root := suite.treePath(textFilesContainer, textFilesDocument)

	ctest.AssertResource(suite, containers.TextFilesStatusID(textFilesContainer, textFilesDocument),
		func(status *containers.TextFilesStatus, asrt *assert.Assertions) {
			asrt.Empty(status.TypedSpec().Error)
		})

	suite.assertFile(filepath.Join(root, "drop.conf"), "drop\n")

	suite.applyDocuments(textFilesDoc(textFilesDocument, map[string]string{"keep.conf": "keep\n"}))

	// The removed file and the now-empty subdirectory both have to go: the tree is the document, not
	// an accumulation of everything the document ever held.
	suite.Assert().EventuallyWithT(func(collect *assert.CollectT) {
		_, err := os.Stat(filepath.Join(root, "drop.conf"))
		assert.True(collect, os.IsNotExist(err), "drop.conf should be removed")

		_, err = os.Stat(filepath.Join(root, "subdir"))
		assert.True(collect, os.IsNotExist(err), "subdir should be removed")
	}, 5*time.Second, 50*time.Millisecond)

	suite.assertFile(filepath.Join(root, "keep.conf"), "keep\n")
}

func (suite *TextFilesSuite) TestRemovesTreeWhenContainerGoesAway() {
	suite.applyDocuments(textFilesDoc(textFilesDocument, map[string]string{"foo.conf": "x\n"}))
	suite.createSpec(textFilesContainer, textFilesDocument)

	containerDir := filepath.Join(suite.baseDir, textFilesContainer)

	ctest.AssertResource(suite, containers.TextFilesStatusID(textFilesContainer, textFilesDocument),
		func(status *containers.TextFilesStatus, asrt *assert.Assertions) {
			asrt.Empty(status.TypedSpec().Error)
		})

	suite.Require().NoError(suite.State().Destroy(suite.Ctx(),
		containers.NewContainerSpec(containers.NamespaceName, textFilesContainer).Metadata()))

	ctest.AssertNoResource[*containers.TextFilesStatus](suite,
		containers.TextFilesStatusID(textFilesContainer, textFilesDocument))

	suite.Assert().EventuallyWithT(func(collect *assert.CollectT) {
		_, err := os.Stat(containerDir)
		assert.True(collect, os.IsNotExist(err), "the container's whole directory should be removed")
	}, 5*time.Second, 50*time.Millisecond)
}

func (suite *TextFilesSuite) TestPerContainerCopies() {
	suite.applyDocuments(textFilesDoc(textFilesDocument, map[string]string{"foo.conf": "x\n"}))
	suite.createSpec("first", textFilesDocument)
	suite.createSpec("second", textFilesDocument)

	// Two containers mounting one document get two trees, so neither can be affected by the other's
	// lifecycle.
	for _, containerID := range []string{"first", "second"} {
		root := suite.treePath(containerID, textFilesDocument)

		ctest.AssertResource(suite, containers.TextFilesStatusID(containerID, textFilesDocument),
			func(status *containers.TextFilesStatus, asrt *assert.Assertions) {
				asrt.Equal(root, status.TypedSpec().Path)
			})

		suite.assertFile(filepath.Join(root, "foo.conf"), "x\n")
	}
}

func (suite *TextFilesSuite) TestReportsMissingDocument() {
	suite.applyDocuments(textFilesDoc("some-other-configs", map[string]string{"foo.conf": "x\n"}))
	suite.createSpec(textFilesContainer, textFilesDocument)

	// A missing document is reported on the status rather than failing the controller: the container
	// then simply waits, exactly as it would for a volume that has not appeared yet.
	ctest.AssertResource(suite, containers.TextFilesStatusID(textFilesContainer, textFilesDocument),
		func(status *containers.TextFilesStatus, asrt *assert.Assertions) {
			asrt.Contains(status.TypedSpec().Error, `text files "director-configs" is not configured`)
			asrt.Empty(status.TypedSpec().Path)
			asrt.Empty(status.TypedSpec().ContentHash)
		})

	// Appearing later resolves it, with no restart of the controller involved.
	suite.applyDocuments(
		textFilesDoc("some-other-configs", map[string]string{"foo.conf": "x\n"}),
		textFilesDoc(textFilesDocument, map[string]string{"foo.conf": "y\n"}),
	)

	ctest.AssertResource(suite, containers.TextFilesStatusID(textFilesContainer, textFilesDocument),
		func(status *containers.TextFilesStatus, asrt *assert.Assertions) {
			asrt.Empty(status.TypedSpec().Error)
			asrt.NotEmpty(status.TypedSpec().ContentHash)
		})
}

// assertFile asserts a file exists with the expected contents.
func (suite *TextFilesSuite) assertFile(path, expected string) {
	suite.T().Helper()

	suite.Assert().EventuallyWithT(func(collect *assert.CollectT) {
		contents, err := os.ReadFile(path)
		if !assert.NoError(collect, err) {
			return
		}

		assert.Equal(collect, expected, string(contents))
	}, 5*time.Second, 50*time.Millisecond)
}
