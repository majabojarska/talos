// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/inmem"
	"github.com/cosi-project/runtime/pkg/state/impl/namespaced"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	runtime "github.com/siderolabs/talos/internal/app/machined/internal/server/v1alpha1"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
)

func TestExplainUnlocatedVolume(t *testing.T) {
	locatedEphemeral := volumeSpec{
		id:         constants.EphemeralPartitionLabel,
		volumeType: block.VolumeTypePartition,
		phase:      block.VolumePhaseReady,
		location:   "/dev/sda4",
	}

	for _, test := range []struct {
		name     string
		volumes  []volumeSpec
		explain  string
		expected string
	}{
		{
			name: "block-backed volume is not located yet",
			volumes: []volumeSpec{
				{
					id:         constants.EphemeralPartitionLabel,
					volumeType: block.VolumeTypePartition,
					phase:      block.VolumePhaseWaiting,
				},
			},
			explain:  constants.EphemeralPartitionLabel,
			expected: `volume "EPHEMERAL" (partition) is not located: phase "waiting"`,
		},
		{
			name: "block-backed volume failed to locate",
			volumes: []volumeSpec{
				{
					id:           constants.EphemeralPartitionLabel,
					volumeType:   block.VolumeTypePartition,
					phase:        block.VolumePhaseFailed,
					errorMessage: "no disks matched",
				},
			},
			explain:  constants.EphemeralPartitionLabel,
			expected: `volume "EPHEMERAL" (partition) is not located: phase "failed": no disks matched`,
		},
		{
			name: "directory volume points at the volume holding its data",
			volumes: []volumeSpec{
				locatedEphemeral,
				{
					id:            constants.CRIContainerdVolumeID,
					volumeType:    block.VolumeTypeDirectory,
					phase:         block.VolumePhaseReady,
					mountParentID: constants.EphemeralPartitionLabel,
				},
			},
			explain: constants.CRIContainerdVolumeID,
			expected: `volume "CRI" is directory-backed and has no block device to wipe; ` +
				`its data is stored on volume "EPHEMERAL" — wipe that instead, but note it may also hold other volumes`,
		},
		{
			name: "overlay volume points at the volume holding its data",
			volumes: []volumeSpec{
				locatedEphemeral,
				{
					id:         "/opt",
					volumeType: block.VolumeTypeOverlay,
					phase:      block.VolumePhaseReady,
					parentID:   constants.EphemeralPartitionLabel,
				},
			},
			explain: "/opt",
			expected: `volume "/opt" is overlay-backed and has no block device to wipe; ` +
				`its data is stored on volume "EPHEMERAL" — wipe that instead, but note it may also hold other volumes`,
		},
		{
			name: "directory volume without a backing volume",
			volumes: []volumeSpec{
				{
					id:         constants.CRIContainerdVolumeID,
					volumeType: block.VolumeTypeDirectory,
					phase:      block.VolumePhaseReady,
				},
			},
			explain:  constants.CRIContainerdVolumeID,
			expected: `volume "CRI" is directory-backed and has no block device to wipe`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			st := state.WrapCore(namespaced.NewState(inmem.Build))

			for _, vol := range test.volumes {
				createVolumeStatus(ctx, t, st, vol)
			}

			volumeStatus, err := safe.StateGetByID[*block.VolumeStatus](ctx, st, test.explain)
			require.NoError(t, err)

			assert.Equal(t, test.expected, runtime.ExplainUnlocatedVolume(ctx, st, volumeStatus))
		})
	}

	t.Run("failing to resolve the backing volume falls back to the short form", func(t *testing.T) {
		ctx := t.Context()
		inner := namespaced.NewState(inmem.Build)
		st := state.WrapCore(inner)

		createVolumeStatus(ctx, t, st, locatedEphemeral)
		createVolumeStatus(ctx, t, st, volumeSpec{
			id:            constants.CRIContainerdVolumeID,
			volumeType:    block.VolumeTypeDirectory,
			phase:         block.VolumePhaseReady,
			mountParentID: constants.EphemeralPartitionLabel,
		})

		volumeStatus, err := safe.StateGetByID[*block.VolumeStatus](ctx, st, constants.CRIContainerdVolumeID)
		require.NoError(t, err)

		// deny all access, so that resolving the backing volume fails with something other than "not found"
		denied := state.WrapCore(state.Filter(inner, func(context.Context, state.Access) error {
			return errors.New("access denied")
		}))

		assert.Equal(t,
			`volume "CRI" is directory-backed and has no block device to wipe`,
			runtime.ExplainUnlocatedVolume(ctx, denied, volumeStatus),
		)
	})
}

// volumeSpec describes a VolumeStatus to seed into the test state.
type volumeSpec struct {
	id            string
	volumeType    block.VolumeType
	phase         block.VolumePhase
	location      string
	parentID      string
	mountParentID string
	errorMessage  string
}

func createVolumeStatus(ctx context.Context, t *testing.T, st state.State, spec volumeSpec) {
	t.Helper()

	volumeStatus := block.NewVolumeStatus(block.NamespaceName, spec.id)
	volumeStatus.TypedSpec().Type = spec.volumeType
	volumeStatus.TypedSpec().Phase = spec.phase
	volumeStatus.TypedSpec().Location = spec.location
	volumeStatus.TypedSpec().ParentID = spec.parentID
	volumeStatus.TypedSpec().MountSpec.ParentID = spec.mountParentID
	volumeStatus.TypedSpec().ErrorMessage = spec.errorMessage

	require.NoError(t, st.Create(ctx, volumeStatus))
}
