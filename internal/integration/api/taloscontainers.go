// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

//go:build integration_api

package api

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"google.golang.org/grpc/codes"

	"github.com/siderolabs/talos/internal/integration/base"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	"github.com/siderolabs/talos/pkg/machinery/client"
	containercfg "github.com/siderolabs/talos/pkg/machinery/config/types/container"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/containers"
)

// TalosContainersSuite verifies running containers via ContainerConfig, without Kubernetes.
type TalosContainersSuite struct {
	base.APISuite

	ctx       context.Context //nolint:containedctx
	ctxCancel context.CancelFunc
}

// SuiteName ...
func (suite *TalosContainersSuite) SuiteName() string {
	return "api.TalosContainersSuite"
}

// SetupTest ...
func (suite *TalosContainersSuite) SetupTest() {
	// Pulling an image can take a while on a cold node.
	suite.ctx, suite.ctxCancel = context.WithTimeout(context.Background(), 5*time.Minute)
}

// TearDownTest ...
func (suite *TalosContainersSuite) TearDownTest() {
	if suite.ctxCancel != nil {
		suite.ctxCancel()
	}
}

// TestContainerLifecycle applies a ContainerConfig, waits for the container to run, then removes it.
func (suite *TalosContainersSuite) TestContainerLifecycle() {
	if testing.Short() {
		suite.T().Skip("skipping in short mode")
	}

	node := suite.RandomDiscoveredNodeInternalIP()
	nodeCtx := client.WithNode(suite.ctx, node)

	suite.T().Logf("testing on node %q", node)

	const containerName = "integration-nginx"

	cfg := containercfg.NewContainerConfigV1Alpha1()
	cfg.MetaName = containerName
	cfg.ContainerImage = "registry.k8s.io/pause:3.10"

	suite.PatchMachineConfig(nodeCtx, cfg)
	defer suite.RemoveMachineConfigDocuments(nodeCtx, containercfg.ContainerConfigKind)

	// The spec is emitted as soon as the document is validated.
	suite.waitForContainerState(nodeCtx, containerName, containers.ContainerStateRunning)

	// The status must carry the resolved digest and a live PID.
	status, err := safe.StateGetByID[*containers.ContainerStatus](nodeCtx, suite.Client.COSI, containerName)
	suite.Require().NoError(err)

	suite.Assert().Equal(containers.ContainerHealthHealthy, status.TypedSpec().Health)
	suite.Assert().NotZero(status.TypedSpec().PID)
	suite.Assert().Contains(status.TypedSpec().Image, "@sha256:", "the running image should be digest-resolved")

	// An instance status exists for the current generation, which is where per-execution detail lives.
	instanceID := containers.InstanceID(containerName, status.TypedSpec().RestartCount)

	instance, err := safe.StateGetByID[*containers.ContainerInstanceStatus](nodeCtx, suite.Client.COSI, instanceID)
	suite.Require().NoError(err)
	suite.Assert().Equal(containers.ContainerInstancePhaseRunning, instance.TypedSpec().Phase)

	// talosctl containers must see it in the dedicated namespace, through the containerd driver.
	resp, err := suite.Client.Containers(nodeCtx, constants.TalosContainersNamespace, common.ContainerDriver_CONTAINERD)
	suite.Require().NoError(err)

	found := false

	for _, message := range resp.GetMessages() {
		for _, container := range message.GetContainers() {
			if container.GetId() == instanceID {
				found = true
			}
		}
	}

	suite.Assert().True(found, "container %q should be listed in the %q namespace", instanceID, constants.TalosContainersNamespace)

	// Removing the document must remove the container and all of its resources.
	suite.RemoveMachineConfigDocuments(nodeCtx, containercfg.ContainerConfigKind)

	suite.Require().NoError(suite.waitForNoResource(nodeCtx, func() error {
		_, err := safe.StateGetByID[*containers.ContainerStatus](nodeCtx, suite.Client.COSI, containerName)

		return err
	}))
}

// TestContainerRestarts verifies that a container which exits is restarted with a new generation.
func (suite *TalosContainersSuite) TestContainerRestarts() {
	if testing.Short() {
		suite.T().Skip("skipping in short mode")
	}

	node := suite.RandomDiscoveredNodeInternalIP()
	nodeCtx := client.WithNode(suite.ctx, node)

	const containerName = "integration-exiter"

	cfg := containercfg.NewContainerConfigV1Alpha1()
	cfg.MetaName = containerName
	cfg.ContainerImage = "registry.k8s.io/pause:3.10"
	// Override the entrypoint so the container exits immediately, which forces the restart path.
	cfg.ContainerEntrypoint = []string{"/pause", "--help"}

	suite.PatchMachineConfig(nodeCtx, cfg)
	defer suite.RemoveMachineConfigDocuments(nodeCtx, containercfg.ContainerConfigKind)

	// Restarts are unconditional and 5s apart, so a couple of generations should appear quickly.
	suite.Require().NoError(retryUntil(nodeCtx, 2*time.Minute, func() bool {
		status, err := safe.StateGetByID[*containers.ContainerStatus](nodeCtx, suite.Client.COSI, containerName)
		if err != nil {
			return false
		}

		return status.TypedSpec().RestartCount >= 2
	}))

	// Retained instances are what make the crash history inspectable.
	instances, err := safe.StateListAll[*containers.ContainerInstanceStatus](nodeCtx, suite.Client.COSI)
	suite.Require().NoError(err)

	count := 0

	for instance := range instances.All() {
		if instance.TypedSpec().ContainerID == containerName {
			count++
		}
	}

	suite.Assert().Greater(count, 1, "terminated instances should be retained for inspection")
}

// TestContainerLogsSurviveExit verifies that a stopped container's output is still retrievable.
//
// Container logs live only in the in-memory circular buffer, so this is the whole story for
// diagnosing a container that failed: if the output went away with the process, a crash loop would
// be un-debuggable.
func (suite *TalosContainersSuite) TestContainerLogsSurviveExit() {
	if testing.Short() {
		suite.T().Skip("skipping in short mode")
	}

	node := suite.RandomDiscoveredNodeInternalIP()
	nodeCtx := client.WithNode(suite.ctx, node)

	const (
		containerName = "integration-logger"
		marker        = "hello-from-the-container"
	)

	cfg := containercfg.NewContainerConfigV1Alpha1()
	cfg.MetaName = containerName
	cfg.ContainerImage = "docker.io/library/alpine:3.23"
	cfg.ContainerEntrypoint = []string{"/bin/sh", "-c", "echo " + marker + "; exit 1"}

	suite.PatchMachineConfig(nodeCtx, cfg)
	defer suite.RemoveMachineConfigDocuments(nodeCtx, containercfg.ContainerConfigKind)

	// Wait until the container has run and exited at least once.
	suite.Require().NoError(retryUntil(nodeCtx, 4*time.Minute, func() bool {
		status, err := safe.StateGetByID[*containers.ContainerStatus](nodeCtx, suite.Client.COSI, containerName)
		if err != nil {
			return false
		}

		return status.TypedSpec().RestartCount >= 1
	}))

	// The container is not running right now, and the request goes through the dedicated namespace
	// rather than -k, which is what the new --namespace flag selects.
	logs, err := suite.readLogs(nodeCtx, constants.TalosContainersNamespace, containerName)
	suite.Require().NoError(err)

	suite.Assert().Contains(logs, marker)

	// Every generation appends to the same buffer, so the restarted container's output is there too.
	suite.Assert().GreaterOrEqual(strings.Count(logs, marker), 2,
		"successive generations should append to one buffer")
}

// readLogs drains the Logs stream for one container into a string.
func (suite *TalosContainersSuite) readLogs(ctx context.Context, namespace, id string) (string, error) {
	stream, err := suite.Client.Logs(ctx, namespace, common.ContainerDriver_CONTAINERD, id, false, -1)
	if err != nil {
		return "", err
	}

	var out strings.Builder

	for {
		data, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || client.StatusCode(err) == codes.Canceled {
				return out.String(), nil
			}

			return out.String(), err
		}

		out.Write(data.GetBytes())
	}
}

// waitForContainerState blocks until the container reaches the given state.
func (suite *TalosContainersSuite) waitForContainerState(ctx context.Context, name string, want containers.ContainerState) {
	suite.Require().NoError(retryUntil(ctx, 4*time.Minute, func() bool {
		status, err := safe.StateGetByID[*containers.ContainerStatus](ctx, suite.Client.COSI, name)
		if err != nil {
			return false
		}

		if status.TypedSpec().State == want {
			return true
		}

		suite.T().Logf("container %q is %s (%s)", name, status.TypedSpec().State, status.TypedSpec().Error)

		return false
	}))
}

// waitForNoResource blocks until get reports the resource is gone.
func (suite *TalosContainersSuite) waitForNoResource(ctx context.Context, get func() error) error {
	return retryUntil(ctx, time.Minute, func() bool {
		return state.IsNotFoundError(get())
	})
}

// retryUntil polls cond until it holds or the timeout elapses.
func retryUntil(ctx context.Context, timeout time.Duration, cond func() bool) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		if cond() {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func init() {
	allSuites = append(allSuites, new(TalosContainersSuite))
}
