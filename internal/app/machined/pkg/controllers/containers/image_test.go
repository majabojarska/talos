// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"

	containersctrl "github.com/siderolabs/talos/internal/app/machined/pkg/controllers/containers"
	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/ctest"
	"github.com/siderolabs/talos/pkg/machinery/resources/containers"
	"github.com/siderolabs/talos/pkg/machinery/resources/v1alpha1"
)

// fakePuller stands in for containerd. Each ref maps to a result, and a ref with a nil result
// blocks until released, which is how the pulling phase gets observed.
type fakePuller struct {
	mu       sync.Mutex
	results  map[string]fakePullResult
	blocked  map[string]chan struct{}
	attempts map[string]int
	closed   bool
}

type fakePullResult struct {
	digest string
	err    error
}

func newFakePuller() *fakePuller {
	return &fakePuller{
		results:  map[string]fakePullResult{},
		blocked:  map[string]chan struct{}{},
		attempts: map[string]int{},
	}
}

func (p *fakePuller) setResult(ref, digest string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.results[ref] = fakePullResult{digest: digest, err: err}
}

// block makes pulls of ref hang until release is called.
func (p *fakePuller) block(ref string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.blocked[ref] = make(chan struct{})
}

func (p *fakePuller) release(ref string) {
	p.mu.Lock()
	ch := p.blocked[ref]
	delete(p.blocked, ref)
	p.mu.Unlock()

	if ch != nil {
		close(ch)
	}
}

func (p *fakePuller) attemptCount(ref string) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.attempts[ref]
}

func (p *fakePuller) Pull(ctx context.Context, ref string, report func(string)) (string, error) {
	p.mu.Lock()
	p.attempts[ref]++
	blocked := p.blocked[ref]
	result := p.results[ref]
	p.mu.Unlock()

	if blocked != nil {
		report("downloading 1.0 MiB / 2.0 MiB")

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-blocked:
		}
	}

	return result.digest, result.err
}

func (p *fakePuller) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed = true

	return nil
}

type ImageSuite struct {
	ctest.DefaultSuite

	puller *fakePuller
}

func TestImageSuite(t *testing.T) {
	t.Parallel()

	puller := newFakePuller()

	suite.Run(t, &ImageSuite{
		puller: puller,
		DefaultSuite: ctest.DefaultSuite{
			Timeout: 15 * time.Second,
			AfterSetup: func(suite *ctest.DefaultSuite) {
				suite.Require().NoError(suite.Runtime().RegisterController(&containersctrl.ImageController{
					PullerProvider: func() (containersctrl.Puller, error) { return puller, nil },
				}))
			},
		},
	})
}

func (suite *ImageSuite) criUp() {
	service := v1alpha1.NewService("cri")
	service.TypedSpec().Running = true
	service.TypedSpec().Healthy = true

	suite.Require().NoError(suite.State().Create(suite.Ctx(), service))
}

func (suite *ImageSuite) createSpecWithImage(name, image string) {
	spec := containers.NewContainerSpec(containers.NamespaceName, name)
	spec.TypedSpec().Image = image

	suite.Require().NoError(suite.State().Create(suite.Ctx(), spec))
}

func (suite *ImageSuite) TestPendingUntilCRIIsUp() {
	suite.createSpecWithImage("nginx", "docker.io/library/nginx:latest")

	// Without the CRI service there is nowhere to pull to, and the status says so rather than
	// silently doing nothing.
	ctest.AssertResource(suite, "nginx", func(status *containers.ContainerImageStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerImagePhasePending, status.TypedSpec().Phase)
	})

	asrt := suite.Assert()
	asrt.Zero(suite.puller.attemptCount("docker.io/library/nginx:latest"))
}

func (suite *ImageSuite) TestPullsAndReportsDigest() {
	const ref = "docker.io/library/nginx:1.27"

	suite.puller.setResult(ref, "sha256:abc123", nil)
	suite.criUp()
	suite.createSpecWithImage("nginx", ref)

	ctest.AssertResource(suite, "nginx", func(status *containers.ContainerImageStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerImagePhaseReady, status.TypedSpec().Phase)
		asrt.Equal("sha256:abc123", status.TypedSpec().Digest)
		asrt.Empty(status.TypedSpec().Error)
	})
}

func (suite *ImageSuite) TestReportsPullingWithProgress() {
	const ref = "docker.io/library/slow:1.0"

	suite.puller.block(ref)
	suite.puller.setResult(ref, "sha256:slow", nil)
	suite.criUp()
	suite.createSpecWithImage("slow", ref)

	ctest.AssertResource(suite, "slow", func(status *containers.ContainerImageStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerImagePhasePulling, status.TypedSpec().Phase)
		asrt.Contains(status.TypedSpec().Progress, "downloading")
	})

	suite.puller.release(ref)

	ctest.AssertResource(suite, "slow", func(status *containers.ContainerImageStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerImagePhaseReady, status.TypedSpec().Phase)
		asrt.Equal("sha256:slow", status.TypedSpec().Digest)
	})
}

func (suite *ImageSuite) TestReportsFailure() {
	const ref = "docker.io/library/broken:1.0"

	suite.puller.setResult(ref, "", errors.New("signature verification denied"))
	suite.criUp()
	suite.createSpecWithImage("broken", ref)

	ctest.AssertResource(suite, "broken", func(status *containers.ContainerImageStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerImagePhaseFailed, status.TypedSpec().Phase)
		asrt.Contains(status.TypedSpec().Error, "signature verification denied")
		// A failed pull leaves no digest, so the instance gate stays shut.
		asrt.Empty(status.TypedSpec().Digest)
	})
}

func (suite *ImageSuite) TestDoesNotRepullUnchangedReference() {
	const ref = "docker.io/library/stable:1.0"

	suite.puller.setResult(ref, "sha256:stable", nil)
	suite.criUp()
	suite.createSpecWithImage("stable", ref)

	ctest.AssertResource(suite, "stable", func(status *containers.ContainerImageStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerImagePhaseReady, status.TypedSpec().Phase)
	})

	// Touch the spec without changing the reference: several reconciles follow, but the pull must
	// not be repeated. Otherwise every unrelated event would re-fetch the image.
	for range 3 {
		spec, err := suite.State().Get(suite.Ctx(), containers.NewContainerSpec(containers.NamespaceName, "stable").Metadata())
		suite.Require().NoError(err)

		updated := spec.(*containers.ContainerSpec) //nolint:forcetypeassert,errcheck
		updated.TypedSpec().WorkingDir = "/srv"

		suite.Require().NoError(suite.State().Update(suite.Ctx(), updated))
	}

	ctest.AssertResource(suite, "stable", func(status *containers.ContainerImageStatus, asrt *assert.Assertions) {
		asrt.Equal(containers.ContainerImagePhaseReady, status.TypedSpec().Phase)
	})

	suite.Assert().Equal(1, suite.puller.attemptCount(ref))
}

func (suite *ImageSuite) TestRepullsWhenReferenceChanges() {
	const (
		oldRef = "docker.io/library/moving:1.0"
		newRef = "docker.io/library/moving:2.0"
	)

	suite.puller.setResult(oldRef, "sha256:v1", nil)
	suite.puller.setResult(newRef, "sha256:v2", nil)
	suite.criUp()
	suite.createSpecWithImage("moving", oldRef)

	ctest.AssertResource(suite, "moving", func(status *containers.ContainerImageStatus, asrt *assert.Assertions) {
		asrt.Equal("sha256:v1", status.TypedSpec().Digest)
	})

	spec, err := suite.State().Get(suite.Ctx(), containers.NewContainerSpec(containers.NamespaceName, "moving").Metadata())
	suite.Require().NoError(err)

	updated := spec.(*containers.ContainerSpec) //nolint:forcetypeassert,errcheck
	updated.TypedSpec().Image = newRef

	suite.Require().NoError(suite.State().Update(suite.Ctx(), updated))

	ctest.AssertResource(suite, "moving", func(status *containers.ContainerImageStatus, asrt *assert.Assertions) {
		asrt.Equal("sha256:v2", status.TypedSpec().Digest)
	})
}

func (suite *ImageSuite) TestRemovesStatusWhenSpecGoesAway() {
	const ref = "docker.io/library/gone:1.0"

	suite.puller.setResult(ref, "sha256:gone", nil)
	suite.criUp()
	suite.createSpecWithImage("gone", ref)

	ctest.AssertResource(suite, "gone", func(*containers.ContainerImageStatus, *assert.Assertions) {})

	suite.Require().NoError(suite.State().Destroy(suite.Ctx(),
		containers.NewContainerSpec(containers.NamespaceName, "gone").Metadata()))

	ctest.AssertNoResource[*containers.ContainerImageStatus](suite, "gone")
}
