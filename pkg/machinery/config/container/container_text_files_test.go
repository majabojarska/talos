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
	containercfg "github.com/siderolabs/talos/pkg/machinery/config/types/container"
)

// newTextFilesDoc builds a minimal TextFilesConfig document.
func newTextFilesDoc(name string) *containercfg.TextFilesConfigV1Alpha1 {
	doc := containercfg.NewTextFilesConfigV1Alpha1()
	doc.MetaName = name
	doc.Files = map[string]string{"foo.conf": "key=value\n"}

	return doc
}

// newContainerWithTextFiles builds a ContainerConfig mounting the named text file sets.
func newContainerWithTextFiles(name string, textFiles ...string) *containercfg.ContainerConfigV1Alpha1 {
	doc := newContainerDoc(name)

	for _, textFilesName := range textFiles {
		doc.MountsConfig = append(doc.MountsConfig, containercfg.ContainerMount{
			TextFilesMount: &containercfg.TextFilesMount{
				SourceName:       textFilesName,
				MountDestination: "/etc/" + textFilesName,
			},
		})
	}

	return doc
}

func validateMixedDocs(t *testing.T, docs ...config.Document) error {
	t.Helper()

	ctr, err := container.New(docs...)
	require.NoError(t, err)

	_, err = ctr.ValidateAsClient(validationMode{})

	return err
}

func TestContainerTextFilesReferences(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		docs        []config.Document
		expectedErr string
	}{
		{
			name: "resolved reference",
			docs: []config.Document{
				newTextFilesDoc("my-configs"),
				newContainerWithTextFiles("a", "my-configs"),
			},
		},
		{
			name: "one document shared by two containers",
			docs: []config.Document{
				newTextFilesDoc("my-configs"),
				newContainerWithTextFiles("a", "my-configs"),
				newContainerWithTextFiles("b", "my-configs"),
			},
		},
		{
			name: "unresolved reference",
			docs: []config.Document{
				newContainerWithTextFiles("a", "missing-configs"),
			},
			expectedErr: `container "a": mounts[0]: textFiles "missing-configs" is not configured`,
		},
		{
			name: "unreferenced document is fine",
			docs: []config.Document{
				newTextFilesDoc("unused-configs"),
				newContainerDoc("a"),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := validateMixedDocs(t, test.docs...)

			if test.expectedErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.expectedErr)
			}
		})
	}
}
