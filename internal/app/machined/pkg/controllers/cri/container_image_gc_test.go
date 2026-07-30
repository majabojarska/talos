// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package cri_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/siderolabs/gen/xslices"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	crictrl "github.com/siderolabs/talos/internal/app/machined/pkg/controllers/cri"
	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/ctest"
	"github.com/siderolabs/talos/pkg/machinery/resources/containers"
	"github.com/siderolabs/talos/pkg/machinery/resources/v1alpha1"
)

// runContainerImageGC drives the container image GC controller past the grace period and returns the
// surviving image names.
//
// declared lists the images that ContainerSpec documents reference; stored is the namespace content.
func runContainerImageGC(t *testing.T, declared []string, stored func(now time.Time) []images.Image) []string {
	var surviving []string

	synctest.Test(t, func(t *testing.T) {
		mock := &mockImageService{}

		// Built inside the synctest function so the controller's ticker uses controlled time.
		controller := crictrl.NewContainerImageGCController()
		controller.ImageServiceProvider = func() (crictrl.ImageServiceProvider, error) {
			return mock, nil
		}

		suite := &ctest.DefaultSuite{
			Timeout: 2 * time.Hour,
			AfterSetup: func(suite *ctest.DefaultSuite) {
				suite.Require().NoError(suite.Runtime().RegisterController(controller))
			},
		}

		suite.SetT(t)
		suite.SetupTest()

		defer suite.TearDownTest()

		now := time.Now()

		mock.images = stored(now)

		// The controller only ticks once the CRI containerd instance reports healthy.
		criService := v1alpha1.NewService("cri")
		criService.TypedSpec().Running = true
		criService.TypedSpec().Healthy = true
		require.NoError(t, suite.State().Create(suite.Ctx(), criService))

		for i, ref := range declared {
			spec := containers.NewContainerSpec(containers.NamespaceName, string(rune('a'+i)))
			spec.TypedSpec().Image = ref
			require.NoError(t, suite.State().Create(suite.Ctx(), spec))
		}

		// Nothing is collected until an image has been seen unreferenced for the grace period, so
		// advance past it and then let a cleanup tick land.
		time.Sleep(crictrl.ImageGCGracePeriod + 5*time.Minute)
		synctest.Wait()
		time.Sleep(crictrl.ImageCleanupInterval)
		synctest.Wait()

		list, err := mock.List(suite.Ctx())
		require.NoError(t, err)

		surviving = xslices.Map(list, func(i images.Image) string { return i.Name })
	})

	return surviving
}

func TestContainerImageGCKeepsDeclaredImages(t *testing.T) {
	const declared = "docker.io/library/nginx:1.27"

	declaredDigest := digest.FromString("nginx")
	canonical := "docker.io/library/nginx@" + declaredDigest.String()

	surviving := runContainerImageGC(t,
		[]string{declared},
		func(now time.Time) []images.Image {
			old := now.Add(-2 * crictrl.ImageGCGracePeriod)

			return []images.Image{
				{
					Name:      declared,
					Target:    v1.Descriptor{Digest: declaredDigest},
					CreatedAt: old,
				},
				{
					// The canonical alias of the same image. Matching is digest-keyed, so it must
					// survive too: a pull records several names for one image and they have to live
					// or die together.
					Name:      canonical,
					Target:    v1.Descriptor{Digest: declaredDigest},
					CreatedAt: old,
				},
				{
					Name:      "docker.io/library/stray:1.0",
					Target:    v1.Descriptor{Digest: digest.FromString("stray")},
					CreatedAt: old,
				},
			}
		},
	)

	assert.ElementsMatch(t, []string{declared, canonical}, surviving)
}

// TestContainerImageGCEmptyExpectedSetCollectsEverything covers the branch the existing suite never
// reached. With no containers declared the expected set is legitimately empty and the whole namespace
// should go; for the kubelet image the same behavior would be catastrophic, which is why it is
// opt-in per instance rather than the default.
func TestContainerImageGCEmptyExpectedSetCollectsEverything(t *testing.T) {
	surviving := runContainerImageGC(t,
		nil,
		func(now time.Time) []images.Image {
			old := now.Add(-2 * crictrl.ImageGCGracePeriod)

			return []images.Image{
				{
					Name:      "docker.io/library/nginx:1.27",
					Target:    v1.Descriptor{Digest: digest.FromString("nginx")},
					CreatedAt: old,
				},
				{
					Name:      "docker.io/library/redis:7",
					Target:    v1.Descriptor{Digest: digest.FromString("redis")},
					CreatedAt: old,
				},
			}
		},
	)

	assert.Empty(t, surviving)
}

func TestContainerImageGCRespectsGracePeriod(t *testing.T) {
	surviving := runContainerImageGC(t,
		nil,
		func(now time.Time) []images.Image {
			return []images.Image{
				{
					Name:   "docker.io/library/fresh:1.0",
					Target: v1.Descriptor{Digest: digest.FromString("fresh")},
					// Dated into the future relative to the ticks, so it never ages past the floor.
					CreatedAt: now.Add(crictrl.ImageGCGracePeriod),
				},
			}
		},
	)

	assert.Equal(t, []string{"docker.io/library/fresh:1.0"}, surviving,
		"an image younger than the grace period must not be collected")
}
