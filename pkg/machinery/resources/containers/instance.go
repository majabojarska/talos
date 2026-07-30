// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers

import (
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/resource/meta"
	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	"github.com/cosi-project/runtime/pkg/resource/typed"

	"github.com/siderolabs/talos/pkg/machinery/proto"
)

// ContainerInstanceSpecType is type of ContainerInstanceSpec resource.
const ContainerInstanceSpecType = resource.Type("ContainerInstanceSpecs.containers.talos.dev")

// ContainerInstanceSpec resource represents a single execution attempt of a container.
//
// Its existence is the instruction to run; its destruction is the instruction to stop. Restart is
// therefore a resource event rather than a loop inside a goroutine: the previous instance
// terminates, and the next generation replaces it.
//
// The ID is <container>-<generation>; see InstanceID and ParseInstanceID.
type ContainerInstanceSpec = typed.Resource[ContainerInstanceSpecSpec, ContainerInstanceSpecExtension]

// ContainerInstanceSpecSpec is the spec for ContainerInstanceSpec.
//
// It carries a resolved snapshot of everything needed to run one execution, so the runtime
// controller never has to re-read the container spec, image status or mount status. That keeps the
// execution independent of later changes to those inputs: a spec change destroys this instance
// rather than mutating it.
//
//gotagsrewrite:gen
type ContainerInstanceSpecSpec struct {
	// ContainerID is the name of the owning container, i.e. the ContainerSpec ID.
	ContainerID string `yaml:"containerID" protobuf:"1"`
	// Generation is this instance's sequence number for that container.
	Generation uint64 `yaml:"generation" protobuf:"2"`

	// Image is the digest-resolved reference to run.
	Image string `yaml:"image" protobuf:"3"`

	Entrypoint  []string `yaml:"entrypoint,omitempty" protobuf:"4"`
	Args        []string `yaml:"args,omitempty" protobuf:"5"`
	WorkingDir  string   `yaml:"workingDir,omitempty" protobuf:"6"`
	User        string   `yaml:"user,omitempty" protobuf:"7"`
	Environment []string `yaml:"environment,omitempty" protobuf:"8"`

	// Mounts are fully resolved, with host source paths filled in.
	Mounts []ResolvedMountSpec `yaml:"mounts,omitempty" protobuf:"9"`

	Security  ContainerSecuritySpec  `yaml:"security,omitempty" protobuf:"10"`
	Network   ContainerNetworkSpec   `yaml:"network,omitempty" protobuf:"11"`
	Resources ContainerResourcesSpec `yaml:"resources,omitempty" protobuf:"12"`
}

// NewContainerInstanceSpec initializes a ContainerInstanceSpec resource.
func NewContainerInstanceSpec(namespace resource.Namespace, id resource.ID) *ContainerInstanceSpec {
	return typed.NewResource[ContainerInstanceSpecSpec, ContainerInstanceSpecExtension](
		resource.NewMetadata(namespace, ContainerInstanceSpecType, id, resource.VersionUndefined),
		ContainerInstanceSpecSpec{},
	)
}

// ContainerInstanceSpecExtension is auxiliary resource data for ContainerInstanceSpec.
type ContainerInstanceSpecExtension struct{}

// ResourceDefinition implements meta.ResourceDefinitionProvider interface.
func (ContainerInstanceSpecExtension) ResourceDefinition() meta.ResourceDefinitionSpec {
	return meta.ResourceDefinitionSpec{
		Type:             ContainerInstanceSpecType,
		Aliases:          []resource.Type{"containerinstancespec", "containerinstancespecs"},
		DefaultNamespace: NamespaceName,
		PrintColumns: []meta.PrintColumn{
			{
				Name:     "Container",
				JSONPath: `{.containerID}`,
			},
			{
				Name:     "Generation",
				JSONPath: `{.generation}`,
			},
			{
				Name:     "Image",
				JSONPath: `{.image}`,
			},
		},
	}
}

// ContainerInstanceStatusType is type of ContainerInstanceStatus resource.
const ContainerInstanceStatusType = resource.Type("ContainerInstanceStatuses.containers.talos.dev")

// ContainerInstanceStatus resource holds the outcome of one container execution.
//
// Terminated instances are retained for a few generations rather than destroyed immediately, so
// this is where crash history is visible.
type ContainerInstanceStatus = typed.Resource[ContainerInstanceStatusSpec, ContainerInstanceStatusExtension]

// ContainerInstanceStatusSpec is the spec for ContainerInstanceStatus.
//
//gotagsrewrite:gen
type ContainerInstanceStatusSpec struct {
	// ContainerID is the name of the owning container.
	ContainerID string `yaml:"containerID" protobuf:"1"`
	// Generation is this instance's sequence number.
	Generation uint64 `yaml:"generation" protobuf:"2"`

	Phase ContainerInstancePhase `yaml:"phase" protobuf:"3"`

	// PID of the task; zero unless running.
	PID uint32 `yaml:"pid,omitempty" protobuf:"4"`
	// ExitCode is meaningful once Phase is terminated.
	ExitCode int32 `yaml:"exitCode,omitempty" protobuf:"5"`
	// Error is the failure that ended this instance, verbatim.
	Error string `yaml:"error,omitempty" protobuf:"6"`

	// StartedAt is when the task started; zero if it never did.
	StartedAt time.Time `yaml:"startedAt,omitempty" protobuf:"7"`
	// FinishedAt is when the task exited; zero while running.
	FinishedAt time.Time `yaml:"finishedAt,omitempty" protobuf:"8"`
}

// NewContainerInstanceStatus initializes a ContainerInstanceStatus resource.
func NewContainerInstanceStatus(namespace resource.Namespace, id resource.ID) *ContainerInstanceStatus {
	return typed.NewResource[ContainerInstanceStatusSpec, ContainerInstanceStatusExtension](
		resource.NewMetadata(namespace, ContainerInstanceStatusType, id, resource.VersionUndefined),
		ContainerInstanceStatusSpec{},
	)
}

// ContainerInstanceStatusExtension is auxiliary resource data for ContainerInstanceStatus.
type ContainerInstanceStatusExtension struct{}

// ResourceDefinition implements meta.ResourceDefinitionProvider interface.
func (ContainerInstanceStatusExtension) ResourceDefinition() meta.ResourceDefinitionSpec {
	return meta.ResourceDefinitionSpec{
		Type:             ContainerInstanceStatusType,
		Aliases:          []resource.Type{"containerinstancestatus", "containerinstancestatuses"},
		DefaultNamespace: NamespaceName,
		PrintColumns: []meta.PrintColumn{
			{
				Name:     "Container",
				JSONPath: `{.containerID}`,
			},
			{
				Name:     "Phase",
				JSONPath: `{.phase}`,
			},
			{
				Name:     "PID",
				JSONPath: `{.pid}`,
			},
			{
				Name:     "Exit Code",
				JSONPath: `{.exitCode}`,
			},
		},
	}
}

func init() {
	proto.RegisterDefaultTypes()

	if err := protobuf.RegisterDynamic(ContainerInstanceSpecType, &ContainerInstanceSpec{}); err != nil {
		panic(err)
	}

	if err := protobuf.RegisterDynamic(ContainerInstanceStatusType, &ContainerInstanceStatus{}); err != nil {
		panic(err)
	}
}
