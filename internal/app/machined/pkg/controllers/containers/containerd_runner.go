// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/cgroups/v3/cgroup2"
	containerdapi "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/siderolabs/talos/internal/app/machined/pkg/runtime"
	"github.com/siderolabs/talos/internal/pkg/capability"
	"github.com/siderolabs/talos/internal/pkg/cgroup"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	containersres "github.com/siderolabs/talos/pkg/machinery/resources/containers"
)

const (
	// gracefulShutdownTimeout is how long a container gets after SIGTERM before SIGKILL.
	//
	// Internal and not configurable, matching the containerd service runner's existing default.
	gracefulShutdownTimeout = 10 * time.Second

	// taskWaitRetryInterval is how long to wait before retrying task.Wait when containerd is
	// temporarily unavailable.
	taskWaitRetryInterval = time.Second

	// oomScoreAdj matches what extension services get. Containers must be killed before apid and
	// trustd, which sit at -998, so that the API stays reachable on a node under memory pressure.
	oomScoreAdj = -600
)

// containerdRunner runs containers against the CRI containerd instance.
type containerdRunner struct {
	client  *containerdapi.Client
	logging runtime.LoggingManager
}

func newContainerdRunner(logging runtime.LoggingManager) (TaskRunner, error) {
	client, err := containerdapi.New(constants.CRIContainerdAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to containerd: %w", err)
	}

	return &containerdRunner{
		client:  client,
		logging: logging,
	}, nil
}

// withNamespace scopes a context to the dedicated namespace.
func withNamespace(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, constants.TalosContainersNamespace)
}

// List implements TaskRunner interface.
func (r *containerdRunner) List(ctx context.Context) ([]string, error) {
	list, err := r.client.Containers(withNamespace(ctx))
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(list))

	for _, container := range list {
		ids = append(ids, container.ID())
	}

	return ids, nil
}

// Remove implements TaskRunner interface.
//
// Absence is not an error: this runs both as part of the orphan sweep and on teardown, and in both
// cases the goal is "gone", not "was there".
func (r *containerdRunner) Remove(ctx context.Context, id string) error {
	ctx = withNamespace(ctx)

	container, err := r.client.LoadContainer(ctx, id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return r.removeSnapshot(ctx, id)
		}

		return fmt.Errorf("failed to load container: %w", err)
	}

	// Kill any task first: a container with a live task cannot be deleted.
	if task, taskErr := container.Task(ctx, nil); taskErr == nil {
		if _, delErr := task.Delete(ctx, containerdapi.WithProcessKill); delErr != nil && !errdefs.IsNotFound(delErr) {
			return fmt.Errorf("failed to delete task: %w", delErr)
		}
	}

	if err := container.Delete(ctx, containerdapi.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("failed to delete container: %w", err)
	}

	return r.removeSnapshot(ctx, id)
}

// removeSnapshot clears a snapshot left behind without its container.
func (r *containerdRunner) removeSnapshot(ctx context.Context, id string) error {
	// Talos sets no gc.root labels and takes no leases here, so a snapshot with no container has
	// nothing to reclaim it and would sit in /var/lib/containerd forever.
	if err := r.client.SnapshotService("").Remove(ctx, id); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("failed to remove snapshot: %w", err)
	}

	return nil
}

// Run implements TaskRunner interface.
//
//nolint:gocyclo,cyclop
func (r *containerdRunner) Run(
	ctx context.Context,
	id string,
	spec containersres.ContainerInstanceSpecSpec,
	started func(pid uint32),
) (int32, error) {
	// Teardown must survive cancellation of ctx: ctx being canceled is precisely how a stop is
	// requested, so using it for the kill and delete calls would make teardown a no-op exactly when
	// it matters. Every cleanup path below uses detachedCtx instead.
	detachedCtx := withNamespace(context.WithoutCancel(ctx))
	ctx = withNamespace(ctx)

	cgroupPath := filepath.Join(constants.CgroupTalosContainersRoot, spec.ContainerID)

	cg, err := cgroup.CreateCgroupWithResources(cgroupPath, cgroupResources(spec.Resources))
	if err != nil {
		return 0, fmt.Errorf("failed to create cgroup: %w", err)
	}

	defer func() {
		if err := cg.Delete(); err != nil {
			// A leaked cgroup is recovered by the recursive sweep of the taloscontainers root when
			// machined exits, so this is not fatal.
			_ = err
		}
	}()

	image, err := r.client.GetImage(ctx, spec.Image)
	if err != nil {
		return 0, fmt.Errorf("failed to get image %q: %w", spec.Image, err)
	}

	// Delete-then-create, never adopt: an existing container of this ID is stale state from a
	// previous life, and adopting it would attach to a task whose wait stream is gone.
	if err := r.Remove(detachedCtx, id); err != nil {
		return 0, fmt.Errorf("failed to clear stale container: %w", err)
	}

	container, err := r.client.NewContainer(ctx, id,
		containerdapi.WithImage(image),
		containerdapi.WithNewSnapshot(id, image),
		containerdapi.WithNewSpec(r.ociSpecOpts(spec, image, cgroupPath)...),
	)
	if err != nil {
		return 0, fmt.Errorf("failed to create container: %w", err)
	}

	defer func() {
		if err := container.Delete(detachedCtx, containerdapi.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
			_ = err
		}
	}()

	// Keyed by container, not by instance: successive generations append to one buffer, so restart
	// history reads as a single continuous log.
	logWriter, err := r.logging.ServiceLog(constants.TalosContainersLogPrefix + spec.ContainerID).Writer()
	if err != nil {
		return 0, fmt.Errorf("failed to open log writer: %w", err)
	}

	defer logWriter.Close() //nolint:errcheck

	task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStreams(nil, logWriter, logWriter)))
	if err != nil {
		return 0, fmt.Errorf("failed to create task: %w", err)
	}

	defer func() {
		if _, err := task.Delete(detachedCtx, containerdapi.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
			_ = err
		}
	}()

	// Subscribe to the exit status before starting, so an immediate exit is not missed.
	statusCh, err := task.Wait(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to wait for task: %w", err)
	}

	if err := task.Start(ctx); err != nil {
		return 0, fmt.Errorf("failed to start task: %w", err)
	}

	started(task.Pid())

	exitCode, err := r.waitForExit(ctx, detachedCtx, task, statusCh)
	if err != nil {
		return 0, err
	}

	return exitCode, nil
}

// waitForExit blocks until the task exits or a stop is requested.
//
//nolint:gocyclo
func (r *containerdRunner) waitForExit(
	ctx, detachedCtx context.Context,
	task containerdapi.Task,
	statusCh <-chan containerdapi.ExitStatus,
) (int32, error) {
	for {
		select {
		case status, ok := <-statusCh:
			if !ok {
				return 0, errors.New("task wait stream closed unexpectedly")
			}

			if err := status.Error(); err != nil {
				// containerd being briefly unavailable must not be mistaken for the task dying: the
				// task keeps running across a containerd restart, and treating this as an exit would
				// take down every container on the node whenever containerd restarts.
				if isUnavailable(err) {
					select {
					case <-ctx.Done():
						return r.stop(detachedCtx, task)
					case <-time.After(taskWaitRetryInterval):
					}

					var waitErr error

					if statusCh, waitErr = task.Wait(ctx); waitErr != nil {
						return 0, fmt.Errorf("failed to re-establish task wait: %w", waitErr)
					}

					continue
				}

				return 0, fmt.Errorf("task wait failed: %w", err)
			}

			return int32(status.ExitCode()), nil //nolint:gosec
		case <-ctx.Done():
			return r.stop(detachedCtx, task)
		}
	}
}

// stop runs the graceful shutdown sequence.
func (r *containerdRunner) stop(detachedCtx context.Context, task containerdapi.Task) (int32, error) {
	statusCh, err := task.Wait(detachedCtx)
	if err != nil {
		return 0, fmt.Errorf("failed to wait for task during stop: %w", err)
	}

	// WithKillAll signals every process in the container, not just PID 1: a container whose init
	// does not forward signals would otherwise leave children behind.
	if err := task.Kill(detachedCtx, syscall.SIGTERM, containerdapi.WithKillAll); err != nil && !errdefs.IsNotFound(err) {
		return 0, fmt.Errorf("failed to send SIGTERM: %w", err)
	}

	select {
	case status := <-statusCh:
		return int32(status.ExitCode()), nil //nolint:gosec
	case <-time.After(gracefulShutdownTimeout):
	}

	if err := task.Kill(detachedCtx, syscall.SIGKILL, containerdapi.WithKillAll); err != nil && !errdefs.IsNotFound(err) {
		return 0, fmt.Errorf("failed to send SIGKILL: %w", err)
	}

	status := <-statusCh

	return int32(status.ExitCode()), nil //nolint:gosec
}

// Close implements TaskRunner interface.
func (r *containerdRunner) Close() error {
	return r.client.Close()
}

// ociSpecOpts builds the OCI spec for a container.
//
//nolint:gocyclo,cyclop
func (r *containerdRunner) ociSpecOpts(spec containersres.ContainerInstanceSpecSpec, image containerdapi.Image, cgroupPath string) []oci.SpecOpts {
	opts := []oci.SpecOpts{
		oci.WithImageConfig(image),
		oci.WithNoNewPrivileges,
		oci.WithCgroup(cgroup.Path(cgroupPath)),
		withOOMScoreAdj(oomScoreAdj),
	}

	if len(spec.Entrypoint) > 0 || len(spec.Args) > 0 {
		args := append(append([]string{}, spec.Entrypoint...), spec.Args...)
		opts = append(opts, oci.WithProcessArgs(args...))
	}

	if spec.WorkingDir != "" {
		opts = append(opts, oci.WithProcessCwd(spec.WorkingDir))
	}

	if spec.User != "" {
		uid, gid := parseUser(spec.User)
		opts = append(opts, oci.WithUIDGID(uid, gid))
	}

	if len(spec.Environment) > 0 {
		opts = append(opts, oci.WithEnv(spec.Environment))
	}

	if spec.Network.HostNetwork {
		opts = append(opts, oci.WithHostNamespace(specs.NetworkNamespace), oci.WithHostHostsFile, oci.WithHostResolvconf)
	}

	if mounts := ociMounts(spec.Mounts); len(mounts) > 0 {
		opts = append(opts, oci.WithMounts(mounts))
	}

	if spec.Security.Privileged {
		// Extension-service-level permissions: all grantable capabilities and all devices.
		opts = append(opts,
			oci.WithCapabilities(capability.AllGrantableCapabilities()),
			oci.WithAllDevicesAllowed,
		)
	} else {
		// Restricted default: no capabilities, read-only rootfs and sysfs.
		opts = append(opts,
			oci.WithCapabilities(nil),
			oci.WithRootFSReadonly(),
		)
	}

	if len(spec.Security.CapabilitiesDrop) > 0 {
		opts = append(opts, oci.WithDroppedCapabilities(prefixCapabilities(spec.Security.CapabilitiesDrop)))
	}

	if len(spec.Security.CapabilitiesAdd) > 0 {
		opts = append(opts, oci.WithAddedCapabilities(prefixCapabilities(spec.Security.CapabilitiesAdd)))
	}

	// SELinux and seccomp go last: the seccomp profile is derived from the capabilities resolved by
	// everything above, so applying it earlier would compute it against the wrong set.
	opts = append(opts, oci.WithSelinuxLabel(constants.SelinuxLabelUnconfinedSysContainer))

	return opts
}

// parseUser splits a validated `uid` or `uid:gid` string into numeric IDs, defaulting gid to 0.
//
// Config validation only accepts a purely numeric uid[:gid] — there are no user namespaces to
// resolve a username against — so this bypasses oci.WithUser's implicit username lookup, which
// would otherwise mount and read the image's /etc/passwd and, worse, statically pulls containerd's
// rootfs-template resolution machinery (and its transitive text/template dependency) into machined.
func parseUser(userstr string) (uid, gid uint32) {
	uidStr, gidStr, hasGid := strings.Cut(userstr, ":")

	if v, err := strconv.ParseUint(uidStr, 10, 32); err == nil {
		uid = uint32(v)
	}

	if hasGid {
		if v, err := strconv.ParseUint(gidStr, 10, 32); err == nil {
			gid = uint32(v)
		}
	}

	return uid, gid
}

// ociMounts converts resolved mounts into OCI mounts.
func ociMounts(mounts []containersres.ResolvedMountSpec) []specs.Mount {
	if len(mounts) == 0 {
		return nil
	}

	out := make([]specs.Mount, 0, len(mounts))

	for _, mount := range mounts {
		switch mount.Kind {
		case containersres.MountKindTmpfs:
			options := append([]string{"nosuid", "nodev"}, mount.Options...)

			if mount.Size > 0 {
				options = append(options, fmt.Sprintf("size=%d", mount.Size))
			}

			out = append(out, specs.Mount{
				Type:        "tmpfs",
				Source:      "tmpfs",
				Destination: mount.Destination,
				Options:     options,
			})
		case containersres.MountKindUserVolume, containersres.MountKindHostPath:
			out = append(out, specs.Mount{
				Type:        "bind",
				Source:      mount.Source,
				Destination: mount.Destination,
				Options:     append([]string{"rbind"}, mount.Options...),
			})
		}
	}

	return out
}

// prefixCapabilities restores the CAP_ prefix the configuration deliberately omits.
func prefixCapabilities(capabilities []string) []string {
	out := make([]string, 0, len(capabilities))

	for _, c := range capabilities {
		if c == "ALL" {
			return []string{"ALL"}
		}

		out = append(out, "CAP_"+c)
	}

	return out
}

// withOOMScoreAdj sets the OOM score adjustment on the process.
func withOOMScoreAdj(score int) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Process == nil {
			s.Process = &specs.Process{}
		}

		s.Process.OOMScoreAdj = &score

		return nil
	}
}

// cgroupResources translates the resolved limits into cgroup v2 resources.
func cgroupResources(res containersres.ContainerResourcesSpec) *cgroup2.Resources {
	out := &cgroup2.Resources{}

	if res.MemoryLimit > 0 {
		out.Memory = &cgroup2.Memory{
			Max: new(int64(res.MemoryLimit)), //nolint:gosec
		}
	}

	if res.CPULimit > 0 {
		// cpu.max is a quota over a period, unlike the weight used elsewhere in Talos: this is a
		// ceiling, not a share.
		const period = 100000

		quota := int64(res.CPULimit) * period / 1000 //nolint:gosec

		out.CPU = &cgroup2.CPU{
			Max: cgroup2.NewCPUMax(&quota, new(uint64(period))),
		}
	}

	return out
}

func isUnavailable(err error) bool {
	if errdefs.IsUnavailable(err) {
		return true
	}

	if s, ok := status.FromError(err); ok {
		return s.Code() == codes.Unavailable
	}

	return false
}
