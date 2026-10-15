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
	"io"
	"time"

	"github.com/jkaninda/logger"
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
// stream, closing with a terminal Done (or Error) frame.
func runJob(ctx context.Context, stream io.ReadWriter, exec Executor) error {
	job, err := proto.ReadJob(stream)
	if err != nil {
		logger.Warn("could not read the job spec", "error", err)
		return err
	}
	fw := proto.NewFrameWriter(stream)
	fields := jobFields(job)
	ref := jobRef(job)
	started := time.Now()
	tolerated := 0
	logger.Info("job started", fields...)

	if !job.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, job.Deadline)
		defer cancel()
	}

	run, err := exec.Begin(ctx, job, func(line string) { _ = fw.Log(jobLog, line) })
	if err != nil {

		logger.Error("job failed", with(fields, "phase", "prepare", "took", took(time.Since(started)), "error", err)...)
		_ = fw.Err("prepare job: " + err.Error())
		_ = fw.Done(statusFailed)
		return err
	}
	defer run.Close()

	for _, step := range job.Steps {
		if err := ctx.Err(); err != nil {
			logger.Warn("job canceled", with(fields, "took", took(time.Since(started)), "at_step", step.Ordinal, "error", err)...)
			_ = fw.Err("job canceled: " + err.Error())
			_ = fw.Done(statusFailed)
			return err
		}
		_ = fw.Step(step.Ordinal, statusRunning)
		logger.Debug("step started", with(ref, stepFields(step)...)...)

		stepStart := time.Now()
		res, runErr := run.Step(ctx, step, func(line string) {
			_ = fw.Log(step.Ordinal, line)
		})
		stepTook := took(time.Since(stepStart))
		if res.Digest != "" {
			_ = fw.Result(step.Ordinal, res.Digest, res.Exit)
		}

		if runErr != nil || res.Exit != 0 {
			_ = fw.Step(step.Ordinal, statusFailed)

			failed := with(with(ref, stepFields(step)...), "took", stepTook)
			if runErr != nil {
				failed = with(failed, "error", runErr)
			} else {
				failed = with(failed, "exit", res.Exit)
			}
			if step.ContinueOnError {
				failed = with(failed, "continue_on_error", true)
			}
			logger.Warn("step failed", failed...)
			if step.ContinueOnError {
				tolerated++
				note := "step failed"
				if runErr != nil {
					note += ": " + runErr.Error()
				}
				_ = fw.Log(step.Ordinal, note+" — continue-on-error is set, continuing")
				continue
			}
			if runErr != nil {
				logger.Error("job failed", with(fields, "took", took(time.Since(started)), "failed_step", step.Ordinal, "error", runErr)...)
				_ = fw.Err(runErr.Error())
				_ = fw.Done(statusFailed)
				return runErr
			}
			logger.Error("job failed", with(fields, "took", took(time.Since(started)), "failed_step", step.Ordinal, "exit", res.Exit)...)
			_ = fw.Done(statusFailed)
			return nil
		}
		_ = fw.Step(step.Ordinal, statusSucceeded)
		logger.Info("step succeeded", with(with(ref, stepFields(step)...), "took", stepTook)...)
	}

	done := with(fields, "took", took(time.Since(started)), "status", statusSucceeded)
	if tolerated > 0 {
		done = with(done, "tolerated_failures", tolerated)
	}
	logger.Info("job completed", done...)
	_ = fw.Done(statusSucceeded)
	return nil
}
