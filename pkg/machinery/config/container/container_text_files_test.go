// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package container_test

import (
	"strings"
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

// newTextFilesDocSized builds a TextFilesConfig document whose single file totals approximately the
// given number of bytes (path length included), for exercising the size warnings.
func newTextFilesDocSized(name string, total int) *containercfg.TextFilesConfigV1Alpha1 {
	const path = "foo.conf"

	doc := containercfg.NewTextFilesConfigV1Alpha1()
	doc.MetaName = name
	doc.Files = map[string]string{path: strings.Repeat("a", total-len(path))}

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

// validateMixedDocsWarnings is like validateMixedDocs, but returns the warnings instead of
// discarding them.
func validateMixedDocsWarnings(t *testing.T, docs ...config.Document) []string {
	t.Helper()

	ctr, err := container.New(docs...)
	require.NoError(t, err)

	warnings, err := ctr.ValidateAsClient(validationMode{})
	require.NoError(t, err)

	return warnings
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

func TestContainerTextFilesSize(t *testing.T) {
	t.Parallel()

	// Below TextFilesWarnSize alone, but over it once doubled by two referencing containers.
	sharedSize := containercfg.TextFilesWarnSize * 3 / 5

	for _, test := range []struct {
		name             string
		docs             []config.Document
		expectSizeWarn   bool
		expectDocOnly    bool
		expectedWarnText string
	}{
		{
			name: "small document shared by two containers has no warning",
			docs: []config.Document{
				newTextFilesDocSized("small-configs", 100),
				newContainerWithTextFiles("a", "small-configs"),
				newContainerWithTextFiles("b", "small-configs"),
			},
		},
		{
			name: "large document mounted by a single container warns once, per document only",
			docs: []config.Document{
				newTextFilesDocSized("large-configs", containercfg.TextFilesWarnSize+100),
				newContainerWithTextFiles("a", "large-configs"),
			},
			expectSizeWarn: true,
			expectDocOnly:  true,
		},
		{
			name: "document under the threshold alone crosses it once shared by two containers",
			docs: []config.Document{
				newTextFilesDocSized("shared-configs", sharedSize),
				newContainerWithTextFiles("a", "shared-configs"),
				newContainerWithTextFiles("b", "shared-configs"),
			},
			expectSizeWarn:   true,
			expectedWarnText: `mounted by 2 containers`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			warnings := validateMixedDocsWarnings(t, test.docs...)

			// Only look at textFiles-related warnings: an unrelated default (e.g. the image not
			// being digest-pinned) is not this test's concern.
			var textFilesWarnings []string

			for _, w := range warnings {
				if strings.HasPrefix(w, "text files ") {
					textFilesWarnings = append(textFilesWarnings, w)
				}
			}

			var sawMountedByWarning bool

			for _, w := range textFilesWarnings {
				if strings.Contains(w, "mounted by") {
					sawMountedByWarning = true
				}
			}

			assert.Equal(t, test.expectSizeWarn && !test.expectDocOnly, sawMountedByWarning,
				"textFiles warnings: %v", textFilesWarnings)

			if test.expectedWarnText != "" {
				assert.Contains(t, strings.Join(textFilesWarnings, "\n"), test.expectedWarnText)
			}

			if test.expectDocOnly {
				require.Len(t, textFilesWarnings, 1)
				assert.NotContains(t, textFilesWarnings[0], "mounted by")
			}

			if !test.expectSizeWarn {
				assert.Empty(t, textFilesWarnings)
			}
		})
	}
}
