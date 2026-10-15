// Copyright (c) 2026 Jonas Kaninda.
//
// This file is part of Miabi.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"

	"github.com/miabi-io/runner/proto"
)

// StepResult is the outcome of executing one job step.
type StepResult struct {
	Exit   int    // process exit code (0 = success)
	Digest string // image digest a build step pushed (sha256:…), empty otherwise
}

// Executor prepares a job and runs its steps on the runner's local container
// backend. The concrete backend (docker CLI here; rootless BuildKit/Kaniko
// later) sits behind this interface, so the job protocol and run loop never
// change when the backend does.
type Executor interface {
	// Begin sets up the job — checkout source, registry login — and returns a
	// JobRun to execute steps. An error fails the whole job. Setup output is
	// streamed via log.
	Begin(ctx context.Context, job proto.JobSpec, log func(line string)) (JobRun, error)
}

// JobRun executes the steps of one prepared job and releases its workspace on
// Close.
type JobRun interface {
	Step(ctx context.Context, step proto.StepSpec, log func(line string)) (StepResult, error)
	Close()
}
