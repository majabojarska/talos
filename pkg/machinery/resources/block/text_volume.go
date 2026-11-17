// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package block

import (
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/resource/meta"
	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	"github.com/cosi-project/runtime/pkg/resource/typed"

	"github.com/siderolabs/talos/pkg/machinery/proto"
)

// TextVolumeRequestType is type of TextVolumeRequest resource.
const TextVolumeRequestType = resource.Type("TextVolumeRequests.block.talos.dev")

// TextVolumeRequest resource holds a request of a consumer for its own instance of a text volume.
//
// Every request gets an instance of its own: a tmpfs volume holding the files of the TextVolumeConfig
// document. The instance is reported by the TextVolumeStatus with the same ID, and mounted by the
// consumer through a VolumeMountRequest for the reported volume ID.
type TextVolumeRequest = typed.Resource[TextVolumeRequestSpec, TextVolumeRequestExtension]

// TextVolumeRequestSpec is the spec for TextVolumeRequest.
//
//gotagsrewrite:gen
type TextVolumeRequestSpec struct {
	// TextVolumeName is the name of the TextVolumeConfig document.
	TextVolumeName string `yaml:"textVolumeName" protobuf:"1"`
	// Requester is the name of the controller holding the request.
	Requester string `yaml:"requester" protobuf:"2"`
}

// NewTextVolumeRequest initializes a TextVolumeRequest resource.
func NewTextVolumeRequest(namespace resource.Namespace, id resource.ID) *TextVolumeRequest {
	return typed.NewResource[TextVolumeRequestSpec, TextVolumeRequestExtension](
		resource.NewMetadata(namespace, TextVolumeRequestType, id, resource.VersionUndefined),
		TextVolumeRequestSpec{},
	)
}

// TextVolumeRequestExtension is auxiliary resource data for TextVolumeRequest.
type TextVolumeRequestExtension struct{}

// ResourceDefinition implements meta.ResourceDefinitionProvider interface.
func (TextVolumeRequestExtension) ResourceDefinition() meta.ResourceDefinitionSpec {
	return meta.ResourceDefinitionSpec{
		Type:             TextVolumeRequestType,
		Aliases:          []resource.Type{},
		DefaultNamespace: NamespaceName,
		PrintColumns: []meta.PrintColumn{
			{
				Name:     "Text Volume",
				JSONPath: `{.textVolumeName}`,
			},
			{
				Name:     "Requester",
				JSONPath: `{.requester}`,
			},
		},
	}
}

// TextVolumeStatusType is type of TextVolumeStatus resource.
const TextVolumeStatusType = resource.Type("TextVolumeStatuses.block.talos.dev")

// TextVolumeStatus resource reports the instance of a text volume serving a TextVolumeRequest.
type TextVolumeStatus = typed.Resource[TextVolumeStatusSpec, TextVolumeStatusExtension]

// TextVolumeStatusSpec is the spec for TextVolumeStatus.
//
//gotagsrewrite:gen
type TextVolumeStatusSpec struct {
	// TextVolumeName is the name of the TextVolumeConfig document.
	TextVolumeName string `yaml:"textVolumeName" protobuf:"1"`
	// VolumeID is the ID of the tmpfs volume holding the instance.
	VolumeID string `yaml:"volumeID" protobuf:"2"`
	// Ready is set once the files have been written to the instance.
	//
	// It stays set while the content is updated in place.
	Ready bool `yaml:"ready" protobuf:"3"`
	// ContentHash is the digest of the content currently written to the instance.
	ContentHash string `yaml:"contentHash,omitempty" protobuf:"4"`
	// Error is the reason the content could not be written, if any.
	Error string `yaml:"error,omitempty" protobuf:"5"`
}

// NewTextVolumeStatus initializes a TextVolumeStatus resource.
func NewTextVolumeStatus(namespace resource.Namespace, id resource.ID) *TextVolumeStatus {
	return typed.NewResource[TextVolumeStatusSpec, TextVolumeStatusExtension](
		resource.NewMetadata(namespace, TextVolumeStatusType, id, resource.VersionUndefined),
		TextVolumeStatusSpec{},
	)
}

// TextVolumeStatusExtension is auxiliary resource data for TextVolumeStatus.
type TextVolumeStatusExtension struct{}

// ResourceDefinition implements meta.ResourceDefinitionProvider interface.
func (TextVolumeStatusExtension) ResourceDefinition() meta.ResourceDefinitionSpec {
	return meta.ResourceDefinitionSpec{
		Type:             TextVolumeStatusType,
		Aliases:          []resource.Type{},
		DefaultNamespace: NamespaceName,
		PrintColumns: []meta.PrintColumn{
			{
				Name:     "Text Volume",
				JSONPath: `{.textVolumeName}`,
			},
			{
				Name:     "Volume ID",
				JSONPath: `{.volumeID}`,
			},
			{
				Name:     "Ready",
				JSONPath: `{.ready}`,
			},
		},
	}
}

func init() {
	proto.RegisterDefaultTypes()

	if err := protobuf.RegisterDynamic(TextVolumeRequestType, &TextVolumeRequest{}); err != nil {
		panic(err)
	}

	if err := protobuf.RegisterDynamic(TextVolumeStatusType, &TextVolumeStatus{}); err != nil {
		panic(err)
	}
}
