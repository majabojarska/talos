// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package container_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/types/container"
)

// loadTextFiles parses a single-document machine configuration and returns the text files document.
func loadTextFiles(t *testing.T, doc string) *container.TextFilesConfigV1Alpha1 {
	t.Helper()

	provider, err := configloader.NewFromBytes([]byte(doc))
	require.NoError(t, err)

	docs := provider.Documents()
	require.Len(t, docs, 1)

	cfg, ok := docs[0].(*container.TextFilesConfigV1Alpha1)
	require.True(t, ok, "expected a TextFilesConfig document, got %T", docs[0])

	return cfg
}

func TestTextFilesConfigMarshalUnmarshal(t *testing.T) {
	t.Parallel()

	cfg := container.NewTextFilesConfigV1Alpha1()
	cfg.MetaName = "my-foobar-configs"
	cfg.Files = map[string]string{
		"foo.conf":       "[section-abc]\nkey1=val1\n",
		"subdir/baz.ini": "key1=val1\n",
	}

	warnings, err := cfg.Validate(validationMode{})
	require.NoError(t, err)
	assert.Empty(t, warnings)

	marshaled, err := encoder.NewEncoder(cfg, encoder.WithComments(encoder.CommentsDisabled)).Encode()
	require.NoError(t, err)

	// Round-trips through the loader unchanged, which is what makes a config edit idempotent.
	assert.Equal(t, cfg, loadTextFiles(t, string(marshaled)))
}

func TestTextFilesConfigNested(t *testing.T) {
	t.Parallel()

	cfg := loadTextFiles(t, `apiVersion: v1alpha1
kind: TextFilesConfig
name: my-foobar-configs
files:
  foo.conf: |
    [section-abc]
    key1=val1
  subdir/baz.ini: |
    key1=val1
`)

	_, err := cfg.Validate(validationMode{})
	require.NoError(t, err)

	assert.Equal(t, "my-foobar-configs", cfg.Name())
	assert.Equal(t, map[string]string{
		"foo.conf":       "[section-abc]\nkey1=val1\n",
		"subdir/baz.ini": "key1=val1\n",
	}, cfg.FileContents())
}

func TestTextFilesConfigValidate(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		docName     string
		files       map[string]string
		expectedErr string
	}{
		{
			name:        "no name",
			files:       map[string]string{"a.conf": "x"},
			expectedErr: "name is required",
		},
		{
			name:        "invalid name",
			docName:     "Not_Valid",
			files:       map[string]string{"a.conf": "x"},
			expectedErr: "name can only contain lowercase ASCII letters, digits and hyphens",
		},
		{
			name:        "no files",
			docName:     "cfg",
			files:       map[string]string{},
			expectedErr: "files is required and must not be empty",
		},
		{
			name:        "absolute path",
			docName:     "cfg",
			files:       map[string]string{"/etc/a.conf": "x"},
			expectedErr: `files["/etc/a.conf"]: path must be relative and must not escape the file set`,
		},
		{
			name:        "parent traversal",
			docName:     "cfg",
			files:       map[string]string{"../a.conf": "x"},
			expectedErr: `files["../a.conf"]: path must be relative and must not escape the file set`,
		},
		{
			name:    "escaping traversal that cleans back inside",
			docName: "cfg",
			// filepath.Clean turns this into "b", so accepting it would silently write a different
			// file than the one named.
			files:       map[string]string{"a/../../b": "x"},
			expectedErr: `files["a/../../b"]: path must be relative and must not escape the file set`,
		},
		{
			name:        "non-canonical path",
			docName:     "cfg",
			files:       map[string]string{"a//b.conf": "x"},
			expectedErr: `files["a//b.conf"]: path must be in canonical form, i.e. "a/b.conf"`,
		},
		{
			name:        "trailing slash",
			docName:     "cfg",
			files:       map[string]string{"a/": "x"},
			expectedErr: `files["a/"]: path must be in canonical form, i.e. "a"`,
		},
		{
			name:        "file is also a directory",
			docName:     "cfg",
			files:       map[string]string{"a/b": "x", "a/b/c": "y"},
			expectedErr: `files["a/b"]: path is used as both a file and a directory`,
		},
		{
			name:        "invalid UTF-8 content",
			docName:     "cfg",
			files:       map[string]string{"a.conf": "\xff\xfe"},
			expectedErr: `files["a.conf"]: content is not valid UTF-8`,
		},
		{
			name:        "NUL in content",
			docName:     "cfg",
			files:       map[string]string{"a.conf": "x\x00y"},
			expectedErr: `files["a.conf"]: content contains a NUL byte`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := container.NewTextFilesConfigV1Alpha1()
			cfg.MetaName = test.docName
			cfg.Files = test.files

			_, err := cfg.Validate(validationMode{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.expectedErr)
		})
	}
}

func TestTextFilesConfigWarnsOnLargePayload(t *testing.T) {
	t.Parallel()

	cfg := container.NewTextFilesConfigV1Alpha1()
	cfg.MetaName = "big"
	cfg.Files = map[string]string{"a.conf": strings.Repeat("x", 2<<20)}

	warnings, err := cfg.Validate(validationMode{})
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "stored in the machine configuration")
}
