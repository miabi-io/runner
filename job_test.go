/*
 * Copyright 2026 Jonas Kaninda
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"

	"github.com/miabi-io/runner/proto"
)

// fakeExecutor returns a scripted result/error per step ordinal.
type fakeExecutor struct {
	results map[int]StepResult
	errs    map[int]error
	logs    map[int]string
}

func (f fakeExecutor) Begin(context.Context, proto.JobSpec, func(string)) (JobRun, error) {
	return fakeRun(f), nil
}

type fakeRun fakeExecutor

func (f fakeRun) Close() {}
func (f fakeRun) Step(_ context.Context, step proto.StepSpec, log func(string)) (StepResult, error) {
	if line, ok := f.logs[step.Ordinal]; ok {
		log(line)
	}
	if err, ok := f.errs[step.Ordinal]; ok {
		return StepResult{}, err
	}
	return f.results[step.Ordinal], nil
}

// drive writes job to one end of a pipe, runs runJob on the other, and returns
// the frames the runner emitted.
func drive(t *testing.T, job proto.JobSpec, exec Executor) []proto.Frame {
	t.Helper()
	cp, runner := net.Pipe()
	defer cp.Close()

	done := make(chan struct{})
	go func() {
		_ = runJob(context.Background(), runner, exec)
		_ = runner.Close()
		close(done)
	}()

	if err := proto.WriteJob(cp, job); err != nil {
		t.Fatalf("WriteJob: %v", err)
	}
	var frames []proto.Frame
	dec := json.NewDecoder(cp)
	for {
		var f proto.Frame
		if err := dec.Decode(&f); err != nil {
			break // runner closed the stream
		}
		frames = append(frames, f)
	}
	<-done
	return frames
}

func TestRunJobSuccess(t *testing.T) {
	job := proto.JobSpec{
		RunID: 1,
		Steps: []proto.StepSpec{{Ordinal: 0, Name: "build", Uses: "build"}, {Ordinal: 1, Name: "deploy", Uses: "deploy"}},
	}
	exec := fakeExecutor{
		results: map[int]StepResult{0: {Digest: "sha256:cafe"}, 1: {}},
		logs:    map[int]string{0: "building"},
	}
	frames := drive(t, job, exec)

	// Expect: step0 running, log, result(digest), step0 succeeded, step1 running,
	// step1 succeeded, done succeeded.
	last := frames[len(frames)-1]
	if last.Type != proto.FrameDone || last.Status != statusSucceeded {
		t.Fatalf("final frame = %+v, want done/succeeded", last)
	}
	var sawResult, sawLog bool
	for _, f := range frames {
		if f.Type == proto.FrameResult && f.Digest == "sha256:cafe" {
			sawResult = true
		}
		if f.Type == proto.FrameLog && f.Line == "building" {
			sawLog = true
		}
	}
	if !sawResult || !sawLog {
		t.Errorf("missing result/log frame: result=%v log=%v (%+v)", sawResult, sawLog, frames)
	}
}

func TestRunJobStopsAtFailingStep(t *testing.T) {
	job := proto.JobSpec{
		RunID: 2,
		Steps: []proto.StepSpec{{Ordinal: 0, Name: "build"}, {Ordinal: 1, Name: "deploy"}},
	}
	// Step 0 fails with an executor error; step 1 must never run.
	exec := fakeExecutor{errs: map[int]error{0: errors.New("boom")}}
	frames := drive(t, job, exec)

	last := frames[len(frames)-1]
	if last.Type != proto.FrameDone || last.Status != statusFailed {
		t.Fatalf("final frame = %+v, want done/failed", last)
	}
	for _, f := range frames {
		if f.Step == 1 {
			t.Errorf("step 1 should not have run after step 0 failed: %+v", f)
		}
	}
}

func TestRunJobNonZeroExitFails(t *testing.T) {
	job := proto.JobSpec{RunID: 3, Steps: []proto.StepSpec{{Ordinal: 0, Name: "test"}}}
	exec := fakeExecutor{results: map[int]StepResult{0: {Exit: 2}}}
	frames := drive(t, job, exec)
	last := frames[len(frames)-1]
	if last.Type != proto.FrameDone || last.Status != statusFailed {
		t.Fatalf("final frame = %+v, want done/failed on non-zero exit", last)
	}
}

// A continue-on-error step that fails is reported failed, but the run keeps going
// and still succeeds — whether the step exited non-zero or errored outright.
func TestRunJobContinueOnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		exec fakeExecutor
	}{
		{"non-zero exit", fakeExecutor{results: map[int]StepResult{0: {Exit: 1}}}},
		{"executor error", fakeExecutor{errs: map[int]error{0: errors.New("boom")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := proto.JobSpec{RunID: 4, Steps: []proto.StepSpec{
				{Ordinal: 0, Name: "scan", ContinueOnError: true},
				{Ordinal: 1, Name: "deploy", Uses: "deploy"},
			}}
			frames := drive(t, job, tc.exec)

			last := frames[len(frames)-1]
			if last.Type != proto.FrameDone || last.Status != statusSucceeded {
				t.Fatalf("final frame = %+v, want done/succeeded despite the allowed failure", last)
			}
			var step0Failed, step1Ran bool
			for _, f := range frames {
				if f.Step == 0 && f.Type == proto.FrameStep && f.Status == statusFailed {
					step0Failed = true
				}
				if f.Step == 1 && f.Type == proto.FrameStep {
					step1Ran = true
				}
			}
			if !step0Failed {
				t.Error("the allowed-failure step should still be reported failed")
			}
			if !step1Ran {
				t.Error("the next step should run after a continue-on-error failure")
			}
		})
	}
}
