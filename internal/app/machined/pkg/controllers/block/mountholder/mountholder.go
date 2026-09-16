// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package mountholder holds volume mounts on behalf of a controller.
//
// A controller which needs a volume mounted creates a block.VolumeMountRequest and takes a
// finalizer on the resulting block.VolumeMountStatus. The finalizer is what stops the volume being
// unmounted while it is in use, so giving a mount back means dropping the finalizer first and only
// then destroying the request.
//
// A controller using this must declare block.VolumeMountRequest as an InputDestroyReady input, so
// it wakes when a request it tore down has no finalizers left, and as an OutputShared output, since
// many controllers write mount requests.
package mountholder

import (
	"context"
	"fmt"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/pkg/machinery/resources/block"
)

// Holder gives one controller's mount requests and the finalizers it holds on them.
//
// Requester is the controller name written to block.VolumeMountRequest.Requester, which is both how
// a request is attributed back to its controller and the finalizer this holder adds and removes.
type Holder struct {
	Requester string
}

// OwnedRequests returns the mount requests created by the holder's controller.
func (h Holder) OwnedRequests(ctx context.Context, runtime controller.Runtime) ([]*block.VolumeMountRequest, error) {
	volumeMountRequests, err := safe.ReaderListAll[*block.VolumeMountRequest](ctx, runtime)
	if err != nil {
		return nil, fmt.Errorf("failed to list mount requests: %w", err)
	}

	var ownedVolumeMountRequests []*block.VolumeMountRequest

	for volumeMountRequest := range volumeMountRequests.All() {
		if volumeMountRequest.TypedSpec().Requester == h.Requester {
			ownedVolumeMountRequests = append(ownedVolumeMountRequests, volumeMountRequest)
		}
	}

	return ownedVolumeMountRequests, nil
}

// Release gives back one mount: the finalizer first, then the request itself.
func (h Holder) Release(ctx context.Context, runtime controller.Runtime, logger *zap.Logger, requestID string) error {
	if err := h.ReleaseFinalizer(ctx, runtime, logger, requestID); err != nil {
		return err
	}

	return h.DestroyRequest(ctx, runtime, logger, requestID)
}

// ReleaseFinalizer drops the holder's hold on the mount status, if it holds one.
func (h Holder) ReleaseFinalizer(ctx context.Context, runtime controller.Runtime, logger *zap.Logger, requestID string) error {
	volumeMountStatus, err := safe.ReaderGetByID[*block.VolumeMountStatus](ctx, runtime, requestID)
	if err != nil {
		if state.IsNotFoundError(err) {
			return nil
		}

		return fmt.Errorf("failed to get mount status %q: %w", requestID, err)
	}

	if !volumeMountStatus.Metadata().Finalizers().Has(h.Requester) {
		return nil
	}

	if err := runtime.RemoveFinalizer(ctx, volumeMountStatus.Metadata(), h.Requester); err != nil {
		return fmt.Errorf("failed to remove finalizer on %q: %w", requestID, err)
	}

	logger.Info("released volume mount", zap.String("request", requestID))

	return nil
}

// DestroyRequest tears down the mount request and destroys it once nothing holds it.
func (h Holder) DestroyRequest(ctx context.Context, runtime controller.Runtime, logger *zap.Logger, requestID string) error {
	requestMD := block.NewVolumeMountRequest(block.NamespaceName, requestID).Metadata()

	okToDestroy, err := runtime.Teardown(ctx, requestMD)
	if err != nil {
		if state.IsNotFoundError(err) {
			return nil
		}

		return fmt.Errorf("failed to tear down mount request %q: %w", requestID, err)
	}

	if !okToDestroy {
		logger.Debug("waiting for the volume mount request to be released", zap.String("request", requestID))

		return nil
	}

	if err := runtime.Destroy(ctx, requestMD); err != nil && !state.IsNotFoundError(err) {
		return fmt.Errorf("failed to destroy mount request %q: %w", requestID, err)
	}

	return nil
}
