/*
 * Copyright 2026 Jonas Kaninda
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/miabi-io/runner/proto"
)

// dockerExecutor runs job steps with the runner's local `docker` CLI: a build
// step builds the checked-out source and pushes it by digest to the job's
// registry; a container step runs the step image with the workspace mounted.
// The command runner is injected so the logic is testable without a daemon.
// (The rootless BuildKit/Kaniko backend is a drop-in replacement behind the
// Executor interface — this uses the runner's OWN daemon, never a hosting node.)
type dockerExecutor struct {
	cmd            commander
	docker         string // docker binary
	pack           string // pack (Cloud Native Buildpacks) binary
	git            string // git binary
	workRoot       string // parent dir for per-job workspaces
	defaultBuilder string // CNB builder used when a buildpack build supplies none
}

func newDockerExecutor() *dockerExecutor {
	builder := os.Getenv("MIABI_RUNNER_DEFAULT_BUILDER")
	if builder == "" {
		builder = defaultBuilder
	}
	return &dockerExecutor{cmd: execCommander{}, docker: "docker", pack: "pack", git: "git", workRoot: buildsDir(), defaultBuilder: builder}
}

// Begin creates the job workspace, checks out the source at the job's commit (if
// a source URL was given), and logs in to the registry so build pushes need no
// login step.
func (e *dockerExecutor) Begin(ctx context.Context, job proto.JobSpec, log func(string)) (JobRun, error) {
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
	run := &dockerJobRun{e: e, job: job, workdir: workdir}
	if err := e.setupRegistryAuth(job, run, log); err != nil {
		_ = os.RemoveAll(workdir)
		return nil, err
	}
	return run, nil
}

// setupRegistryAuth writes this job's registry credential into a private
// DOCKER_CONFIG dir kept OUTSIDE the build context. This gives two properties:
//
//   - Isolation: concurrent jobs on the same docker daemon never share or clobber
//     a global ~/.docker/config.json. A plain `docker login` writes credentials
//     keyed by registry host, so two jobs logging into the same registry with
//     their own workspace-scoped tokens would overwrite each other and a push
//     could use the wrong token. Each job now authenticates only with its own.
//   - No token leak into the image: the dir is a sibling of the workdir, not
//     inside it, so the credential is never sent to the daemon as build context
//     (a malicious Dockerfile can't COPY it out).
//
// A blank token (anonymous build) sets up nothing and commands run unwrapped.
func (e *dockerExecutor) setupRegistryAuth(job proto.JobSpec, run *dockerJobRun, log func(string)) error {
	env := envMap(job.Env)
	reg, user, token := env["MIABI_REGISTRY"], env["MIABI_REGISTRY_USER"], env["MIABI_REGISTRY_TOKEN"]
	if token == "" {
		return nil // anonymous / no push credential
	}
	cfgDir, err := os.MkdirTemp(e.workRoot, fmt.Sprintf("miabi-auth-%d-", job.RunID))
	if err != nil {
		return fmt.Errorf("create registry auth dir: %w", err)
	}
	if err := writeDockerConfig(cfgDir, reg, user, token); err != nil {
		_ = os.RemoveAll(cfgDir)
		return fmt.Errorf("write registry config: %w", err)
	}
	run.cfgDir = cfgDir
	log("using registry " + reg + " (isolated per-job credentials)")
	return nil
}

// dockerJobRun executes the steps of one prepared job against its workspace.
type dockerJobRun struct {
	e       *dockerExecutor
	job     proto.JobSpec
	workdir string
	cfgDir  string // private DOCKER_CONFIG (per-job registry auth), a sibling of workdir
}

func (r *dockerJobRun) Close() {
	_ = os.RemoveAll(r.workdir)
	if r.cfgDir != "" {
		_ = os.RemoveAll(r.cfgDir)
	}
}

// authCmd wraps a docker/pack invocation so it uses this job's private
// DOCKER_CONFIG — its own registry credentials in an isolated dir — instead of a
// shared ~/.docker/config.json that concurrent jobs on the same daemon would
// clobber. With no per-job credential (anonymous build) the command is unchanged.
func (r *dockerJobRun) authCmd(bin string, args ...string) (string, []string) {
	if r.cfgDir == "" {
		return bin, args
	}
	return "env", append([]string{"DOCKER_CONFIG=" + r.cfgDir, bin}, args...)
}

func (r *dockerJobRun) Step(ctx context.Context, step proto.StepSpec, log func(string)) (StepResult, error) {
	switch step.Uses {
	case "build":
		return r.build(ctx, step, log)
	case "deploy":
		// The terminal deploy-by-digest is enqueued by the control plane (it holds
		// the run's digest and the target node); the runner has nothing to do.
		log("deploy handled by the control plane (deploy-by-digest)")
		return StepResult{}, nil
	default:
		return r.container(ctx, step, log)
	}
}

// build turns the checked-out source into an image — a Dockerfile build or a
// Cloud Native Buildpacks build (per the step's BuildConfig, auto-detected when
// unset) — pushes it, and returns the pushed digest so the deploy step (and
// provenance) can reference it by digest.
func (r *dockerJobRun) build(ctx context.Context, step proto.StepSpec, log func(string)) (StepResult, error) {
	if r.job.Repository == "" {
		return StepResult{}, errors.New("build step requires MIABI_IMAGE_REPOSITORY (no push target)")
	}
	tag := r.job.Repository + ":" + buildTag(r.job)

	switch resolveBuildMethod(r.workdir, step.Build) {
	case "buildpack":
		builder := ""
		if step.Build != nil {
			builder = strings.TrimSpace(step.Build.Builder)
		}
		if builder == "" {
			builder = r.e.defaultBuilder
		}
		log(fmt.Sprintf("building %s with buildpacks (builder %s)", tag, builder))
		name, args := r.authCmd(r.e.pack, packArgs(tag, builder, step.Build)...)
		if code, err := r.e.cmd.run(ctx, r.workdir, log, name, args...); err != nil {
			return StepResult{}, fmt.Errorf("pack build: %w", err)
		} else if code != 0 {
			return StepResult{Exit: code}, nil
		}
	default: // dockerfile
		buildArgs := []string{"build", "-t", tag}
		if df := dockerfilePath(step.Build); df != "Dockerfile" {
			buildArgs = append(buildArgs, "-f", df)
		}
		buildArgs = append(buildArgs, ".")
		log("building " + tag)
		name, args := r.authCmd(r.e.docker, buildArgs...)
		if code, err := r.e.cmd.run(ctx, r.workdir, log, name, args...); err != nil {
			return StepResult{}, fmt.Errorf("docker build: %w", err)
		} else if code != 0 {
			return StepResult{Exit: code}, nil
		}
	}

	log("pushing " + tag)
	name, args := r.authCmd(r.e.docker, "push", tag)
	if code, err := r.e.cmd.run(ctx, r.workdir, log, name, args...); err != nil {
		return StepResult{}, fmt.Errorf("docker push: %w", err)
	} else if code != 0 {
		return StepResult{Exit: code}, nil
	}

	digest, err := r.digest(ctx, tag)
	if err != nil {
		return StepResult{}, err
	}
	log("pushed digest " + digest)
	return StepResult{Digest: digest}, nil
}

// container runs a custom step image with the workspace mounted at /workspace
// and the job + step env injected.
func (r *dockerJobRun) container(ctx context.Context, step proto.StepSpec, log func(string)) (StepResult, error) {
	if step.Image == "" {
		return StepResult{}, fmt.Errorf("step %q has no image to run", step.Name)
	}
	args := []string{"run", "--rm", "-w", "/workspace", "-v", r.workdir + ":/workspace"}
	for _, e := range append(append([]string{}, r.job.Env...), step.Env...) {
		args = append(args, "-e", e)
	}
	args = append(args, step.Image)
	args = append(args, step.Run...)

	name, cargs := r.authCmd(r.e.docker, args...)
	code, err := r.e.cmd.run(ctx, "", log, name, cargs...)
	if err != nil {
		return StepResult{}, err
	}
	return StepResult{Exit: code}, nil
}

// digest reads the digest docker recorded for the just-pushed tag.
func (r *dockerJobRun) digest(ctx context.Context, tag string) (string, error) {
	out, err := r.e.cmd.capture(ctx, "", r.e.docker, "inspect", "--format", "{{index .RepoDigests 0}}", tag)
	if err != nil {
		return "", fmt.Errorf("inspect digest: %w", err)
	}
	if _, d, ok := strings.Cut(out, "@"); ok && d != "" {
		return d, nil // out is repo@sha256:…
	}
	return "", fmt.Errorf("no pushed digest for %s (got %q)", tag, out)
}

// buildTag names the built image: run-<number> for a pipeline run, else the
// deploy id (RunID carries the deployment id for a deploy build), else latest.
// The pushed digest is the real identity the control plane deploys by; the tag is
// a human-readable, unique-per-build label alongside <workspace>/<app>.
func buildTag(job proto.JobSpec) string {
	if job.RunNumber > 0 {
		return "run-" + strconv.Itoa(job.RunNumber)
	}
	if job.RunID > 0 {
		return strconv.FormatUint(uint64(job.RunID), 10)
	}
	return "latest"
}
