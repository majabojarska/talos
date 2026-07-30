// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers

import (
	"context"
	"fmt"
	"sync"
	"time"

	containerdapi "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/dustin/go-humanize"
	"github.com/siderolabs/gen/channel"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/internal/app/machined/pkg/runtime"
	"github.com/siderolabs/talos/internal/pkg/containers/image"
	"github.com/siderolabs/talos/internal/pkg/containers/image/progress"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/containers"
	"github.com/siderolabs/talos/pkg/machinery/resources/cri"
	"github.com/siderolabs/talos/pkg/machinery/resources/v1alpha1"
)

// criServiceID is the Talos service running the CRI containerd instance.
const criServiceID = "cri"

// progressReportInterval throttles pull progress writes.
//
// image.Pull reports per-layer progress continuously; writing every update straight to COSI would
// spin every watcher on the type for the duration of the pull.
const progressReportInterval = time.Second

// Puller pulls an image into the taloscontainers namespace.
//
// This is the seam that keeps the controller testable: the default implementation talks to
// containerd, tests substitute a fake.
type Puller interface {
	// Pull fetches the reference and returns the resolved digest reference.
	Pull(ctx context.Context, ref string, report func(string)) (string, error)
	// Close releases the underlying client.
	Close() error
}

// ImageController owns pulling images for declared containers.
//
// The pull is the one side effect this controller owns. It runs in a goroutine per container
// because image.Pull blocks for up to PullTimeout (20 minutes) and retries internally; doing it
// inline would stall every other container and stop the controller reacting to events. Unlike the
// runtime controller's goroutine this is bounded work rather than process supervision.
type ImageController struct {
	// Runtime provides access to the COSI state, needed to resolve registry configuration.
	Runtime runtime.Runtime

	// PullerProvider is overridable for testing.
	PullerProvider func() (Puller, error)

	pulls map[string]*pullState
}

// Name implements controller.Controller interface.
func (ctrl *ImageController) Name() string {
	return "containers.ImageController"
}

// Inputs implements controller.Controller interface.
func (ctrl *ImageController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerSpecType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: v1alpha1.NamespaceName,
			Type:      v1alpha1.ServiceType,
			ID:        optional.Some(criServiceID),
			Kind:      controller.InputWeak,
		},
		{
			Namespace: cri.NamespaceName,
			Type:      cri.RegistriesConfigType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: cri.NamespaceName,
			Type:      cri.ImageCacheConfigType,
			Kind:      controller.InputWeak,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *ImageController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: containers.ContainerImageStatusType,
			Kind: controller.OutputExclusive,
		},
	}
}

// pullState tracks one in-flight pull.
type pullState struct {
	ref string

	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	progress string
	digest   string
	err      error
	done     bool
}

func (s *pullState) snapshot() (progressText, digest string, err error, done bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.progress, s.digest, s.err, s.done
}

func (s *pullState) setProgress(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.progress = text
}

func (s *pullState) finish(digest string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.digest, s.err, s.done = digest, err, true
}

func (s *pullState) stop() {
	s.cancel()
	s.wg.Wait()
}

// Run implements controller.Controller interface.
//
//nolint:gocyclo,cyclop
func (ctrl *ImageController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	if ctrl.PullerProvider == nil {
		ctrl.PullerProvider = ctrl.defaultPullerProvider
	}

	ctrl.pulls = map[string]*pullState{}

	defer func() {
		for _, pull := range ctrl.pulls {
			pull.stop()
		}
	}()

	notifyCh := make(chan struct{}, 1)

	var puller Puller

	defer func() {
		if puller != nil {
			puller.Close() //nolint:errcheck
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		case <-notifyCh:
		}

		// Nothing can be pulled until the CRI containerd instance is up, since that is the socket
		// the taloscontainers namespace lives on.
		criUp, err := ctrl.criIsUp(ctx, r)
		if err != nil {
			return err
		}

		if criUp && puller == nil {
			if puller, err = ctrl.PullerProvider(); err != nil {
				logger.Error("failed to create image puller", zap.Error(err))

				return fmt.Errorf("failed to create image puller: %w", err)
			}

			logger.Info("connected to the container runtime, image pulls enabled")
		}

		if err := ctrl.reconcile(ctx, r, logger, puller, notifyCh); err != nil {
			logger.Error("failed to reconcile container images", zap.Error(err))

			return err
		}

		r.ResetRestartBackoff()
	}
}

func (ctrl *ImageController) criIsUp(ctx context.Context, r controller.Runtime) (bool, error) {
	service, err := safe.ReaderGetByID[*v1alpha1.Service](ctx, r, criServiceID)
	if err != nil {
		if state.IsNotFoundError(err) {
			return false, nil
		}

		return false, fmt.Errorf("failed to get %q service: %w", criServiceID, err)
	}

	return service.TypedSpec().Running && service.TypedSpec().Healthy, nil
}

//nolint:gocyclo,cyclop
func (ctrl *ImageController) reconcile(
	ctx context.Context,
	r controller.Runtime,
	logger *zap.Logger,
	puller Puller,
	notifyCh chan struct{},
) error {
	specs, err := safe.ReaderListAll[*containers.ContainerSpec](ctx, r)
	if err != nil {
		return fmt.Errorf("failed to list container specs: %w", err)
	}

	r.StartTrackingOutputs()

	wanted := map[string]struct{}{}

	for spec := range specs.All() {
		containerID := spec.Metadata().ID()
		ref := spec.TypedSpec().Image

		wanted[containerID] = struct{}{}

		pull, exists := ctrl.pulls[containerID]

		// A changed reference invalidates an in-flight or completed pull.
		if exists && pull.ref != ref {
			logger.Info("container image reference changed, restarting the pull",
				zap.String("container", containerID),
				zap.String("from", pull.ref),
				zap.String("to", ref),
			)

			pull.stop()
			delete(ctrl.pulls, containerID)

			exists = false
		}

		if !exists {
			if puller == nil {
				// Waiting for the CRI service; report pending so the operator can see why.
				logger.Debug("waiting for the container runtime before pulling",
					zap.String("container", containerID),
					zap.String("image", ref),
				)

				if err := ctrl.writeStatus(ctx, r, containerID, ref, containers.ContainerImagePhasePending, "", "", ""); err != nil {
					return err
				}

				continue
			}

			pull = ctrl.startPull(ctx, logger, puller, containerID, ref, notifyCh)
			ctrl.pulls[containerID] = pull
		}

		progressText, digest, pullErr, done := pull.snapshot()

		switch {
		case !done:
			logger.Debug("image pull in progress",
				zap.String("container", containerID),
				zap.String("image", ref),
				zap.String("progress", progressText),
			)

			if err := ctrl.writeStatus(ctx, r, containerID, ref, containers.ContainerImagePhasePulling, "", progressText, ""); err != nil {
				return err
			}
		case pullErr != nil:
			if err := ctrl.writeStatus(ctx, r, containerID, ref, containers.ContainerImagePhaseFailed, "", "", pullErr.Error()); err != nil {
				return err
			}
		default:
			if err := ctrl.writeStatus(ctx, r, containerID, ref, containers.ContainerImagePhaseReady, digest, "", ""); err != nil {
				return err
			}
		}
	}

	// Stop pulls for containers that are gone.
	for containerID, pull := range ctrl.pulls {
		if _, exists := wanted[containerID]; exists {
			continue
		}

		logger.Info("container is gone, abandoning its image pull", zap.String("container", containerID))

		pull.stop()
		delete(ctrl.pulls, containerID)
	}

	return safe.CleanupOutputs[*containers.ContainerImageStatus](ctx, r)
}

func (ctrl *ImageController) writeStatus(
	ctx context.Context,
	r controller.Runtime,
	containerID, ref string,
	phase containers.ContainerImagePhase,
	digest, progressText, errText string,
) error {
	if err := safe.WriterModify(ctx, r,
		containers.NewContainerImageStatus(containers.NamespaceName, containerID),
		func(res *containers.ContainerImageStatus) error {
			res.TypedSpec().Phase = phase
			res.TypedSpec().Image = ref
			res.TypedSpec().Digest = digest
			res.TypedSpec().Progress = progressText
			res.TypedSpec().Error = errText

			return nil
		},
	); err != nil {
		return fmt.Errorf("failed to write image status %q: %w", containerID, err)
	}

	return nil
}

// startPull launches the pull for one container.
//
// A failed pull is not retried here: the instance controller never opens the gate, the container
// stays visible as failed, and the operator sees the error. Retrying is the restart path's job,
// which re-enters this controller when the reference or the spec changes.
func (ctrl *ImageController) startPull(
	ctx context.Context,
	logger *zap.Logger,
	puller Puller,
	containerID, ref string,
	notifyCh chan struct{},
) *pullState {
	pull := &pullState{ref: ref}

	pullCtx, cancel := context.WithCancel(ctx)
	pull.cancel = cancel

	pull.wg.Go(func() {
		defer func() {
			// A panic in a pull must not take down machined.
			if p := recover(); p != nil {
				pull.finish("", fmt.Errorf("panic: %v", p))

				logger.Error("image pull panicked", zap.Stack("stack"), zap.String("container", containerID))
			}

			channel.SendWithContext(pullCtx, notifyCh, struct{}{})
		}()

		logger.Info("pulling container image", zap.String("container", containerID), zap.String("image", ref))

		var lastReport time.Time

		digest, err := puller.Pull(pullCtx, ref, func(text string) {
			// Throttle: a pull reports continuously, and each report would otherwise be a COSI write.
			if time.Since(lastReport) < progressReportInterval {
				return
			}

			lastReport = time.Now()

			pull.setProgress(text)

			channel.SendWithContext(pullCtx, notifyCh, struct{}{})
		})

		pull.finish(digest, err)

		if err != nil {
			logger.Error("image pull failed",
				zap.String("container", containerID),
				zap.String("image", ref),
				zap.Error(err),
			)
		} else {
			logger.Info("image pulled",
				zap.String("container", containerID),
				zap.String("digest", digest),
			)
		}
	})

	return pull
}

// defaultPullerProvider dials the CRI containerd instance and pulls through image.Pull.
func (ctrl *ImageController) defaultPullerProvider() (Puller, error) {
	client, err := containerdapi.New(constants.CRIContainerdAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to containerd: %w", err)
	}

	st := ctrl.Runtime.State().V1Alpha2().Resources()

	return &containerdPuller{
		client:          client,
		state:           st,
		registryBuilder: cri.RegistryBuilder(st),
	}, nil
}

type containerdPuller struct {
	client          *containerdapi.Client
	state           state.State
	registryBuilder image.RegistriesBuilder
}

func (p *containerdPuller) Pull(ctx context.Context, ref string, report func(string)) (string, error) {
	// The taloscontainers namespace keeps these images away from both Kubernetes pods and Talos'
	// own system images.
	ctx = namespaces.WithNamespace(ctx, constants.TalosContainersNamespace)

	img, err := image.Pull(ctx, p.registryBuilder, p.state, p.client, ref,
		// IfNotPresent semantics: an image already on the node is not re-fetched, which also keeps a
		// crash-looping container from hammering the registry on every restart.
		image.WithSkipIfAlreadyPulled(),
		image.WithProgressReporter(image.NewSimpleProgressReporter(func(layer progress.LayerPullProgress) {
			report(formatProgress(layer))
		})),
	)
	if err != nil {
		return "", err
	}

	return img.Target().Digest.String(), nil
}

func (p *containerdPuller) Close() error {
	return p.client.Close()
}

// formatProgress renders one layer's progress as a short human-readable line.
func formatProgress(layer progress.LayerPullProgress) string {
	status := layerStatusText(layer.Status)

	if layer.Status == progress.LayerPullStatusDownloading && layer.Total > 0 {
		return fmt.Sprintf("%s %s / %s", status, humanize.IBytes(uint64(layer.Offset)), humanize.IBytes(uint64(layer.Total)))
	}

	return status
}

func layerStatusText(status progress.LayerPullStatus) string {
	switch status {
	case progress.LayerPullStatusDownloading:
		return "downloading"
	case progress.LayerPullStatusDownloadComplete:
		return "download complete"
	case progress.LayerPullStatusExtracting:
		return "extracting"
	case progress.LayerPullStatusExtractComplete:
		return "extract complete"
	case progress.LayerPullStatusAlreadyExists:
		return "already exists"
	default:
		return "pulling"
	}
}
