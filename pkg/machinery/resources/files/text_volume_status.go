// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package files

import (
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/resource/meta"
	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	"github.com/cosi-project/runtime/pkg/resource/typed"

	"github.com/siderolabs/talos/pkg/machinery/proto"
)

// TextVolumeStatusType is type of TextVolumeStatus resource.
const TextVolumeStatusType = resource.Type("TextVolumeStatuses.files.talos.dev")

// TextVolumeStatus resource reports the files of a text volume as rendered.
//
// The resource ID is the volume ID.
type TextVolumeStatus = typed.Resource[TextVolumeStatusSpec, TextVolumeStatusExtension]

// TextVolumeStatusSpec describes the rendered files of a text volume.
//
//gotagsrewrite:gen
type TextVolumeStatusSpec struct {
	// VolumeName is the name of the TextVolumeConfig document.
	VolumeName string `yaml:"volumeName" protobuf:"1"`
	// Target is the path the volume is mounted at.
	Target string `yaml:"target" protobuf:"2"`
	// ContentsHash is the hash of the rendered files.
	ContentsHash string `yaml:"contentsHash" protobuf:"3"`
	// Files is the number of rendered files.
	Files int `yaml:"files" protobuf:"4"`
}

// NewTextVolumeStatus initializes a TextVolumeStatus resource.
func NewTextVolumeStatus(namespace resource.Namespace, id resource.ID) *TextVolumeStatus {
	return typed.NewResource[TextVolumeStatusSpec, TextVolumeStatusExtension](
		resource.NewMetadata(namespace, TextVolumeStatusType, id, resource.VersionUndefined),
		TextVolumeStatusSpec{},
	)
}

// TextVolumeStatusExtension provides auxiliary methods for TextVolumeStatus.
type TextVolumeStatusExtension struct{}

// ResourceDefinition implements [typed.Extension] interface.
func (TextVolumeStatusExtension) ResourceDefinition() meta.ResourceDefinitionSpec {
	return meta.ResourceDefinitionSpec{
		Type:             TextVolumeStatusType,
		Aliases:          []resource.Type{},
		DefaultNamespace: NamespaceName,
		PrintColumns: []meta.PrintColumn{
			{
				Name:     "Volume",
				JSONPath: "{.volumeName}",
			},
			{
				Name:     "Target",
				JSONPath: "{.target}",
			},
			{
				Name:     "Files",
				JSONPath: "{.files}",
			},
			{
				Name:     "Hash",
				JSONPath: "{.contentsHash}",
			},
		},
	}
}

func init() {
	proto.RegisterDefaultTypes()

	err := protobuf.RegisterDynamic(TextVolumeStatusType, &TextVolumeStatus{})
	if err != nil {
		panic(err)
	}
}
