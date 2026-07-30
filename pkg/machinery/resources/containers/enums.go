// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package containers

// ContainerState describes where a container is in its lifecycle.
//
// This is the fine-grained view, derived from the newest instance. It is deliberately more detailed
// than ContainerHealth so that "waiting to retry" and "still setting up" can be told apart while
// the coarse health value stays a stable contract.
//
// There is no terminal state: containers restart unconditionally, so a failing container cycles
// between backoff and pulling indefinitely. The only way out of the cycle is stopping, and that is
// always driven from outside, by a changed document, a removed document, or the node shutting down.
type ContainerState int

// Container states.
//
//structprotogen:gen_enum
const (
	ContainerStatePending  ContainerState = iota // pending
	ContainerStatePulling                        // pulling
	ContainerStateStarting                       // starting
	ContainerStateRunning                        // running
	ContainerStateExited                         // exited
	ContainerStateBackoff                        // backoff
	ContainerStateStopping                       // stopping
)

// Health returns the coarse health summary for a state.
//
// Keeping this mapping in one place means the projection cannot drift between controllers.
func (state ContainerState) Health() ContainerHealth {
	switch state {
	case ContainerStatePending:
		return ContainerHealthPending
	case ContainerStatePulling, ContainerStateStarting:
		return ContainerHealthPulling
	case ContainerStateRunning:
		return ContainerHealthHealthy
	case ContainerStateExited, ContainerStateBackoff:
		return ContainerHealthDegraded
	case ContainerStateStopping:
		// Stopping is transient and externally driven; report the last meaningful value rather
		// than inventing a health for a container that is on its way out.
		return ContainerHealthHealthy
	default:
		return ContainerHealthDegraded
	}
}

// ContainerHealth is the coarse answer to "should I be looking at this container?".
type ContainerHealth int

// Container health values.
//
//structprotogen:gen_enum
const (
	ContainerHealthPending  ContainerHealth = iota // pending
	ContainerHealthPulling                         // pulling
	ContainerHealthHealthy                         // healthy
	ContainerHealthDegraded                        // degraded
)

// ContainerImagePhase describes the state of a container's image pull.
type ContainerImagePhase int

// Container image phases.
//
//structprotogen:gen_enum
const (
	ContainerImagePhasePending ContainerImagePhase = iota // pending
	ContainerImagePhasePulling                            // pulling
	ContainerImagePhaseReady                              // ready
	ContainerImagePhaseFailed                             // failed
)

// ContainerInstancePhase describes the state of a single container execution.
type ContainerInstancePhase int

// Container instance phases.
//
//structprotogen:gen_enum
const (
	// ContainerInstancePhaseCreated means the instance exists but the task has not started yet.
	ContainerInstancePhaseCreated ContainerInstancePhase = iota // created
	// ContainerInstancePhaseRunning means the task is running.
	ContainerInstancePhaseRunning // running
	// ContainerInstancePhaseTerminated means the task exited, for any reason. The exit code and
	// error carry the detail; the instance controller decides what happens next.
	ContainerInstancePhaseTerminated // terminated
	// ContainerInstancePhaseFailed means setup failed before the task ever started, e.g. the
	// cgroup or container could not be created.
	ContainerInstancePhaseFailed // failed
)

// Done reports whether the instance has finished, successfully or otherwise.
func (phase ContainerInstancePhase) Done() bool {
	return phase == ContainerInstancePhaseTerminated || phase == ContainerInstancePhaseFailed
}
