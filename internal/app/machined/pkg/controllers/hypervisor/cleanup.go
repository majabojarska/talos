// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package hypervisor

import (
	"context"
	"fmt"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/controller/generic"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
)

// cleanupOutputs tears down and destroys every output of type T whose ID is no longer desired, or
// which is already leaving. what names the resource in the error messages.
//
// The hypervisor controllers cannot use safe.CleanupOutputs for this: they deliberately leave an
// output untouched when its virtual machine is held back by a host resource that has not appeared
// yet, and output tracking would read that skipped write as an output to destroy.
func cleanupOutputs[T generic.ResourceWithRD](
	ctx context.Context,
	runtime controller.Runtime,
	what string,
	desired map[resource.ID]struct{},
) error {
	outputs, err := safe.ReaderListAll[T](ctx, runtime)
	if err != nil {
		return fmt.Errorf("failed to list %s: %w", what, err)
	}

	for output := range outputs.All() {
		if _, wanted := desired[output.Metadata().ID()]; wanted && output.Metadata().Phase() == resource.PhaseRunning {
			continue
		}

		ready, teardownErr := runtime.Teardown(ctx, output.Metadata())
		if teardownErr != nil {
			return fmt.Errorf("failed to tear down %s %q: %w", what, output.Metadata().ID(), teardownErr)
		}

		if !ready {
			continue
		}

		if destroyErr := runtime.Destroy(ctx, output.Metadata()); destroyErr != nil {
			return fmt.Errorf("failed to destroy %s %q: %w", what, output.Metadata().ID(), destroyErr)
		}
	}

	return nil
}
