// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package logging_test

import (
	"io"
	"log"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos/internal/app/machined/pkg/runtime/logging"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

// containerLogID is what a container declared via ContainerConfig logs under.
const containerLogID = constants.TalosContainersLogPrefix + "nginx"

func newManager() *logging.CircularBufferLoggingManager {
	return logging.NewCircularBufferLoggingManager(log.New(io.Discard, "", 0))
}

func readAll(t *testing.T, manager *logging.CircularBufferLoggingManager, id string) string {
	t.Helper()

	r, err := manager.ServiceLog(id).Reader()
	require.NoError(t, err)

	defer r.Close() //nolint:errcheck

	contents, err := io.ReadAll(r)
	require.NoError(t, err)

	return string(contents)
}

// TestBufferOutlivesWriter covers the property `talosctl logs --namespace taloscontainers` depends
// on: nothing writes container logs to disk, so if closing the writer dropped the buffer, the output
// of a container that has exited would be unreachable.
func TestBufferOutlivesWriter(t *testing.T) {
	t.Parallel()

	manager := newManager()

	w, err := manager.ServiceLog(containerLogID).Writer()
	require.NoError(t, err)

	_, err = w.Write([]byte("listening on :80\n"))
	require.NoError(t, err)

	// The container exits and the runner closes its side of the pipe.
	require.NoError(t, w.Close())

	assert.Equal(t, "listening on :80\n", readAll(t, manager, containerLogID))
}

// TestRestartAppendsToSameBuffer covers the restart path: the log identifier is keyed by container
// rather than by instance, so a crash loop reads as one continuous log instead of losing everything
// each time the container is replaced.
func TestRestartAppendsToSameBuffer(t *testing.T) {
	t.Parallel()

	manager := newManager()

	first, err := manager.ServiceLog(containerLogID).Writer()
	require.NoError(t, err)

	_, err = first.Write([]byte("generation 0\n"))
	require.NoError(t, err)
	require.NoError(t, first.Close())

	second, err := manager.ServiceLog(containerLogID).Writer()
	require.NoError(t, err)

	_, err = second.Write([]byte("generation 1\n"))
	require.NoError(t, err)
	require.NoError(t, second.Close())

	assert.Equal(t, "generation 0\ngeneration 1\n", readAll(t, manager, containerLogID))
}

// TestRegisteredLogsListsStoppedContainers covers shell completion, which lists the identifiers the
// manager knows about rather than the containers that are currently running.
func TestRegisteredLogsListsStoppedContainers(t *testing.T) {
	t.Parallel()

	manager := newManager()

	w, err := manager.ServiceLog(containerLogID).Writer()
	require.NoError(t, err)
	require.NoError(t, w.Close())

	assert.True(t, slices.Contains(manager.RegisteredLogs(), containerLogID))
}

// TestReaderRejectsUnknownLog documents the error a container which never started produces: no
// buffer is created until something writes to it, and only Writer creates one.
func TestReaderRejectsUnknownLog(t *testing.T) {
	t.Parallel()

	manager := newManager()

	_, err := manager.ServiceLog(constants.TalosContainersLogPrefix + "never-started").Reader()
	assert.ErrorContains(t, err, "was not registered")
}
