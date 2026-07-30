// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers

import (
	"context"
	"fmt"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/pkg/machinery/resources/containers"
)

// reconcileLifecycle holds a finalizer on the container shutdown barrier on behalf of controllerName.
//
// The barrier carries no data: the finalizer set is the payload, and the shutdown sequence blocks
// until it is empty. Every controller that owns something which must be wound down before services
// stop holds one, and releases it only once releasable reports that it has nothing left to wind down.
//
// The barrier may not exist yet, in which case there is nothing to hold and normal operation
// proceeds.
func reconcileLifecycle(ctx context.Context, r controller.Runtime, logger *zap.Logger, controllerName string, releasable bool) error {
	lifecycle, err := safe.ReaderGetByID[*containers.ContainerLifecycle](ctx, r, containers.ContainerLifecycleID)
	if err != nil {
		if state.IsNotFoundError(err) {
			return nil
		}

		return fmt.Errorf("failed to get container lifecycle: %w", err)
	}

	hasFinalizer := lifecycle.Metadata().Finalizers().Has(controllerName)

	switch lifecycle.Metadata().Phase() {
	case resource.PhaseRunning:
		if !hasFinalizer {
			if err := r.AddFinalizer(ctx, lifecycle.Metadata(), controllerName); err != nil {
				return fmt.Errorf("failed to add lifecycle finalizer: %w", err)
			}

			logger.Debug("holding the container shutdown barrier")
		}
	case resource.PhaseTearingDown:
		// Not logging the still-waiting case: it would repeat on every reconcile for the length of
		// the shutdown, and the controllers already log each thing they are winding down.
		if hasFinalizer && releasable {
			if err := r.RemoveFinalizer(ctx, lifecycle.Metadata(), controllerName); err != nil {
				return fmt.Errorf("failed to remove lifecycle finalizer: %w", err)
			}

			logger.Info("released the container shutdown barrier")
		}
	}

	return nil
}
