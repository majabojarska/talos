// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers

import (
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/resource/meta"
	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	"github.com/cosi-project/runtime/pkg/resource/typed"

	"github.com/siderolabs/talos/pkg/machinery/proto"
)

// ContainerImageStatusType is type of ContainerImageStatus resource.
const ContainerImageStatusType = resource.Type("ContainerImageStatuses.containers.talos.dev")

// ContainerImageStatus resource holds the state of a container's image pull.
type ContainerImageStatus = typed.Resource[ContainerImageStatusSpec, ContainerImageStatusExtension]

// ContainerImageStatusSpec is the spec for ContainerImageStatus.
//
//gotagsrewrite:gen
type ContainerImageStatusSpec struct {
	Phase ContainerImagePhase `yaml:"phase" protobuf:"1"`
	// Image is the reference that was requested, in canonical form.
	Image string `yaml:"image" protobuf:"2"`
	// Digest is the resolved digest, set once the pull completes.
	Digest string `yaml:"digest,omitempty" protobuf:"3"`
	// Progress is a human-readable pull progress line, e.g. "12 MiB / 40 MiB".
	Progress string `yaml:"progress,omitempty" protobuf:"4"`
	// Error is the last pull failure, verbatim.
	Error string `yaml:"error,omitempty" protobuf:"5"`
}

// NewContainerImageStatus initializes a ContainerImageStatus resource.
func NewContainerImageStatus(namespace resource.Namespace, id resource.ID) *ContainerImageStatus {
	return typed.NewResource[ContainerImageStatusSpec, ContainerImageStatusExtension](
		resource.NewMetadata(namespace, ContainerImageStatusType, id, resource.VersionUndefined),
		ContainerImageStatusSpec{},
	)
}

// ContainerImageStatusExtension is auxiliary resource data for ContainerImageStatus.
type ContainerImageStatusExtension struct{}

// ResourceDefinition implements meta.ResourceDefinitionProvider interface.
func (ContainerImageStatusExtension) ResourceDefinition() meta.ResourceDefinitionSpec {
	return meta.ResourceDefinitionSpec{
		Type:             ContainerImageStatusType,
		Aliases:          []resource.Type{"containerimagestatus", "containerimagestatuses"},
		DefaultNamespace: NamespaceName,
		PrintColumns: []meta.PrintColumn{
			{
				Name:     "Phase",
				JSONPath: `{.phase}`,
			},
			{
				Name:     "Digest",
				JSONPath: `{.digest}`,
			},
		},
	}
}

// ContainerMountStatusType is type of ContainerMountStatus resource.
const ContainerMountStatusType = resource.Type("ContainerMountStatuses.containers.talos.dev")

// ContainerMountStatus resource holds the resolved host paths for a container's mounts.
type ContainerMountStatus = typed.Resource[ContainerMountStatusSpec, ContainerMountStatusExtension]

// ContainerMountStatusSpec is the spec for ContainerMountStatus.
//
//gotagsrewrite:gen
type ContainerMountStatusSpec struct {
	// Ready is true once every mount the container needs is available.
	Ready bool `yaml:"ready" protobuf:"1"`
	// Mounts are the resolved mounts, with host source paths filled in.
	Mounts []ResolvedMountSpec `yaml:"mounts,omitempty" protobuf:"2"`
	// Error is the last mount failure, verbatim.
	Error string `yaml:"error,omitempty" protobuf:"3"`
}

// ResolvedMountSpec is a mount with its host-side source resolved.
//
//gotagsrewrite:gen
type ResolvedMountSpec struct {
	Kind string `yaml:"kind" protobuf:"1"`
	// Source is the host path to bind from; empty for tmpfs.
	Source      string   `yaml:"source,omitempty" protobuf:"2"`
	Destination string   `yaml:"destination" protobuf:"3"`
	Size        uint64   `yaml:"size,omitempty" protobuf:"4"`
	Options     []string `yaml:"options,omitempty" protobuf:"5"`
}

// NewContainerMountStatus initializes a ContainerMountStatus resource.
func NewContainerMountStatus(namespace resource.Namespace, id resource.ID) *ContainerMountStatus {
	return typed.NewResource[ContainerMountStatusSpec, ContainerMountStatusExtension](
		resource.NewMetadata(namespace, ContainerMountStatusType, id, resource.VersionUndefined),
		ContainerMountStatusSpec{},
	)
}

// ContainerMountStatusExtension is auxiliary resource data for ContainerMountStatus.
type ContainerMountStatusExtension struct{}

// ResourceDefinition implements meta.ResourceDefinitionProvider interface.
func (ContainerMountStatusExtension) ResourceDefinition() meta.ResourceDefinitionSpec {
	return meta.ResourceDefinitionSpec{
		Type:             ContainerMountStatusType,
		Aliases:          []resource.Type{"containermountstatus", "containermountstatuses"},
		DefaultNamespace: NamespaceName,
		PrintColumns: []meta.PrintColumn{
			{
				Name:     "Ready",
				JSONPath: `{.ready}`,
			},
		},
	}
}

// ContainerStatusType is type of ContainerStatus resource.
const ContainerStatusType = resource.Type("ContainerStatuses.containers.talos.dev")

// ContainerStatus resource is the aggregated, user-facing status of a container.
//
// It is stored in memory only and does not survive a reboot.
type ContainerStatus = typed.Resource[ContainerStatusSpec, ContainerStatusExtension]

// ContainerStatusSpec is the spec for ContainerStatus.
//
//gotagsrewrite:gen
type ContainerStatusSpec struct {
	// State is the fine-grained lifecycle position, derived from the newest instance.
	State ContainerState `yaml:"state" protobuf:"1"`
	// Health is the coarse summary of State, kept stable across internal changes.
	Health ContainerHealth `yaml:"health" protobuf:"2"`
	// Image is the resolved digest once the pull completes, otherwise the requested reference.
	Image string `yaml:"image,omitempty" protobuf:"3"`
	// PID of the running task; zero when not running.
	PID uint32 `yaml:"pid,omitempty" protobuf:"4"`
	// ExitCode of the last task exit.
	ExitCode int32 `yaml:"exitCode,omitempty" protobuf:"5"`
	// RestartCount is the current instance generation, i.e. restarts beyond the first start.
	RestartCount uint64 `yaml:"restartCount" protobuf:"6"`
	// Error is the last failure, verbatim, from whichever stage produced it.
	Error string `yaml:"error,omitempty" protobuf:"7"`
	// WaitingFor lists the unmet dependsOn entries while State is pending.
	WaitingFor []string `yaml:"waitingFor,omitempty" protobuf:"8"`
}

// NewContainerStatus initializes a ContainerStatus resource.
func NewContainerStatus(namespace resource.Namespace, id resource.ID) *ContainerStatus {
	return typed.NewResource[ContainerStatusSpec, ContainerStatusExtension](
		resource.NewMetadata(namespace, ContainerStatusType, id, resource.VersionUndefined),
		ContainerStatusSpec{},
	)
}

// ContainerStatusExtension is auxiliary resource data for ContainerStatus.
type ContainerStatusExtension struct{}

// ResourceDefinition implements meta.ResourceDefinitionProvider interface.
func (ContainerStatusExtension) ResourceDefinition() meta.ResourceDefinitionSpec {
	return meta.ResourceDefinitionSpec{
		Type:             ContainerStatusType,
		Aliases:          []resource.Type{"containerstatus", "containerstatuses"},
		DefaultNamespace: NamespaceName,
		PrintColumns: []meta.PrintColumn{
			{
				Name:     "State",
				JSONPath: `{.state}`,
			},
			{
				Name:     "Health",
				JSONPath: `{.health}`,
			},
			{
				Name:     "Restarts",
				JSONPath: `{.restartCount}`,
			},
			{
				Name:     "Image",
				JSONPath: `{.image}`,
			},
		},
	}
}

func init() {
	proto.RegisterDefaultTypes()

	if err := protobuf.RegisterDynamic(ContainerImageStatusType, &ContainerImageStatus{}); err != nil {
		panic(err)
	}

	if err := protobuf.RegisterDynamic(ContainerMountStatusType, &ContainerMountStatus{}); err != nil {
		panic(err)
	}

	if err := protobuf.RegisterDynamic(ContainerStatusType, &ContainerStatus{}); err != nil {
		panic(err)
	}
}
