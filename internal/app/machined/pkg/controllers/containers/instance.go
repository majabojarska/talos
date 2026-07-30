// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers

import (
	"context"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/pkg/machinery/resources/containers"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
	timeres "github.com/siderolabs/talos/pkg/machinery/resources/time"
	"github.com/siderolabs/talos/pkg/machinery/resources/v1alpha1"
)

const (
	// RestartInterval is how long to wait after an instance terminates before starting the next one.
	//
	// Flat, with no exponential backoff, matching what extension services use today. A container
	// that keeps failing keeps retrying at this rate for the life of the node; restartCount and the
	// recorded error are what make that visible.
	RestartInterval = 5 * time.Second

	// MaxRetainedInstances is how many terminated instances are kept per container.
	//
	// Retaining them rather than destroying them immediately is what makes the generation scheme
	// work: the next generation is max(existing)+1, so no counter has to be persisted anywhere, and
	// creating the next instance never waits for the previous one to be destroyed. It also means
	// crash history is inspectable.
	MaxRetainedInstances = 5

	// pathPollInterval is how often to re-check dependsOn.paths entries.
	//
	// Paths are the one dependency with no COSI equivalent, so they have to be polled. This is a
	// timer rather than a goroutine: the controller stays a pure function of its inputs.
	pathPollInterval = time.Second
)

// InstanceController decides when a container should be running.
//
// It owns no side effects and holds no containerd client or goroutine: given a spec, the gating
// statuses and the outcome of previous instances, it decides whether a ContainerInstanceSpec should
// exist. That makes dependency gating and restart timing testable without any infrastructure.
type InstanceController struct{}

// Name implements controller.Controller interface.
func (ctrl *InstanceController) Name() string {
	return "containers.InstanceController"
}

// Inputs implements controller.Controller interface.
func (ctrl *InstanceController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerSpecType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerImageStatusType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerMountStatusType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerInstanceStatusType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerStatusType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: containers.NamespaceName,
			Type:      containers.ContainerInstanceSpecType,
			Kind:      controller.InputDestroyReady,
		},
		{
			Namespace: network.NamespaceName,
			Type:      network.StatusType,
			ID:        optional.Some(network.StatusID),
			Kind:      controller.InputWeak,
		},
		{
			Namespace: v1alpha1.NamespaceName,
			Type:      timeres.StatusType,
			ID:        optional.Some(timeres.StatusID),
			Kind:      controller.InputWeak,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *InstanceController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: containers.ContainerInstanceSpecType,
			Kind: controller.OutputExclusive,
		},
	}
}

// Run implements controller.Controller interface.
func (ctrl *InstanceController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	// A single timer serves both the restart delay and path polling: it is reset each pass to the
	// earliest deadline anything is waiting on, so an idle node does no work at all.
	timer := time.NewTimer(0)
	defer timer.Stop()

	if !timer.Stop() {
		<-timer.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		case <-timer.C:
		}

		wakeAfter, err := ctrl.reconcile(ctx, r, logger)
		if err != nil {
			logger.Error("failed to reconcile container instances", zap.Error(err))

			return err
		}

		if !timer.Stop() {
			// Drain a timer that fired while we were reconciling, so the next Reset is honored.
			select {
			case <-timer.C:
			default:
			}
		}

		if deadline, ok := wakeAfter.Get(); ok {
			timer.Reset(deadline)
		}

		r.ResetRestartBackoff()
	}
}

// reconcile returns how long until the controller next needs to wake up on its own, if at all.
//
//nolint:gocyclo,cyclop
func (ctrl *InstanceController) reconcile(ctx context.Context, r controller.Runtime, logger *zap.Logger) (optional.Optional[time.Duration], error) {
	specs, err := safe.ReaderListAll[*containers.ContainerSpec](ctx, r)
	if err != nil {
		return optional.None[time.Duration](), fmt.Errorf("failed to list container specs: %w", err)
	}

	instances, err := safe.ReaderListAll[*containers.ContainerInstanceSpec](ctx, r)
	if err != nil {
		return optional.None[time.Duration](), fmt.Errorf("failed to list container instances: %w", err)
	}

	// Group instances by owning container so each container can be reasoned about independently.
	instancesByContainer := map[string][]*containers.ContainerInstanceSpec{}

	for instance := range instances.All() {
		containerID := instance.TypedSpec().ContainerID
		instancesByContainer[containerID] = append(instancesByContainer[containerID], instance)
	}

	for _, list := range instancesByContainer {
		slices.SortFunc(list, func(a, b *containers.ContainerInstanceSpec) int {
			return int(a.TypedSpec().Generation) - int(b.TypedSpec().Generation)
		})
	}

	gates, err := loadGates(ctx, r)
	if err != nil {
		return optional.None[time.Duration](), err
	}

	var earliest optional.Optional[time.Duration]

	specIDs := map[string]struct{}{}

	for spec := range specs.All() {
		specIDs[spec.Metadata().ID()] = struct{}{}

		wakeAfter, err := ctrl.reconcileContainer(ctx, r, logger, spec, instancesByContainer[spec.Metadata().ID()], gates)
		if err != nil {
			return optional.None[time.Duration](), err
		}

		earliest = earlier(earliest, wakeAfter)
	}

	// Instances whose container is gone: stop them, then remove them.
	for containerID, list := range instancesByContainer {
		if _, exists := specIDs[containerID]; exists {
			continue
		}

		for _, instance := range list {
			logger.Debug("removing instance of a container that no longer exists",
				zap.String("container", containerID),
				zap.String("instance", instance.Metadata().ID()),
			)

			if err := ctrl.destroyInstance(ctx, r, logger, instance); err != nil {
				return optional.None[time.Duration](), err
			}
		}
	}

	return earliest, nil
}

//nolint:gocyclo,cyclop
func (ctrl *InstanceController) reconcileContainer(
	ctx context.Context,
	r controller.Runtime,
	logger *zap.Logger,
	spec *containers.ContainerSpec,
	instances []*containers.ContainerInstanceSpec,
	gates gateInputs,
) (optional.Optional[time.Duration], error) {
	containerID := spec.Metadata().ID()

	none := optional.None[time.Duration]()

	// Garbage-collect terminated instances beyond the retention window, oldest first. Only
	// terminated ones are eligible: a live instance is never removed here, only by the paths below.
	if err := ctrl.collectOldInstances(ctx, r, logger, instances); err != nil {
		return none, err
	}

	newest := newestInstance(instances)

	// A live instance means nothing to do: the runtime controller owns it until it terminates.
	if newest != nil {
		status, err := safe.ReaderGetByID[*containers.ContainerInstanceStatus](ctx, r, newest.Metadata().ID())
		if err != nil && !state.IsNotFoundError(err) {
			return none, fmt.Errorf("failed to get instance status %q: %w", newest.Metadata().ID(), err)
		}

		// A spec change invalidates the running instance: destroy it, and the next pass starts a
		// fresh generation from the new spec.
		if specChanged(spec, newest) {
			logger.Info("container spec changed, restarting",
				zap.String("container", containerID),
				zap.Uint64("generation", newest.TypedSpec().Generation),
			)

			if err := ctrl.destroyInstance(ctx, r, logger, newest); err != nil {
				return none, err
			}

			return none, nil
		}

		// A volume going away revokes the mounts the instance is using. The mount controller cannot
		// release its finalizer until the container stops, and nothing else would tell us to stop
		// it, so this is what breaks that deadlock.
		mountsRevoked, err := mountsNoLongerReady(ctx, r, containerID)
		if err != nil {
			return none, err
		}

		if mountsRevoked {
			logger.Info("container mounts are no longer available, stopping",
				zap.String("container", containerID),
				zap.Uint64("generation", newest.TypedSpec().Generation),
			)

			if err := ctrl.destroyInstance(ctx, r, logger, newest); err != nil {
				return none, err
			}

			return none, nil
		}

		if status == nil || !status.TypedSpec().Phase.Done() {
			// Still starting or running.
			return none, nil
		}

		// Terminated: wait out the restart interval measured from when it finished.
		finishedAt := status.TypedSpec().FinishedAt
		if finishedAt.IsZero() {
			finishedAt = time.Now()
		}

		if remaining := RestartInterval - time.Since(finishedAt); remaining > 0 {
			logger.Debug("waiting before restarting container",
				zap.String("container", containerID),
				zap.Uint64("generation", newest.TypedSpec().Generation),
				zap.Duration("remaining", remaining),
			)

			return optional.Some(remaining), nil
		}

		logger.Info("restarting container",
			zap.String("container", containerID),
			zap.Uint64("generation", newest.TypedSpec().Generation),
			zap.Int32("exitCode", status.TypedSpec().ExitCode),
		)
	}

	// Nothing live, so decide whether a new instance may start.
	ready, waitingFor, wakeAfter := evaluateGates(ctx, r, spec, gates)
	if !ready {
		logger.Debug("container is waiting on dependencies",
			zap.String("container", containerID),
			zap.Strings("waitingFor", waitingFor),
		)

		return wakeAfter, nil
	}

	generation := uint64(0)
	if newest != nil {
		generation = newest.TypedSpec().Generation + 1
	}

	image, err := ctrl.resolvedImage(ctx, r, containerID)
	if err != nil {
		return none, err
	}

	mounts, err := ctrl.resolvedMounts(ctx, r, containerID)
	if err != nil {
		return none, err
	}

	id := containers.InstanceID(containerID, generation)

	if err := safe.WriterModify(ctx, r,
		containers.NewContainerInstanceSpec(containers.NamespaceName, id),
		func(res *containers.ContainerInstanceSpec) error {
			instanceSpec := res.TypedSpec()

			instanceSpec.ContainerID = containerID
			instanceSpec.Generation = generation
			instanceSpec.Image = image
			instanceSpec.Entrypoint = spec.TypedSpec().Entrypoint
			instanceSpec.Args = spec.TypedSpec().Args
			instanceSpec.WorkingDir = spec.TypedSpec().WorkingDir
			instanceSpec.User = spec.TypedSpec().User
			instanceSpec.Environment = spec.TypedSpec().Environment
			instanceSpec.Mounts = mounts
			instanceSpec.Security = spec.TypedSpec().Security
			instanceSpec.Network = spec.TypedSpec().Network
			instanceSpec.Resources = spec.TypedSpec().Resources

			return nil
		},
	); err != nil {
		return none, fmt.Errorf("failed to write instance spec %q: %w", id, err)
	}

	logger.Info("container instance created",
		zap.String("container", containerID),
		zap.Uint64("generation", generation),
		zap.String("image", image),
	)

	return none, nil
}

// destroyInstance tears down and destroys an instance, waiting for the runtime controller to
// release its finalizer first.
func (ctrl *InstanceController) destroyInstance(
	ctx context.Context,
	r controller.Runtime,
	logger *zap.Logger,
	instance *containers.ContainerInstanceSpec,
) error {
	id := instance.Metadata().ID()

	okToDestroy, err := r.Teardown(ctx, instance.Metadata())
	if err != nil {
		if state.IsNotFoundError(err) {
			return nil
		}

		return fmt.Errorf("failed to tear down instance %q: %w", id, err)
	}

	if !okToDestroy {
		// The runtime controller still holds a finalizer: it is stopping the task. Come back when
		// it releases, which the InputDestroyReady input will wake us for.
		logger.Debug("waiting for the container instance to stop", zap.String("instance", id))

		return nil
	}

	if err := r.Destroy(ctx, instance.Metadata()); err != nil && !state.IsNotFoundError(err) {
		return fmt.Errorf("failed to destroy instance %q: %w", id, err)
	}

	logger.Debug("container instance destroyed", zap.String("instance", id))

	return nil
}

// collectOldInstances destroys terminated instances beyond the retention window.
func (ctrl *InstanceController) collectOldInstances(
	ctx context.Context,
	r controller.Runtime,
	logger *zap.Logger,
	instances []*containers.ContainerInstanceSpec,
) error {
	if len(instances) <= MaxRetainedInstances {
		return nil
	}

	// instances is sorted oldest-first; never touch the newest, which may be live.
	excess := len(instances) - MaxRetainedInstances

	for _, instance := range instances[:excess] {
		status, err := safe.ReaderGetByID[*containers.ContainerInstanceStatus](ctx, r, instance.Metadata().ID())
		if err != nil && !state.IsNotFoundError(err) {
			return fmt.Errorf("failed to get instance status %q: %w", instance.Metadata().ID(), err)
		}

		// Only collect instances that are actually finished.
		if status != nil && !status.TypedSpec().Phase.Done() {
			continue
		}

		logger.Debug("collecting a retired container instance", zap.String("instance", instance.Metadata().ID()))

		if err := ctrl.destroyInstance(ctx, r, logger, instance); err != nil {
			return err
		}
	}

	return nil
}

func (ctrl *InstanceController) resolvedImage(ctx context.Context, r controller.Runtime, containerID string) (string, error) {
	imageStatus, err := safe.ReaderGetByID[*containers.ContainerImageStatus](ctx, r, containerID)
	if err != nil {
		return "", fmt.Errorf("failed to get image status %q: %w", containerID, err)
	}

	// Prefer the resolved digest: an instance should run exactly the bytes the pull produced, even
	// if a mutable tag moves underneath it afterwards.
	if digest := imageStatus.TypedSpec().Digest; digest != "" {
		return digest, nil
	}

	return imageStatus.TypedSpec().Image, nil
}

func (ctrl *InstanceController) resolvedMounts(ctx context.Context, r controller.Runtime, containerID string) ([]containers.ResolvedMountSpec, error) {
	mountStatus, err := safe.ReaderGetByID[*containers.ContainerMountStatus](ctx, r, containerID)
	if err != nil {
		return nil, fmt.Errorf("failed to get mount status %q: %w", containerID, err)
	}

	return mountStatus.TypedSpec().Mounts, nil
}

// gateInputs holds the node-wide gating resources, read once per reconcile.
type gateInputs struct {
	network *network.Status
	time    *timeres.Status
}

func loadGates(ctx context.Context, r controller.Runtime) (gateInputs, error) {
	var gates gateInputs

	networkStatus, err := safe.ReaderGetByID[*network.Status](ctx, r, network.StatusID)
	if err != nil && !state.IsNotFoundError(err) {
		return gates, fmt.Errorf("failed to get network status: %w", err)
	}

	gates.network = networkStatus

	timeStatus, err := safe.ReaderGetByID[*timeres.Status](ctx, r, timeres.StatusID)
	if err != nil && !state.IsNotFoundError(err) {
		return gates, fmt.Errorf("failed to get time status: %w", err)
	}

	gates.time = timeStatus

	return gates, nil
}

// evaluateGates reports whether a container may start, and if not, what it is waiting on.
//
// Shared by InstanceController, which uses it to decide, and StatusController, which uses it to
// report. One implementation means the decision and the reported reason cannot disagree.
//
// The returned duration is set when something needs re-checking on a timer rather than an event,
// which today means only dependsOn.paths.
//
//nolint:gocyclo,cyclop
func evaluateGates(
	ctx context.Context,
	r controller.Runtime,
	spec *containers.ContainerSpec,
	gates gateInputs,
) (bool, []string, optional.Optional[time.Duration]) {
	var (
		waitingFor []string
		wakeAfter  optional.Optional[time.Duration]
	)

	containerID := spec.Metadata().ID()

	imageStatus, err := safe.ReaderGetByID[*containers.ContainerImageStatus](ctx, r, containerID)
	if err != nil || imageStatus.TypedSpec().Phase != containers.ContainerImagePhaseReady {
		waitingFor = append(waitingFor, "image")
	}

	mountStatus, err := safe.ReaderGetByID[*containers.ContainerMountStatus](ctx, r, containerID)
	if err != nil || !mountStatus.TypedSpec().Ready {
		waitingFor = append(waitingFor, "mounts")
	}

	dependsOn := spec.TypedSpec().DependsOn

	for _, condition := range dependsOn.Networks {
		if !networkConditionMet(gates.network, condition) {
			waitingFor = append(waitingFor, "network: "+condition)
		}
	}

	if dependsOn.Time {
		// A node with time sync disabled would otherwise wait forever, so treat that as satisfied.
		if gates.time == nil || !(gates.time.TypedSpec().Synced || gates.time.TypedSpec().SyncDisabled) {
			waitingFor = append(waitingFor, "time")
		}
	}

	for _, peer := range dependsOn.Containers {
		peerStatus, err := safe.ReaderGetByID[*containers.ContainerStatus](ctx, r, peer)
		if err != nil || peerStatus.TypedSpec().State != containers.ContainerStateRunning {
			waitingFor = append(waitingFor, "container: "+peer)
		}
	}

	for _, path := range dependsOn.Paths {
		if _, err := os.Stat(path); err != nil {
			waitingFor = append(waitingFor, "path: "+path)
		}
	}

	if len(dependsOn.Paths) > 0 {
		// Paths have no event to wake us, so poll while any are declared.
		wakeAfter = optional.Some(pathPollInterval)
	}

	return len(waitingFor) == 0, waitingFor, wakeAfter
}

func networkConditionMet(status *network.Status, condition string) bool {
	if status == nil {
		return false
	}

	spec := status.TypedSpec()

	switch condition {
	case "addresses":
		return spec.AddressReady
	case "connectivity":
		return spec.ConnectivityReady
	case "hostname":
		return spec.HostnameReady
	case "etcfiles":
		return spec.EtcFilesReady
	default:
		// Validation rejects unknown conditions, so this is unreachable from configuration.
		return false
	}
}

// specChanged reports whether the container spec no longer matches what the instance was built from.
//
// The instance carries a resolved snapshot precisely so this comparison is possible: a running
// container is never mutated in place, it is replaced.
func specChanged(spec *containers.ContainerSpec, instance *containers.ContainerInstanceSpec) bool {
	s := spec.TypedSpec()
	i := instance.TypedSpec()

	return !slices.Equal(s.Entrypoint, i.Entrypoint) ||
		!slices.Equal(s.Args, i.Args) ||
		s.WorkingDir != i.WorkingDir ||
		s.User != i.User ||
		!slices.Equal(s.Environment, i.Environment) ||
		s.Security.Privileged != i.Security.Privileged ||
		!slices.Equal(s.Security.CapabilitiesAdd, i.Security.CapabilitiesAdd) ||
		!slices.Equal(s.Security.CapabilitiesDrop, i.Security.CapabilitiesDrop) ||
		s.Network != i.Network ||
		s.Resources != i.Resources
}

// mountsNoLongerReady reports whether a container's mounts have been revoked after it started.
//
// A missing mount status is not treated as revocation: it means the mount controller has not caught
// up yet, and tearing down a healthy container over a transient gap would be worse than waiting.
func mountsNoLongerReady(ctx context.Context, r controller.Runtime, containerID string) (bool, error) {
	mountStatus, err := safe.ReaderGetByID[*containers.ContainerMountStatus](ctx, r, containerID)
	if err != nil {
		if state.IsNotFoundError(err) {
			return false, nil
		}

		return false, fmt.Errorf("failed to get mount status %q: %w", containerID, err)
	}

	return !mountStatus.TypedSpec().Ready, nil
}

func newestInstance(instances []*containers.ContainerInstanceSpec) *containers.ContainerInstanceSpec {
	if len(instances) == 0 {
		return nil
	}

	return instances[len(instances)-1]
}

func earlier(a, b optional.Optional[time.Duration]) optional.Optional[time.Duration] {
	aVal, aOK := a.Get()
	bVal, bOK := b.Get()

	switch {
	case !aOK:
		return b
	case !bOK:
		return a
	case bVal < aVal:
		return b
	default:
		return a
	}
}
