// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package block_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/types/block"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

func newTextVolumeConfig(files map[string]block.TextVolumeFileV1Alpha1) *block.TextVolumeConfigV1Alpha1 {
	c := block.NewTextVolumeConfigV1Alpha1()
	c.MetaName = "app-config"
	c.FilesConfig = files

	return c
}

func TestTextVolumeConfigMarshalUnmarshal(t *testing.T) {
	t.Parallel()

	cfg := newTextVolumeConfig(map[string]block.TextVolumeFileV1Alpha1{
		"app.yaml": {
			FileContents: "listen: :8080\n",
		},
		"conf.d/extra.ini": {
			FileMode:     0o600,
			FileContents: "[section]\nkey=value\n",
		},
	})

	warnings, err := cfg.Validate(validationMode{})
	require.NoError(t, err)
	require.Empty(t, warnings)

	marshaled, err := encoder.NewEncoder(cfg, encoder.WithComments(encoder.CommentsDisabled)).Encode()
	require.NoError(t, err)

	t.Log(string(marshaled))

	expectedMarshaled, err := os.ReadFile(filepath.Join("testdata", "textvolumeconfig.yaml"))
	require.NoError(t, err)

	assert.Equal(t, string(expectedMarshaled), string(marshaled))

	provider, err := configloader.NewFromBytes(expectedMarshaled)
	require.NoError(t, err)

	docs := provider.Documents()
	require.Len(t, docs, 1)

	assert.Equal(t, cfg, docs[0])

	assert.Equal(t, []config.TextVolumeFile{
		{Path: "app.yaml", Mode: 0o644, Contents: "listen: :8080\n"},
		{Path: "conf.d/extra.ini", Mode: 0o600, Contents: "[section]\nkey=value\n"},
	}, provider.TextVolumeConfigs()[0].Files())
}

func TestTextVolumeConfigValidate(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string

		cfg func() *block.TextVolumeConfigV1Alpha1

		expectedErrors string
	}{
		{
			name: "reserved nested component",
			cfg: func() *block.TextVolumeConfigV1Alpha1 {
				return newTextVolumeConfig(map[string]block.TextVolumeFileV1Alpha1{
					".env":              {FileContents: "A=b\n"},
					"a/b/c.conf":        {FileMode: 0o400},
					"a/bc":              {},
					"a.d/file":          {},
					"some.dir/..x.conf": {},
				})
			},
			expectedErrors: `file path "some.dir/..x.conf": path components starting with ".." are reserved`,
		},
		{
			name: "valid nested",
			cfg: func() *block.TextVolumeConfigV1Alpha1 {
				return newTextVolumeConfig(map[string]block.TextVolumeFileV1Alpha1{
					".env":       {FileContents: "A=b\n"},
					"a/b/c.conf": {FileMode: 0o400},
					"a/bc":       {},
					"a.d/file":   {},
				})
			},
		},
		{
			name: "no name",
			cfg: func() *block.TextVolumeConfigV1Alpha1 {
				c := newTextVolumeConfig(map[string]block.TextVolumeFileV1Alpha1{"a": {}})
				c.MetaName = ""

				return c
			},
			expectedErrors: "name is required\nname must be between 1 and 34 characters long",
		},
		{
			name: "invalid name",
			cfg: func() *block.TextVolumeConfigV1Alpha1 {
				c := newTextVolumeConfig(map[string]block.TextVolumeFileV1Alpha1{"a": {}})
				c.MetaName = "a/b"

				return c
			},
			expectedErrors: "name can only contain lowercase and uppercase ASCII letters, digits, and hyphens",
		},
		{
			name: "no files",
			cfg: func() *block.TextVolumeConfigV1Alpha1 {
				return newTextVolumeConfig(nil)
			},
			expectedErrors: "at least one file is required",
		},
		{
			name: "invalid paths",
			cfg: func() *block.TextVolumeConfigV1Alpha1 {
				return newTextVolumeConfig(map[string]block.TextVolumeFileV1Alpha1{
					"":       {},
					"/abs":   {},
					"../up":  {},
					"a//b":   {},
					"a/./b":  {},
					"..data": {},
					"x/":     {},
				})
			},
			expectedErrors: "file path can't be empty\n" +
				`file path "../up" must be a clean relative path` + "\n" +
				`file path "..data": path components starting with ".." are reserved` + "\n" +
				`file path "/abs" must be a clean relative path` + "\n" +
				`file path "a/./b" must be a clean relative path` + "\n" +
				`file path "a//b" must be a clean relative path` + "\n" +
				`file path "x/" must be a clean relative path`,
		},
		{
			name: "file and directory",
			cfg: func() *block.TextVolumeConfigV1Alpha1 {
				return newTextVolumeConfig(map[string]block.TextVolumeFileV1Alpha1{
					"a":        {},
					"a.d/file": {},
					"a/b/c":    {},
				})
			},
			expectedErrors: `file "a" conflicts with file "a/b/c": a path can't be both a file and a directory`,
		},
		{
			name: "invalid mode",
			cfg: func() *block.TextVolumeConfigV1Alpha1 {
				return newTextVolumeConfig(map[string]block.TextVolumeFileV1Alpha1{
					"a": {FileMode: block.TextVolumeFileMode(fs.ModeSetuid | 0o755)},
				})
			},
			expectedErrors: `file "a": mode 0o40000755 may only contain permission bits`,
		},
		{
			name: "too large",
			cfg: func() *block.TextVolumeConfigV1Alpha1 {
				return newTextVolumeConfig(map[string]block.TextVolumeFileV1Alpha1{
					"a": {FileContents: strings.Repeat("x", constants.TextVolumeMaxContentsSize)},
					"b": {FileContents: "x"},
				})
			},
			expectedErrors: "total size of the file contents 1048577 exceeds the limit of 1048576 bytes",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			warnings, err := test.cfg().Validate(validationMode{})
			assert.Empty(t, warnings)

			if test.expectedErrors == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.EqualError(t, err, test.expectedErrors)
		})
	}
}
