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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jkaninda/logger"
	"github.com/miabi-io/runner/proto"
)

// Uses the runner's own daemon, never a hosting node.
type dockerExecutor struct {
	cmd            commander
	docker         string // docker binary
	pack           string // pack (Cloud Native Buildpacks) binary
	git            string // git binary
	workRoot       string // parent dir for per-job workspaces
	defaultBuilder string // CNB builder used when a buildpack build supplies none
	// Kept apart from the per-job DOCKER_CONFIG so a builder created by one job is found by the next.
	buildxConfig string
	readDigest   func(metaFile string) (string, error) // injectable for tests
	hostPaths    *hostPathResolver                     // nil passes workspace paths to docker unchanged
}

func newDockerExecutor() *dockerExecutor {
	builder := os.Getenv("MIABI_RUNNER_DEFAULT_BUILDER")
	if builder == "" {
		builder = defaultBuilder
	}
	root := buildsDir()
	cmd := execCommander{}
	return &dockerExecutor{
		cmd: cmd, docker: "docker", pack: "pack", git: "git", workRoot: root, defaultBuilder: builder,
		buildxConfig: filepath.Join(root, ".buildx"), readDigest: readImageDigest,
		hostPaths: newHostPathResolver(cmd, "docker"),
	}
}

// hostPath returns path as the docker daemon sees it.
func (e *dockerExecutor) hostPath(path string) (string, error) {
	if e.hostPaths == nil {
		return path, nil
	}
	return e.hostPaths.resolve(path)
}

// checkHostPaths warns at startup when container steps cannot mount the workspace.
func (e *dockerExecutor) checkHostPaths() {
	if _, err := e.hostPath(e.workRoot); err != nil {
		logger.Warn("container steps will fail", "error", err)
	}
}

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
	// Outside the workspace so it never enters a build context; world-writable so any step user can append.
	envFile, err := os.CreateTemp(e.workRoot, fmt.Sprintf("miabi-env-%d-*.env", job.RunID))
	if err != nil {
		_ = os.RemoveAll(workdir)
		return nil, fmt.Errorf("create env file: %w", err)
	}
	_ = envFile.Close()
	_ = os.Chmod(envFile.Name(), 0o666)
	run.envFile = envFile.Name()
	if err := e.setupRegistryAuth(job, run, log); err != nil {
		run.Close()
		return nil, err
	}
	return run, nil
}

// Private per-job DOCKER_CONFIG outside the build context: no token clobbering between jobs, no COPY-out leak.
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
	// envFile is a per-job KEY=VALUE file, exposed to steps as $MIABI_ENV.
	envFile string
}

func (r *dockerJobRun) Close() {
	_ = os.RemoveAll(r.workdir)
	if r.cfgDir != "" {
		_ = os.RemoveAll(r.cfgDir)
	}
	if r.envFile != "" {
		_ = os.Remove(r.envFile)
	}
}

func (r *dockerJobRun) exportEnv(key, value string) {
	if r.envFile == "" {
		return
	}
	f, err := os.OpenFile(r.envFile, os.O_APPEND|os.O_WRONLY, 0o666)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "%s=%s\n", key, value)
}

// Later assignments to the same key win, since docker keeps the last -e for a key.
func (r *dockerJobRun) exportedEnv() []string {
	if r.envFile == "" {
		return nil
	}
	data, err := os.ReadFile(r.envFile)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "=") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// Per-job DOCKER_CONFIG, since a shared ~/.docker/config.json would be clobbered by concurrent jobs.
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
		// Deploy-by-digest is enqueued by the control plane, which holds the digest and target node.
		log("deploy handled by the control plane (deploy-by-digest)")
		return StepResult{}, nil
	default:
		return r.container(ctx, step, log)
	}
}

func (r *dockerJobRun) build(ctx context.Context, step proto.StepSpec, log func(string)) (StepResult, error) {
	if r.job.Repository == "" {
		return StepResult{}, errors.New("build step requires MIABI_IMAGE_REPOSITORY (no push target)")
	}
	tag := r.job.Repository + ":" + buildTag(r.job)

	switch resolveBuildMethod(r.workdir, step.Build) {
	case "buildpack":
		if len(platforms(step.Build)) > 0 {
			return StepResult{}, errors.New("buildpack builds produce the runner's own platform only; build for several platforms from a Dockerfile")
		}
		builder := ""
		if step.Build != nil {
			builder = strings.TrimSpace(step.Build.Builder)
		}
		if builder == "" {
			builder = r.e.defaultBuilder
		}
		log(fmt.Sprintf("building %s with buildpacks (builder %s%s)", tag, builder, cacheNote(step.Build)))
		name, args := r.authCmd(r.e.pack, packArgs(tag, builder, step.Build)...)
		if code, err := r.e.cmd.run(ctx, r.workdir, nil, log, name, args...); err != nil {
			return StepResult{}, fmt.Errorf("pack build: %w", err)
		} else if code != 0 {
			return StepResult{Exit: code}, nil
		}
	default: // dockerfile
		if len(platforms(step.Build)) > 0 {
			return r.buildx(ctx, step, tag, log)
		}
		buildArgs := []string{"build", "-t", tag}
		if noCache(step.Build) {
			buildArgs = append(buildArgs, "--no-cache")
		}
		if df := dockerfilePath(step.Build); df != "Dockerfile" {
			buildArgs = append(buildArgs, "-f", df)
		}
		cdir, err := contextDir(r.workdir, step.Build)
		if err != nil {
			return StepResult{}, err
		}
		buildArgs = append(buildArgs, buildArgFlags(step.Build, "--build-arg", "")...)
		rel := contextLabel(r.workdir, cdir)
		buildArgs = append(buildArgs, rel)
		log("building " + tag + " (context " + rel + cacheNote(step.Build) + ")")
		name, args := r.authCmd(r.e.docker, buildArgs...)
		if code, err := r.e.cmd.run(ctx, r.workdir, nil, log, name, args...); err != nil {
			return StepResult{}, fmt.Errorf("docker build: %w", err)
		} else if code != 0 {
			return StepResult{Exit: code}, nil
		}
	}

	log("pushing " + tag)
	name, args := r.authCmd(r.e.docker, "push", tag)
	if code, err := r.e.cmd.run(ctx, r.workdir, nil, log, name, args...); err != nil {
		return StepResult{}, fmt.Errorf("docker push: %w", err)
	} else if code != 0 {
		return StepResult{Exit: code}, nil
	}

	digest, err := r.digest(ctx, tag)
	if err != nil {
		return StepResult{}, err
	}
	log("pushed digest " + digest)
	// digest is only the sha256 hash, so rebuild the pullable repo@digest form.
	r.exportEnv("MIABI_IMAGE", tag)
	r.exportEnv("MIABI_IMAGE_DIGEST", r.job.Repository+"@"+digest)
	return StepResult{Digest: digest}, nil
}

const multiPlatformBuilder = "miabi-runner"

// A multi-platform image cannot be loaded into a daemon and pushed afterwards, so pushing is part of the build.
func (r *dockerJobRun) buildx(ctx context.Context, step proto.StepSpec, tag string, log func(string)) (StepResult, error) {
	builder, err := r.multiPlatformBuilder(ctx, log)
	if err != nil {
		return StepResult{}, err
	}
	cdir, err := contextDir(r.workdir, step.Build)
	if err != nil {
		return StepResult{}, err
	}
	meta := filepath.Join(r.workdir, ".miabi-build-metadata.json")
	args := []string{"buildx", "build"}
	if builder != "" {
		args = append(args, "--builder", builder)
	}
	args = append(args, "--platform", strings.Join(platforms(step.Build), ","), "-t", tag, "--push", "--metadata-file", meta)
	if df := dockerfilePath(step.Build); df != "Dockerfile" {
		args = append(args, "-f", df)
	}
	if noCache(step.Build) {
		args = append(args, "--no-cache")
	}
	args = append(args, buildxCacheFlags(step.Build)...)
	args = append(args, buildArgFlags(step.Build, "--build-arg", "")...)
	rel := contextLabel(r.workdir, cdir)
	args = append(args, rel)

	warnEmulators(step.Build, log)
	log("building " + tag + " (buildx, context " + rel + cacheNote(step.Build) + platformNote(step.Build) + ")")
	name, cmdArgs := r.buildxCmd(args...)
	if code, err := r.e.cmd.run(ctx, r.workdir, nil, log, name, cmdArgs...); err != nil {
		return StepResult{}, fmt.Errorf("docker buildx build: %w", err)
	} else if code != 0 {
		return StepResult{Exit: code}, nil
	}
	digest, err := r.e.readDigest(meta)
	if err != nil {
		return StepResult{}, fmt.Errorf("read build digest: %w", err)
	}
	log("pushed digest " + digest)
	r.exportEnv("MIABI_IMAGE", tag)
	r.exportEnv("MIABI_IMAGE_DIGEST", r.job.Repository+"@"+digest)
	return StepResult{Digest: digest}, nil
}

// The containerd image store builds multi-platform itself and keeps the daemon's registry settings.
func (r *dockerJobRun) multiPlatformBuilder(ctx context.Context, log func(string)) (string, error) {
	if out, err := r.e.cmd.capture(ctx, "", r.e.docker, "info", "--format", "{{json .DriverStatus}}"); err == nil &&
		strings.Contains(out, "io.containerd.snapshotter") {
		return "", nil
	}
	inspect := func() error {
		name, args := r.buildxCmd("buildx", "inspect", multiPlatformBuilder)
		_, err := r.e.cmd.capture(ctx, "", name, args...)
		return err
	}
	if inspect() == nil {
		return multiPlatformBuilder, nil
	}
	log("creating the " + multiPlatformBuilder + " buildx builder (the daemon cannot build for several platforms itself)")
	name, args := r.buildxCmd("buildx", "create", "--name", multiPlatformBuilder, "--driver", "docker-container")
	if code, err := r.e.cmd.run(ctx, r.workdir, nil, log, name, args...); err != nil || code != 0 {
		// A concurrent job may have created it first.
		if inspect() == nil {
			return multiPlatformBuilder, nil
		}
		if err == nil {
			err = fmt.Errorf("exit %d", code)
		}
		return "", fmt.Errorf("create buildx builder: %w", err)
	}
	return multiPlatformBuilder, nil
}

// BUILDX_CONFIG must outlive the job, unlike the per-job DOCKER_CONFIG.
func (r *dockerJobRun) buildxCmd(args ...string) (string, []string) {
	env := []string{"BUILDX_CONFIG=" + r.e.buildxConfig}
	if r.cfgDir != "" {
		env = append(env, "DOCKER_CONFIG="+r.cfgDir)
	}
	return "env", append(append(env, r.e.docker), args...)
}

func (r *dockerJobRun) container(ctx context.Context, step proto.StepSpec, log func(string)) (StepResult, error) {
	if step.Image == "" {
		return StepResult{}, fmt.Errorf("step %q has no image to run", step.Name)
	}
	workspace, err := r.e.hostPath(r.workdir)
	if err != nil {
		return StepResult{}, err
	}
	args := []string{"run", "--rm", "-w", "/workspace", "-v", workspace + ":/workspace"}

	// `-e NAME` takes the value from our env, so a resolved secret never lands in a readable command line.
	stepEnv := append([]string{}, r.job.Env...)
	stepEnv = append(stepEnv, r.exportedEnv()...)
	stepEnv = append(stepEnv, step.Env...)
	childEnv := make([]string, 0, len(stepEnv))
	for _, e := range stepEnv {
		name, _, ok := strings.Cut(e, "=")
		if !ok || name == "" {
			continue
		}
		args = append(args, "-e", name)
		childEnv = append(childEnv, e)
	}
	if r.envFile != "" {
		envFile, err := r.e.hostPath(r.envFile)
		if err != nil {
			return StepResult{}, err
		}
		args = append(args, "-v", envFile+":/miabi/env", "-e", "MIABI_ENV=/miabi/env")
	}
	if len(step.Run) > 0 {
		args = append(args, "--entrypoint", step.Run[0], step.Image)
		args = append(args, step.Run[1:]...)
	} else {
		args = append(args, step.Image)
	}

	name, cargs := r.authCmd(r.e.docker, args...)
	code, err := r.e.cmd.run(ctx, "", childEnv, log, name, cargs...)
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

// The pushed digest is the real identity deployed by; the tag is only a human-readable label.
func buildTag(job proto.JobSpec) string {
	if job.RunNumber > 0 {
		return "run-" + strconv.Itoa(job.RunNumber)
	}
	if job.RunID > 0 {
		return strconv.FormatUint(uint64(job.RunID), 10)
	}
	return "latest"
}
