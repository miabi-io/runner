/*
 * Copyright 2026 Jonas Kaninda
 * SPDX-License-Identifier: Apache-2.0
 */

// Package proto is the job/report wire contract between the Miabi control plane
// and a runner, carried over one yamux stream (opened by the control plane via
// the shared wstunnel transport). The control plane writes exactly one JobSpec,
// then reads a sequence of report Frames the runner emits as it executes; both
// are newline-delimited JSON. Keeping the contract in the runner module (which
// the control plane imports) means a single source of truth and independent
// versioning, mirroring how the transport lives in github.com/miabi-io/wstunnel.
package proto

import (
	"encoding/json"
	"io"
	"time"
)

// JobSpec is the control plane's request to execute one pipeline run on a runner.
// It carries the predefined, non-secret build context (the MIABI_* variables);
// secrets and the ready-to-use registry login are injected separately per job.
type JobSpec struct {
	RunID       uint   `json:"run_id"`
	RunNumber   int    `json:"run_number"`
	Pipeline    string `json:"pipeline"`
	WorkspaceID uint   `json:"workspace_id"`
	Workspace   string `json:"workspace"` // workspace handle (MIABI_WORKSPACE_NAME)
	AppID       *uint  `json:"app_id,omitempty"`
	App         string `json:"app,omitempty"`
	Commit      string `json:"commit,omitempty"`
	Ref         string `json:"ref,omitempty"`
	Branch      string `json:"branch,omitempty"`
	// SourceURL is the git remote the runner clones and checks out at Commit into
	// the workdir before running steps. Empty means the workspace is pre-populated
	// (or the job runs no build). Any credential is carried in the URL or the
	// injected job env, never logged.
	SourceURL  string     `json:"source_url,omitempty"`
	Repository string     `json:"repository,omitempty"` // fully-qualified image repo to push to
	Registry   string     `json:"registry,omitempty"`   // registry host
	Steps      []StepSpec `json:"steps"`
	Env        []string   `json:"env,omitempty"` // KEY=VALUE, non-secret MIABI_* context
	Deadline   time.Time  `json:"deadline"`      // hard job deadline (per-job cap)
}

// StepSpec is one step of a run, executed in an isolated container on the runner.
type StepSpec struct {
	Ordinal int      `json:"ordinal"`
	Name    string   `json:"name"`
	Uses    string   `json:"uses"`            // "build" | "deploy" | custom-step
	Image   string   `json:"image,omitempty"` // container image for a custom step
	Env     []string `json:"env,omitempty"`   // KEY=VALUE, step-scoped
	Run     []string `json:"run,omitempty"`   // command for a container step
}

// FrameType is the kind of report a runner sends back.
type FrameType string

const (
	FrameLog    FrameType = "log"    // a line of step output (streamed to pipeline:{runID})
	FrameStep   FrameType = "step"   // a step status transition (running/succeeded/failed)
	FrameResult FrameType = "result" // a build step produced an image (Digest)
	FrameDone   FrameType = "done"   // the run reached a terminal Status
	FrameError  FrameType = "error"  // a runner-side failure (Error)
)

// Frame is one report from the runner to the control plane. The meaningful
// payload fields depend on Type (see FrameType).
type Frame struct {
	Type   FrameType `json:"type"`
	Step   int       `json:"step,omitempty"`   // step ordinal for log/step/result
	Line   string    `json:"line,omitempty"`   // FrameLog
	Status string    `json:"status,omitempty"` // FrameStep / FrameDone
	Exit   int       `json:"exit,omitempty"`   // FrameResult exit code
	Digest string    `json:"digest,omitempty"` // FrameResult image digest (sha256:…)
	Error  string    `json:"error,omitempty"`  // FrameError
}

// WriteJob writes the single JobSpec that opens a job stream.
func WriteJob(w io.Writer, j JobSpec) error { return json.NewEncoder(w).Encode(j) }

// ReadJob reads the JobSpec at the start of a job stream.
func ReadJob(r io.Reader) (JobSpec, error) {
	var j JobSpec
	err := json.NewDecoder(r).Decode(&j)
	return j, err
}

// FrameWriter emits report frames back to the control plane over the stream.
type FrameWriter struct{ enc *json.Encoder }

// NewFrameWriter wraps the stream's write side. All emit helpers are safe to
// call from a single goroutine (the job runner); the control plane decodes the
// frames in order.
func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{enc: json.NewEncoder(w)} }

func (fw *FrameWriter) emit(f Frame) error { return fw.enc.Encode(f) }

// Log streams one output line for a step.
func (fw *FrameWriter) Log(step int, line string) error {
	return fw.emit(Frame{Type: FrameLog, Step: step, Line: line})
}

// Step reports a step's status transition.
func (fw *FrameWriter) Step(ordinal int, status string) error {
	return fw.emit(Frame{Type: FrameStep, Step: ordinal, Status: status})
}

// Result reports a build step's produced image digest and exit code.
func (fw *FrameWriter) Result(step int, digest string, exit int) error {
	return fw.emit(Frame{Type: FrameResult, Step: step, Digest: digest, Exit: exit})
}

// Done reports the run's terminal status (succeeded/failed/canceled).
func (fw *FrameWriter) Done(status string) error {
	return fw.emit(Frame{Type: FrameDone, Status: status})
}

// Err reports a runner-side failure that aborted the run.
func (fw *FrameWriter) Err(msg string) error {
	return fw.emit(Frame{Type: FrameError, Error: msg})
}
