/*
 * Copyright 2026 Jonas Kaninda
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"errors"

	"github.com/miabi-io/runner/proto"
)

// StepResult is the outcome of executing one job step.
type StepResult struct {
	Exit   int    // process exit code (0 = success)
	Digest string // image digest a build step pushed (sha256:…), empty otherwise
}

// Executor runs a single job step in isolation on the runner and streams its
// output via log. The job protocol is executor-agnostic on purpose: the
// rootless Docker/BuildKit backend drops in behind this interface without
// touching the wire contract or the run loop.
type Executor interface {
	Run(ctx context.Context, job proto.JobSpec, step proto.StepSpec, log func(line string)) (StepResult, error)
}

// ErrNoExecutor is returned by the default executor. A runner with no build
// backend configured fails a job loudly rather than silently reporting success.
var ErrNoExecutor = errors.New("no build backend configured on this runner")

// unconfiguredExecutor is the safe default until a Docker/BuildKit executor is
// wired (a later phase): it refuses every step so a misconfigured runner cannot
// report bogus successes.
type unconfiguredExecutor struct{}

func (unconfiguredExecutor) Run(context.Context, proto.JobSpec, proto.StepSpec, func(string)) (StepResult, error) {
	return StepResult{}, ErrNoExecutor
}
