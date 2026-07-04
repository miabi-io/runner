/*
 * Copyright 2026 Jonas Kaninda
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/miabi-io/runner/proto"
)

// buildkitExecutor builds rootless and daemonless with BuildKit
// (buildctl-daemonless.sh): a build step builds the checked-out Dockerfile and
// pushes it straight to the registry by digest — no Docker daemon, no
// /var/run/docker.sock. This is the P4 backend that removes the privilege
// surface; selected with MIABI_RUNNER_BUILDER=buildkit. Container steps need a
// container runtime and are not supported here (use the docker backend).
type buildkitExecutor struct {
	cmd        commander
	buildctl   string // buildctl-daemonless.sh
	git        string
	workRoot   string
	readDigest func(metaFile string) (string, error) // injectable for tests
}

func newBuildkitExecutor() *buildkitExecutor {
	return &buildkitExecutor{
		cmd:        execCommander{},
		buildctl:   "buildctl-daemonless.sh",
		git:        "git",
		workRoot:   os.TempDir(),
		readDigest: readImageDigest,
	}
}

// Begin creates the workspace, checks out source, and writes a docker
// config.json with the registry login so BuildKit's push authenticates with no
// login step (the plan's pre-written $DOCKER_CONFIG/config.json).
func (e *buildkitExecutor) Begin(ctx context.Context, job proto.JobSpec, log func(string)) (JobRun, error) {
	workdir, err := os.MkdirTemp(e.workRoot, fmt.Sprintf("miabi-run-%d-", job.RunID))
	if err != nil {
		return nil, fmt.Errorf("create workspace: %w", err)
	}
	if job.SourceURL != "" {
		if err := gitCheckout(ctx, e.cmd, e.git, workdir, job, log); err != nil {
			_ = os.RemoveAll(workdir)
			return nil, err
		}
	}
	env := envMap(job.Env)
	cfgDir := filepath.Join(workdir, ".docker")
	if err := writeDockerConfig(cfgDir, env["MIABI_REGISTRY"], env["MIABI_REGISTRY_USER"], env["MIABI_REGISTRY_TOKEN"]); err != nil {
		_ = os.RemoveAll(workdir)
		return nil, fmt.Errorf("write registry config: %w", err)
	}
	hasAuth := env["MIABI_REGISTRY_TOKEN"] != ""
	return &buildkitJobRun{e: e, job: job, workdir: workdir, cfgDir: cfgDir, hasAuth: hasAuth}, nil
}

type buildkitJobRun struct {
	e       *buildkitExecutor
	job     proto.JobSpec
	workdir string
	cfgDir  string
	hasAuth bool
}

func (r *buildkitJobRun) Close() { _ = os.RemoveAll(r.workdir) }

func (r *buildkitJobRun) Step(ctx context.Context, step proto.StepSpec, log func(string)) (StepResult, error) {
	switch step.Uses {
	case "build":
		return r.build(ctx, step, log)
	case "deploy":
		log("deploy handled by the control plane (deploy-by-digest)")
		return StepResult{}, nil
	default:
		return StepResult{}, fmt.Errorf("container step %q needs a container runtime; the buildkit runner backend builds only (use a docker-backed runner)", step.Name)
	}
}

// build runs a rootless BuildKit build that pushes the image and writes a
// metadata file, from which the pushed digest is read. BuildKit is daemonless
// and Dockerfile-only here; a buildpack build needs the docker backend.
func (r *buildkitJobRun) build(ctx context.Context, step proto.StepSpec, log func(string)) (StepResult, error) {
	if r.job.Repository == "" {
		return StepResult{}, errors.New("build step requires MIABI_IMAGE_REPOSITORY (no push target)")
	}
	if resolveBuildMethod(r.workdir, step.Build) == "buildpack" {
		return StepResult{}, errors.New("buildpack builds require a docker-backed runner (MIABI_RUNNER_BUILDER=docker); the rootless buildkit backend builds Dockerfiles only")
	}
	ref := r.job.Repository + ":" + buildTag(r.job)
	meta := filepath.Join(r.workdir, ".miabi-build-metadata.json")

	buildArgs := []string{
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + r.workdir,
		"--local", "dockerfile=" + r.workdir,
		"--opt", "filename=" + dockerfilePath(step.Build),
		"--output", fmt.Sprintf("type=image,name=%s,push=true", ref),
		"--metadata-file", meta,
	}
	// Point BuildKit at the per-job docker config for its push credential.
	name, args := r.buildctlCmd(buildArgs)

	log("building " + ref + " (rootless buildkit)")
	if code, err := r.e.cmd.run(ctx, r.workdir, log, name, args...); err != nil {
		return StepResult{}, fmt.Errorf("buildctl: %w", err)
	} else if code != 0 {
		return StepResult{Exit: code}, nil
	}

	digest, err := r.e.readDigest(meta)
	if err != nil {
		return StepResult{}, fmt.Errorf("read build digest: %w", err)
	}
	log("pushed digest " + digest)
	return StepResult{Digest: digest}, nil
}

// buildctlCmd prepends `env DOCKER_CONFIG=<dir>` when a registry credential was
// written, so buildctl finds the push auth.
func (r *buildkitJobRun) buildctlCmd(buildArgs []string) (string, []string) {
	if r.hasAuth {
		return "env", append([]string{"DOCKER_CONFIG=" + r.cfgDir, r.e.buildctl}, buildArgs...)
	}
	return r.e.buildctl, buildArgs
}

// readImageDigest reads the pushed image digest from a buildctl --metadata-file.
func readImageDigest(metaFile string) (string, error) {
	b, err := os.ReadFile(metaFile)
	if err != nil {
		return "", err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return "", err
	}
	if d, ok := m["containerimage.digest"].(string); ok && d != "" {
		return d, nil
	}
	return "", fmt.Errorf("no containerimage.digest in build metadata")
}
