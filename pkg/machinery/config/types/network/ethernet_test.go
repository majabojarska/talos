// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package network_test

import (
	_ "embed"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/merge"
	"github.com/siderolabs/talos/pkg/machinery/config/types/meta"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"
)

//go:embed testdata/ethernetconfig.yaml
var expectedEthernetConfigDocument []byte

//go:embed testdata/ethernetconfig_empty_wake_on_lan.yaml
var expectedEthernetConfigDocumentEmptyWakeOnLAN []byte

func TestEthernetConfigMarshalStability(t *testing.T) {
	t.Parallel()

	cfg := network.NewEthernetConfigV1Alpha1("enp0s1")
	cfg.RingsConfig = &network.EthernetRingsConfig{
		RX: new(uint32(16)),
	}
	cfg.FeaturesConfig = map[string]bool{
		"tx-checksum-ipv4": true,
	}
	cfg.ChannelsConfig = &network.EthernetChannelsConfig{
		Combined: new(uint32(1)),
	}
	cfg.WakeOnLANConfig = network.WOLModeList{
		nethelpers.WOLModeUnicast,
		nethelpers.WOLModeMulticast,
	}

	marshaled, err := encoder.NewEncoder(cfg, encoder.WithComments(encoder.CommentsDisabled)).Encode()
	require.NoError(t, err)

	t.Log(string(marshaled))

	assert.Equal(t, expectedEthernetConfigDocument, marshaled)
}

func TestEthernetConfigUnmarshal(t *testing.T) {
	t.Parallel()

	provider, err := configloader.NewFromBytes(expectedEthernetConfigDocument)
	require.NoError(t, err)

	docs := provider.Documents()
	require.Len(t, docs, 1)

	assert.Equal(t, &network.EthernetConfigV1Alpha1{
		Meta: meta.Meta{
			MetaAPIVersion: "v1alpha1",
			MetaKind:       network.EthernetKind,
		},
		MetaName: "enp0s1",
		FeaturesConfig: map[string]bool{
			"tx-checksum-ipv4": true,
		},
		RingsConfig: &network.EthernetRingsConfig{
			RX: new(uint32(16)),
		},
		ChannelsConfig: &network.EthernetChannelsConfig{
			Combined: new(uint32(1)),
		},
		WakeOnLANConfig: network.WOLModeList{
			nethelpers.WOLModeUnicast,
			nethelpers.WOLModeMulticast,
		},
	}, docs[0])
}

// TestEthernetConfigMarshalStabilityEmptyWakeOnLAN asserts that an explicitly empty
// list of Wake-on-LAN modes survives encoding.
//
// An empty list disables Wake-on-LAN, so dropping it on encoding (as `omitempty`
// would) makes it impossible to persist.
func TestEthernetConfigMarshalStabilityEmptyWakeOnLAN(t *testing.T) {
	t.Parallel()

	cfg := network.NewEthernetConfigV1Alpha1("enp0s1")
	cfg.WakeOnLANConfig = network.WOLModeList{}

	marshaled, err := encoder.NewEncoder(cfg, encoder.WithComments(encoder.CommentsDisabled)).Encode()
	require.NoError(t, err)

	t.Log(string(marshaled))

	assert.Equal(t, expectedEthernetConfigDocumentEmptyWakeOnLAN, marshaled)

	// the encoder takes a different code path when comments are enabled, so cover it as well
	marshaledWithComments, err := encoder.NewEncoder(cfg, encoder.WithComments(encoder.CommentsAll)).Encode()
	require.NoError(t, err)

	t.Log(string(marshaledWithComments))

	assert.Contains(t, string(marshaledWithComments), "wakeOnLan: []")
}

// TestEthernetConfigMarshalUnsetWakeOnLAN asserts that an unset list of Wake-on-LAN
// modes is not encoded, as that would turn "unchanged" into "disable" on the next write.
func TestEthernetConfigMarshalUnsetWakeOnLAN(t *testing.T) {
	t.Parallel()

	cfg := network.NewEthernetConfigV1Alpha1("enp0s1")

	marshaled, err := encoder.NewEncoder(cfg, encoder.WithComments(encoder.CommentsDisabled)).Encode()
	require.NoError(t, err)

	assert.NotContains(t, string(marshaled), "wakeOnLan:")

	// with comments enabled the field may still appear as a commented-out example
	// ("# wakeOnLan:"), which is fine; only an uncommented key would mean the
	// value was actually encoded
	marshaledWithComments, err := encoder.NewEncoder(cfg, encoder.WithComments(encoder.CommentsAll)).Encode()
	require.NoError(t, err)

	assert.NotContains(t, string(marshaledWithComments), "\nwakeOnLan:")
}

func TestEthernetConfigUnmarshalEmptyWakeOnLAN(t *testing.T) {
	t.Parallel()

	provider, err := configloader.NewFromBytes(expectedEthernetConfigDocumentEmptyWakeOnLAN)
	require.NoError(t, err)

	docs := provider.Documents()
	require.Len(t, docs, 1)

	cfg, ok := docs[0].(*network.EthernetConfigV1Alpha1)
	require.True(t, ok)

	assert.NotNil(t, cfg.WakeOnLANConfig)
	assert.Empty(t, cfg.WakeOnLANConfig)

	wol := cfg.WakeOnLAN()
	assert.NotNil(t, wol)
	assert.Empty(t, wol)
}

// TestEthernetConfigMergeWakeOnLAN asserts that a strategic merge patch replaces the
// list of Wake-on-LAN modes instead of appending to it, so that `wakeOnLan: []` disables it.
func TestEthernetConfigMergeWakeOnLAN(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		left     network.WOLModeList
		right    network.WOLModeList
		expected network.WOLModeList
	}{
		{
			name:     "disable",
			left:     network.WOLModeList{nethelpers.WOLModePhy},
			right:    network.WOLModeList{},
			expected: network.WOLModeList{},
		},
		{
			name:     "replace",
			left:     network.WOLModeList{nethelpers.WOLModePhy},
			right:    network.WOLModeList{nethelpers.WOLModeMagic},
			expected: network.WOLModeList{nethelpers.WOLModeMagic},
		},
		{
			name:     "unset keeps left",
			left:     network.WOLModeList{nethelpers.WOLModePhy},
			right:    nil,
			expected: network.WOLModeList{nethelpers.WOLModePhy},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			left := network.NewEthernetConfigV1Alpha1("enp0s1")
			left.WakeOnLANConfig = test.left

			right := network.NewEthernetConfigV1Alpha1("enp0s1")
			right.WakeOnLANConfig = test.right

			require.NoError(t, merge.Merge(left, right))

			assert.Equal(t, test.expected, left.WakeOnLANConfig)
		})
	}
}

func TestEthernetValidate(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		cfg  func() *network.EthernetConfigV1Alpha1

		expectedError    string
		expectedWarnings []string
	}{
		{
			name: "empty",
			cfg: func() *network.EthernetConfigV1Alpha1 {
				return network.NewEthernetConfigV1Alpha1("")
			},

			expectedError: "name is required",
		},
		{
			name: "valid",
			cfg: func() *network.EthernetConfigV1Alpha1 {
				cfg := network.NewEthernetConfigV1Alpha1("enp0s1")
				cfg.FeaturesConfig = map[string]bool{
					"tx-checksum-ipv4": true,
				}
				cfg.RingsConfig = &network.EthernetRingsConfig{
					RX: new(uint32(16)),
				}

				return cfg
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			warnings, err := test.cfg().Validate(validationMode{})

			assert.Equal(t, test.expectedWarnings, warnings)

			if test.expectedError != "" {
				assert.EqualError(t, err, test.expectedError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
