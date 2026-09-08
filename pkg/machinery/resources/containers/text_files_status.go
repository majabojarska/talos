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

// TextFilesStatusType is type of TextFilesStatus resource.
const TextFilesStatusType = resource.Type("TextFilesStatuses.containers.talos.dev")

// TextFilesStatus resource reports a materialized text file tree on the host.
//
// The ID is <containerID>/<documentName>; see TextFilesStatusID. One tree per container rather than
// one per document, so that the container the tree belongs to is visible in its identity and a
// container going away takes its own tree with it.
type TextFilesStatus = typed.Resource[TextFilesStatusSpec, TextFilesStatusExtension]

// TextFilesStatusID builds the ID of a TextFilesStatus.
//
// Slash-separated because both halves may contain hyphens, which would make a hyphen-joined ID
// ambiguous and let two containers collide on one status.
func TextFilesStatusID(containerID, documentName string) resource.ID {
	return containerID + "/" + documentName
}

// TextFilesStatusSpec is the spec for TextFilesStatus.
//
//gotagsrewrite:gen
type TextFilesStatusSpec struct {
	// ContainerID is the name of the owning container, i.e. the ContainerSpec ID.
	ContainerID string `yaml:"containerID" protobuf:"1"`
	// DocumentName is the name of the TextFilesConfig document the tree was built from.
	DocumentName string `yaml:"documentName" protobuf:"2"`
	// Path is the host directory holding the materialized tree; empty when Error is set.
	Path string `yaml:"path,omitempty" protobuf:"3"`
	// ContentHash identifies the materialized contents; empty when Error is set.
	ContentHash string `yaml:"contentHash,omitempty" protobuf:"4"`
	// Error describes why the tree could not be materialized.
	Error string `yaml:"error,omitempty" protobuf:"5"`
}

// NewTextFilesStatus initializes a TextFilesStatus resource.
func NewTextFilesStatus(namespace resource.Namespace, id resource.ID) *TextFilesStatus {
	return typed.NewResource[TextFilesStatusSpec, TextFilesStatusExtension](
		resource.NewMetadata(namespace, TextFilesStatusType, id, resource.VersionUndefined),
		TextFilesStatusSpec{},
	)
}

// TextFilesStatusExtension is auxiliary resource data for TextFilesStatus.
type TextFilesStatusExtension struct{}

// ResourceDefinition implements meta.ResourceDefinitionProvider interface.
func (TextFilesStatusExtension) ResourceDefinition() meta.ResourceDefinitionSpec {
	return meta.ResourceDefinitionSpec{
		Type:             TextFilesStatusType,
		Aliases:          []resource.Type{"textfilesstatus", "textfilesstatuses"},
		DefaultNamespace: NamespaceName,
		PrintColumns: []meta.PrintColumn{
			{
				Name:     "Path",
				JSONPath: `{.path}`,
			},
			{
				Name:     "Error",
				JSONPath: `{.error}`,
			},
		},
	}
}

func init() {
	proto.RegisterDefaultTypes()

	if err := protobuf.RegisterDynamic(TextFilesStatusType, &TextFilesStatus{}); err != nil {
		panic(err)
	}
}
