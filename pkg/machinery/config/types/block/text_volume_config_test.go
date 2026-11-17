// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package block_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/types/block"
)

func TestTextVolumeConfigMarshalUnmarshal(t *testing.T) {
	t.Parallel()

	cfg := block.NewTextVolumeConfigV1Alpha1()
	cfg.MetaName = "app-config"
	cfg.FilesSpec = map[string]string{
		"app.conf":       "[server]\nlisten=:8080\n",
		"conf.d/log.ini": "level=info\n",
	}

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
}

func TestTextVolumeConfigValidate(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		cfg   func() *block.TextVolumeConfigV1Alpha1
		errs  []string
		warns []string
	}{
		{
			name: "valid",
			cfg: func() *block.TextVolumeConfigV1Alpha1 {
				c := block.NewTextVolumeConfigV1Alpha1()
				c.MetaName = "app-config"
				c.FilesSpec = map[string]string{"a": "b", "dir/sub/c": ""}

				return c
			},
		},
		{
			name: "no name, no files",
			cfg:  block.NewTextVolumeConfigV1Alpha1,
			errs: []string{"name is required", "files is required and must not be empty"},
		},
		{
			name: "invalid name",
			cfg: func() *block.TextVolumeConfigV1Alpha1 {
				c := block.NewTextVolumeConfigV1Alpha1()
				c.MetaName = "app/config"
				c.FilesSpec = map[string]string{"a": "b"}

				return c
			},
			errs: []string{"name can only contain lowercase and uppercase ASCII letters, digits, and hyphens"},
		},
		{
			name: "bad paths",
			cfg: func() *block.TextVolumeConfigV1Alpha1 {
				c := block.NewTextVolumeConfigV1Alpha1()
				c.MetaName = "app-config"
				c.FilesSpec = map[string]string{
					"":                "",
					"/etc/passwd":     "",
					"../escape":       "",
					"a//b":            "",
					"dir":             "",
					"dir/file":        "",
					".talos-tmp-x":    "",
					"sub/.talos-hide": "",
				}

				return c
			},
			errs: []string{
				`files: path must not be empty`,
				`files["/etc/passwd"]: path must be relative and must not escape the volume`,
				`files["../escape"]: path must be relative and must not escape the volume`,
				`files["a//b"]: path must be in canonical form, i.e. "a/b"`,
				`files["dir"]: path is used as both a file and a directory`,
				`files[".talos-tmp-x"]: names starting with ".talos-" are reserved`,
				`files["sub/.talos-hide"]: names starting with ".talos-" are reserved`,
			},
		},
		{
			name: "bad content",
			cfg: func() *block.TextVolumeConfigV1Alpha1 {
				c := block.NewTextVolumeConfigV1Alpha1()
				c.MetaName = "app-config"
				c.FilesSpec = map[string]string{"nul": "a\x00b", "utf": "\xff"}

				return c
			},
			errs: []string{
				`files["nul"]: content contains a NUL byte`,
				`files["utf"]: content is not valid UTF-8`,
			},
		},
		{
			name: "large",
			cfg: func() *block.TextVolumeConfigV1Alpha1 {
				c := block.NewTextVolumeConfigV1Alpha1()
				c.MetaName = "app-config"
				c.FilesSpec = map[string]string{"big": strings.Repeat("x", block.TextVolumeWarnSize)}

				return c
			},
			warns: []string{`text volume "app-config": total content is 1048579 bytes, which is kept in the machine configuration and in memory for every consumer`},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			warnings, err := test.cfg().Validate(validationMode{})

			assert.Equal(t, test.warns, warnings)

			if len(test.errs) == 0 {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)

			for _, expected := range test.errs {
				assert.ErrorContains(t, err, expected)
			}
		})
	}
}
