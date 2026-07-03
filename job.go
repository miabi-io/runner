/*
 * Copyright 2026 Jonas Kaninda
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"io"

	"github.com/miabi-io/runner/proto"
)

// Run/step status strings, matching the control plane's PipelineRunStatus so the
// reported values map straight onto the run and its steps.
const (
	statusRunning   = "running"
	statusSucceeded = "succeeded"
	statusFailed    = "failed"
)

// jobLog is the step ordinal for job-level (setup/checkout) log lines that don't
// belong to a specific step.
const jobLog = -1

// runJob reads the JobSpec that opens a job stream, prepares the job workspace,
// executes each step in order, and streams report frames back over the same
// stream, closing with a terminal Done (or Error) frame. JobSpec.Deadline bounds
// the whole run. It stops at the first failing step (non-zero exit or executor
// error), mirroring the control plane's sequential pipeline semantics.
func runJob(ctx context.Context, stream io.ReadWriter, exec Executor) error {
	job, err := proto.ReadJob(stream)
	if err != nil {
		return err
	}
	fw := proto.NewFrameWriter(stream)

	if !job.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, job.Deadline)
		defer cancel()
	}

	run, err := exec.Begin(ctx, job, func(line string) { _ = fw.Log(jobLog, line) })
	if err != nil {
		_ = fw.Err("prepare job: " + err.Error())
		_ = fw.Done(statusFailed)
		return err
	}
	defer run.Close()

	for _, step := range job.Steps {
		if err := ctx.Err(); err != nil {
			_ = fw.Err("job canceled: " + err.Error())
			_ = fw.Done(statusFailed)
			return err
		}
		_ = fw.Step(step.Ordinal, statusRunning)

		res, runErr := run.Step(ctx, step, func(line string) {
			_ = fw.Log(step.Ordinal, line)
		})
		if runErr != nil {
			_ = fw.Step(step.Ordinal, statusFailed)
			_ = fw.Err(runErr.Error())
			_ = fw.Done(statusFailed)
			return runErr
		}
		if res.Digest != "" {
			_ = fw.Result(step.Ordinal, res.Digest, res.Exit)
		}
		if res.Exit != 0 {
			_ = fw.Step(step.Ordinal, statusFailed)
			_ = fw.Done(statusFailed)
			return nil // a failed step is a completed (failed) run, not a runner error
		}
		_ = fw.Step(step.Ordinal, statusSucceeded)
	}

	_ = fw.Done(statusSucceeded)
	return nil
}
