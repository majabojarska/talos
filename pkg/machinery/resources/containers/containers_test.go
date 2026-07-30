// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/resource/meta"
	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/inmem"
	"github.com/cosi-project/runtime/pkg/state/impl/namespaced"
	"github.com/cosi-project/runtime/pkg/state/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos/pkg/machinery/resources/containers"
)

func TestRegisterResource(t *testing.T) {
	ctx := t.Context()

	resources := state.WrapCore(namespaced.NewState(inmem.Build))
	resourceRegistry := registry.NewResourceRegistry(resources)

	for _, res := range []meta.ResourceWithRD{
		&containers.ContainerSpec{},
		&containers.ContainerImageStatus{},
		&containers.ContainerMountStatus{},
		&containers.ContainerInstanceSpec{},
		&containers.ContainerInstanceStatus{},
		&containers.ContainerStatus{},
		&containers.ContainerLifecycle{},
	} {
		assert.NoError(t, resourceRegistry.Register(ctx, res))
	}
}

// ParseInstanceID splits an instance ID back into the container name and generation.
func ParseInstanceID(id resource.ID) (container string, generation uint64, err error) {
	idx := strings.LastIndex(id, "-")
	if idx < 1 || idx == len(id)-1 {
		return "", 0, fmt.Errorf("malformed instance ID %q, expected <container>-<generation>", id)
	}

	generation, err = strconv.ParseUint(id[idx+1:], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("malformed generation in instance ID %q: %w", id, err)
	}

	return id[:idx], generation, nil
}

// TestProtobufRoundTrip guards the protobuf tags on every spec. RegisterDynamic marshals via those
// tags rather than generated code, so a missing or duplicated tag only surfaces here or on the wire.
func TestProtobufRoundTrip(t *testing.T) {
	t.Parallel()

	spec := containers.NewContainerSpec(containers.NamespaceName, "nginx")
	*spec.TypedSpec() = containers.ContainerSpecSpec{
		Image:       "docker.io/library/nginx:latest",
		Entrypoint:  []string{"/entrypoint.sh"},
		Args:        []string{"nginx", "-g", "daemon off;"},
		WorkingDir:  "/srv",
		User:        "65534:65534",
		Environment: []string{"NGINX_PORT=8080"},
		Mounts: []containers.ContainerMountSpec{
			{
				Kind:        containers.MountKindUserVolume,
				VolumeID:    "u-web-content",
				Destination: "/usr/share/nginx/html",
				Options:     []string{"ro"},
			},
			{
				Kind:        containers.MountKindTmpfs,
				Destination: "/tmp",
				Size:        64 << 20,
			},
		},
		Security: containers.ContainerSecuritySpec{
			Privileged:       true,
			CapabilitiesAdd:  []string{"NET_ADMIN"},
			CapabilitiesDrop: []string{"ALL"},
		},
		Network:   containers.ContainerNetworkSpec{HostNetwork: true},
		Resources: containers.ContainerResourcesSpec{MemoryLimit: 1 << 29, CPULimit: 1500},
		DependsOn: containers.ContainerDependsOnSpec{
			Paths:      []string{"/var/mnt/web-content"},
			Networks:   []string{"addresses"},
			Time:       true,
			Containers: []string{"other"},
		},
	}

	assertRoundTrip(t, spec)

	instanceStatus := containers.NewContainerInstanceStatus(containers.NamespaceName, containers.InstanceID("nginx", 3))
	*instanceStatus.TypedSpec() = containers.ContainerInstanceStatusSpec{
		ContainerID: "nginx",
		Generation:  3,
		Phase:       containers.ContainerInstancePhaseTerminated,
		PID:         4242,
		ExitCode:    137,
		Error:       "killed",
		// Timestamps go over the wire as google.protobuf.Timestamp, so truncate to the precision
		// that survives the trip.
		StartedAt:  time.Now().UTC().Truncate(time.Second),
		FinishedAt: time.Now().UTC().Truncate(time.Second),
	}

	assertRoundTrip(t, instanceStatus)

	status := containers.NewContainerStatus(containers.NamespaceName, "nginx")
	*status.TypedSpec() = containers.ContainerStatusSpec{
		State:        containers.ContainerStateBackoff,
		Health:       containers.ContainerHealthDegraded,
		Image:        "docker.io/library/nginx@sha256:abc",
		PID:          0,
		ExitCode:     1,
		RestartCount: 7,
		Error:        "boom",
		WaitingFor:   []string{"network: addresses"},
	}

	assertRoundTrip(t, status)
}

func assertRoundTrip[T resource.Resource](t *testing.T, res T) {
	t.Helper()

	protoRes, err := protobuf.FromResource(res)
	require.NoError(t, err)

	marshaled, err := protoRes.Marshal()
	require.NoError(t, err)

	unmarshaled, err := protobuf.Unmarshal(marshaled)
	require.NoError(t, err)

	decoded, err := protobuf.UnmarshalResource(unmarshaled)
	require.NoError(t, err)

	assert.Equal(t, res.Metadata().ID(), decoded.Metadata().ID())
	assert.Equal(t, res.Metadata().Type(), decoded.Metadata().Type())
	assert.Equal(t, res.Spec(), decoded.Spec())
}

func TestInstanceID(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "nginx-0", containers.InstanceID("nginx", 0))
	assert.Equal(t, "nginx-42", containers.InstanceID("nginx", 42))

	// A container name containing a hyphen must still round-trip, which is why the split is on the
	// last hyphen rather than the first.
	assert.Equal(t, "my-app-7", containers.InstanceID("my-app", 7))

	for _, test := range []struct {
		id         string
		container  string
		generation uint64
		expectErr  bool
	}{
		{id: "nginx-0", container: "nginx", generation: 0},
		{id: "my-app-7", container: "my-app", generation: 7},
		{id: "nginx", expectErr: true},
		{id: "nginx-", expectErr: true},
		{id: "-0", expectErr: true},
		{id: "nginx-abc", expectErr: true},
	} {
		t.Run(test.id, func(t *testing.T) {
			t.Parallel()

			container, generation, err := ParseInstanceID(test.id)

			if test.expectErr {
				assert.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, test.container, container)
			assert.Equal(t, test.generation, generation)
		})
	}
}

// TestStateHealthMappingIsTotal keeps the coarse projection honest: every state must map to a
// health value, and the mapping lives in one place so it cannot drift between controllers.
func TestStateHealthMappingIsTotal(t *testing.T) {
	t.Parallel()

	for _, state := range containers.ContainerStateValues() {
		health := state.Health()

		assert.Contains(t, containers.ContainerHealthValues(), health,
			"state %s maps to an unknown health value", state)
	}

	assert.Equal(t, containers.ContainerHealthPending, containers.ContainerStatePending.Health())
	assert.Equal(t, containers.ContainerHealthPulling, containers.ContainerStatePulling.Health())
	assert.Equal(t, containers.ContainerHealthPulling, containers.ContainerStateStarting.Health())
	assert.Equal(t, containers.ContainerHealthHealthy, containers.ContainerStateRunning.Health())
	assert.Equal(t, containers.ContainerHealthDegraded, containers.ContainerStateBackoff.Health())
}
