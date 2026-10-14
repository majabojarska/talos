// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"

	containersctrl "github.com/siderolabs/talos/internal/app/machined/pkg/controllers/containers"
	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/ctest"
	configcfg "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	blockcfg "github.com/siderolabs/talos/pkg/machinery/config/types/block"
	containercfg "github.com/siderolabs/talos/pkg/machinery/config/types/container"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/containers"
)

type ConfigSuite struct {
	ctest.DefaultSuite
}

func TestConfigSuite(t *testing.T) {
	t.Parallel()

	suite.Run(t, &ConfigSuite{
		Timeout: 5 * time.Second,
		AfterSetup: func(suite *ctest.DefaultSuite) {
			suite.Require().NoError(suite.Runtime().RegisterController(&containersctrl.ConfigController{}))
		},
	})
}

// applyContainers puts the given documents into the active machine config.
func (suite *ConfigSuite) applyContainers(documents ...configcfg.Document) {
	cfg, err := container.New(documents...)
	suite.Require().NoError(err)

	suite.Require().NoError(suite.State().Create(suite.Ctx(), config.NewMachineConfig(cfg)))
}

func newDoc(name, image string) *containercfg.ContainerConfigV1Alpha1 {
	doc := containercfg.NewContainerConfigV1Alpha1()
	doc.MetaName = name
	doc.ContainerImage = image

	return doc
}

func (suite *ConfigSuite) TestNormalizesImageAndResolvesDefaults() {
	suite.applyContainers(newDoc("nginx", "nginx"))

	ctest.AssertResource(suite, "nginx", func(spec *containers.ContainerSpec, asrt *assert.Assertions) {
		// A short reference is expanded here, once, so that everything downstream sees the same
		// canonical string.
		asrt.Equal("index.docker.io/library/nginx:latest", spec.TypedSpec().Image.Ref)

		// Defaults: restricted, no host network, no limits.
		asrt.False(spec.TypedSpec().Security.Privileged)
		asrt.False(spec.TypedSpec().Network.HostNetwork)
		asrt.Zero(spec.TypedSpec().Resources.MemoryLimit)
	})
}

func (suite *ConfigSuite) TestResolvesMounts() {
	doc := newDoc("director", "ghcr.io/siderolabs/director:v1.0.0")
	doc.MountsConfig = []containercfg.ContainerMount{
		{
			VolumeMount: &containercfg.VolumeMount{
				VolumeName:       "director-data",
				MountDestination: "/var/lib/director",
				MountOpts:        []string{"rw"},
			},
		},
		{
			TmpfsMount: &containercfg.TmpfsMount{
				MountDestination: "/tmp",
				MountSize:        "64MiB",
			},
		},
		{
			TmpfsMount: &containercfg.TmpfsMount{
				MountDestination: "/tmp/ro",
				MountOpts:        []string{"ro"},
			},
		},
		{
			HostPathMount: &containercfg.HostPathMount{
				MountSource:      "/dev",
				MountDestination: "/dev",
			},
		},
	}

	userVolume := blockcfg.NewUserVolumeConfigV1Alpha1()
	userVolume.MetaName = "director-data"

	suite.applyContainers(doc, userVolume)

	ctest.AssertResource(suite, "director", func(spec *containers.ContainerSpec, asrt *assert.Assertions) {
		mounts := spec.TypedSpec().Mounts
		asrt.Len(mounts, 4)

		// A volume resolves to its volume ID, not a host path: the path is only known
		// once the volume is mounted, which is a different controller's job.
		asrt.Equal(containers.MountKindVolume, mounts[0].Kind)
		asrt.Equal("director-data", mounts[0].VolumeName)
		asrt.Equal("u-director-data", mounts[0].VolumeID)
		asrt.Empty(mounts[0].Source)
		asrt.NotContains(mounts[0].Options, "ro")

		asrt.Equal(containers.MountKindTmpfs, mounts[1].Kind)
		asrt.Equal(uint64(64<<20), mounts[1].Size)
		// Writable by default.
		asrt.NotContains(mounts[1].Options, "ro")

		asrt.Equal(containers.MountKindTmpfs, mounts[2].Kind)
		// An explicit ro is honored.
		asrt.Equal([]string{"ro"}, mounts[2].Options)

		asrt.Equal(containers.MountKindHostPath, mounts[3].Kind)
		asrt.Equal("/dev", mounts[3].Source)
		// Writable by default.
		asrt.NotContains(mounts[3].Options, "ro")
	})
}

// TestResolvesVolumeKinds covers a volume mount naming each kind of volume: the name resolves to the
// volume ID of whichever kind declares it, and a name nothing declares resolves to no volume ID.
func (suite *ConfigSuite) TestResolvesVolumeKinds() {
	doc := newDoc("director", "ghcr.io/siderolabs/director:v1.0.0")

	for _, name := range []string{"user-data", "existing-data", "external-data", "undeclared"} {
		doc.MountsConfig = append(doc.MountsConfig, containercfg.ContainerMount{
			VolumeMount: &containercfg.VolumeMount{
				VolumeName:       name,
				MountDestination: "/mnt/" + name,
				MountOpts:        []string{"ro"},
			},
		})
	}

	userVolume := blockcfg.NewUserVolumeConfigV1Alpha1()
	userVolume.MetaName = "user-data"

	existingVolume := blockcfg.NewExistingVolumeConfigV1Alpha1()
	existingVolume.MetaName = "existing-data"

	externalVolume := blockcfg.NewExternalVolumeConfigV1Alpha1()
	externalVolume.MetaName = "external-data"

	suite.applyContainers(doc, userVolume, existingVolume, externalVolume)

	ctest.AssertResource(suite, "director", func(spec *containers.ContainerSpec, asrt *assert.Assertions) {
		mounts := spec.TypedSpec().Mounts
		asrt.Len(mounts, 4)

		for i, expected := range []struct {
			name string
			id   string
		}{
			{name: "user-data", id: "u-user-data"},
			{name: "existing-data", id: "e-existing-data"},
			{name: "external-data", id: "x-external-data"},
			{name: "undeclared", id: ""},
		} {
			asrt.Equal(containers.MountKindVolume, mounts[i].Kind)
			asrt.Equal(expected.name, mounts[i].VolumeName)
			asrt.Equal(expected.id, mounts[i].VolumeID)
		}
	})
}

func (suite *ConfigSuite) TestResolvesSecurityNetworkAndResources() {
	doc := newDoc("director", "ghcr.io/siderolabs/director:v1.0.0")
	doc.SecurityConfig = &containercfg.ContainerSecurity{
		SecurityProfile: "privileged",
		SecurityCapabilities: &containercfg.ContainerCapabilities{
			CapabilitiesAddConfig:  []string{"NET_ADMIN"},
			CapabilitiesDropConfig: []string{"ALL"},
		},
	}
	doc.NetworkConfig = &containercfg.ContainerNetwork{NetworkMode: "host"}
	doc.ResourcesConfig = &containercfg.ContainerResources{
		Limits: &containercfg.ContainerResourceLimits{CPU: "1500m", Memory: "512MiB"},
	}
	doc.DependsOnConfig = &containercfg.ContainerDependsOn{
		PathsConfig:    []string{"/var/mnt/data"},
		NetworksConfig: []string{"addresses"},
		TimeConfig:     new(true),
	}
	doc.RunAsConfig = &containercfg.ContainerRunAs{
		RunAsUID: new(int32(1000)),
		RunAsGID: new(int32(1000)),
	}

	suite.applyContainers(doc)

	ctest.AssertResource(suite, "director", func(spec *containers.ContainerSpec, asrt *assert.Assertions) {
		asrt.True(spec.TypedSpec().Security.Privileged)
		asrt.Equal([]string{"NET_ADMIN"}, spec.TypedSpec().Security.CapabilitiesAdd)
		asrt.True(spec.TypedSpec().Network.HostNetwork)

		asrt.Equal(uint64(512<<20), spec.TypedSpec().Resources.MemoryLimit)
		// CPU stays in millicores; the cgroup conversion happens where the cgroup is created.
		asrt.Equal(uint64(1500), spec.TypedSpec().Resources.CPULimit)

		asrt.Equal([]string{"/var/mnt/data"}, spec.TypedSpec().DependsOn.Paths)
		asrt.True(spec.TypedSpec().DependsOn.Time)

		if asrt.NotNil(spec.TypedSpec().RunAs.UID) {
			asrt.Equal(int32(1000), *spec.TypedSpec().RunAs.UID)
		}

		if asrt.NotNil(spec.TypedSpec().RunAs.GID) {
			asrt.Equal(int32(1000), *spec.TypedSpec().RunAs.GID)
		}
	})
}

func (suite *ConfigSuite) TestRemovesSpecWhenDocumentGoesAway() {
	suite.applyContainers(newDoc("a", "nginx"), newDoc("b", "nginx"))

	ctest.AssertResources(suite, []string{"a", "b"}, func(*containers.ContainerSpec, *assert.Assertions) {})

	// Replace the machine config with one that only declares "a".
	cfg, err := container.New(newDoc("a", "nginx"))
	suite.Require().NoError(err)

	oldCfg, err := suite.State().Get(suite.Ctx(), config.NewMachineConfig(nil).Metadata())
	suite.Require().NoError(err)

	newCfg := config.NewMachineConfig(cfg)
	newCfg.Metadata().SetVersion(oldCfg.Metadata().Version())

	suite.Require().NoError(suite.State().Update(suite.Ctx(), newCfg))

	ctest.AssertNoResource[*containers.ContainerSpec](suite, "b")
	ctest.AssertResource(suite, "a", func(*containers.ContainerSpec, *assert.Assertions) {})
}
