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

// Package proto is the newline-delimited JSON job/report contract between the control plane and a runner.
package proto

import (
	"encoding/json"
	"io"
	"time"
)

// JobSpec carries only non-secret build context; secrets and registry login are injected separately.
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
	// SourceURL is cloned at Commit; empty means a pre-populated workspace. Any embedded credential is never logged.
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
	// ContinueOnError reports the step failed but lets the run continue and succeed.
	ContinueOnError bool `json:"continue_on_error,omitempty"`
	// Build configures a build step; nil or empty Method auto-detects Dockerfile vs buildpacks.
	Build *BuildConfig `json:"build,omitempty"`
}

type BuildConfig struct {
	// Method is "" | "auto" | "dockerfile" | "buildpack" (empty/auto auto-detects).
	Method string `json:"method,omitempty"`
	// Dockerfile is relative to the source root, not to Context, like docker build -f.
	Dockerfile string `json:"dockerfile,omitempty"`
	// Context is relative to the source root, settable apart from Dockerfile for monorepos.
	Context string `json:"context,omitempty"`
	// Builder is the CNB builder image for the buildpack method (empty = runner default).
	Builder string `json:"builder,omitempty"`
	// Buildpacks are extra buildpacks to apply (pack --buildpack).
	Buildpacks []string `json:"buildpacks,omitempty"`
	// BuildEnv is build-time env for the buildpack method (pack --env KEY=VALUE).
	BuildEnv map[string]string `json:"build_env,omitempty"`
	// BuildArgs are recorded in image history and readable by anyone who can pull, so never use them for secrets.
	BuildArgs map[string]string `json:"build_args,omitempty"`
	// NoCache builds every layer from scratch, ignoring any cache the builder
	NoCache bool `json:"no_cache,omitempty"`
	// CacheFrom are registry refs to seed the layer cache from. Ignored when NoCache is set.
	CacheFrom []string `json:"cache_from,omitempty"`
	// CacheTo is separate from CacheFrom so a branch build never writes into a cache a trunk build trusts.
	CacheTo string `json:"cache_to,omitempty"`
	// Platforms builds one multi-platform image; only a runner advertising FeatureMultiPlatform honours it.
	Platforms []string `json:"platforms,omitempty"`
}

// HeaderFeatures lets the control plane schedule a job only on a runner that can carry it out.
const HeaderFeatures = "X-Runner-Features"

// FeatureMultiPlatform means the runner honours BuildConfig.Platforms.
const FeatureMultiPlatform = "multi-platform"

// FrameType is the kind of report a runner sends back.
type FrameType string

const (
	FrameLog    FrameType = "log"    // a line of step output (streamed to pipeline:{runID})
	FrameStep   FrameType = "step"   // a step status transition (running/succeeded/failed)
	FrameResult FrameType = "result" // a build step produced an image (Digest)
	FrameDone   FrameType = "done"   // the run reached a terminal Status
	FrameError  FrameType = "error"  // a runner-side failure (Error)
)

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

// NewFrameWriter is not concurrency-safe: write from one goroutine so the control plane decodes frames in order.
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
