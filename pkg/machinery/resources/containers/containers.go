// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package containers provides resources for containers declared via ContainerConfig.
//
// These containers are run directly by Talos, without Kubernetes and without registering a Talos
// service. The resource chain is:
//
//	ContainerConfig (machine config) -> ContainerSpec -> ContainerInstanceSpec -> ContainerInstanceStatus
//
// with ContainerImageStatus and ContainerMountStatus gating the step from spec to instance, and
// ContainerStatus as the aggregated user-facing surface.
package containers

import (
	"fmt"

	"github.com/cosi-project/runtime/pkg/resource"
)

//go:generate go tool github.com/siderolabs/deep-copy -type ContainerSpecSpec -type ContainerImageStatusSpec -type ContainerMountStatusSpec -type ContainerInstanceSpecSpec -type ContainerInstanceStatusSpec -type ContainerStatusSpec -type ContainerLifecycleSpec -header-file ../../../../hack/boilerplate.txt -o deep_copy.generated.go .

//go:generate go tool github.com/dmarkham/enumer -type=ContainerState,ContainerHealth,ContainerImagePhase,ContainerInstancePhase -linecomment -text

// NamespaceName contains resources for Talos-managed containers.
const NamespaceName resource.Namespace = "containers"

// InstanceID builds the ID of a ContainerInstanceSpec/Status from a container name and generation.
//
// Generations are numbered rather than reusing the container name so that creating the next
// instance never has to wait for the previous one to be destroyed. See ParseInstanceID for the
// inverse.
func InstanceID(container string, generation uint64) resource.ID {
	return fmt.Sprintf("%s-%d", container, generation)
}
