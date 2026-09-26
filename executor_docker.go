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
	"path/filepath"
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
	// buildxConfig holds buildx's builder state. Kept apart from the per-job DOCKER_CONFIG, which is where
	// buildx would otherwise look, so a builder created by one job is found by the next.
	buildxConfig string
	readDigest   func(metaFile string) (string, error) // injectable for tests
}

func newDockerExecutor() *dockerExecutor {
	builder := os.Getenv("MIABI_RUNNER_DEFAULT_BUILDER")
	if builder == "" {
		builder = defaultBuilder
	}
	root := buildsDir()
	return &dockerExecutor{
		cmd: execCommander{}, docker: "docker", pack: "pack", git: "git", workRoot: root, defaultBuilder: builder,
		buildxConfig: filepath.Join(root, ".buildx"), readDigest: readImageDigest,
	}
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
	// Per-job env file ($MIABI_ENV) for step-to-step variable exports. A sibling
	// of the workspace (not inside it) so it's never part of a build context, and
	// world-writable so a container step running as any user can append to it.
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

// exportEnv appends a KEY=VALUE line to the job's $MIABI_ENV file so every later
// step sees it. Used by built-in steps (e.g. the build step publishing
// MIABI_IMAGE) exactly as a user step would via `echo K=V >> $MIABI_ENV`.
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

// exportedEnv reads the vars steps have written to $MIABI_ENV so far, as
// KEY=VALUE strings ready to pass to `docker run -e`. Blank lines and lines
// without '=' are ignored; later assignments to the same key win (docker keeps
// the last -e for a given key).
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
	// Publish the produced reference to later steps via $MIABI_ENV — the same
	// generic channel a user step uses. MIABI_IMAGE is the tag ref (readable);
	// MIABI_IMAGE_DIGEST the immutable repo@sha256 form (`digest` is only the
	// sha256:… hash, so rebuild the pullable repo@digest).
	r.exportEnv("MIABI_IMAGE", tag)
	r.exportEnv("MIABI_IMAGE_DIGEST", r.job.Repository+"@"+digest)
	return StepResult{Digest: digest}, nil
}

// multiPlatformBuilder is the buildx builder a runner creates when its daemon cannot build for several
// platforms itself.
const multiPlatformBuilder = "miabi-runner"

// buildx builds the step for its platforms and pushes one image carrying them all. The classic `docker build`
// produces the daemon's own platform only, and pushing is part of the build: a multi-platform image cannot
// be loaded into a daemon to be pushed afterwards.
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

// multiPlatformBuilder picks the buildx builder: none (the daemon's own) when the daemon uses the containerd
// image store, which builds for several platforms and keeps the daemon's registry settings; otherwise a
// docker-container builder, created once and reused.
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

// buildxCmd is authCmd for buildx: the per-job DOCKER_CONFIG for credentials, plus a BUILDX_CONFIG that
// outlives the job.
func (r *dockerJobRun) buildxCmd(args ...string) (string, []string) {
	env := []string{"BUILDX_CONFIG=" + r.e.buildxConfig}
	if r.cfgDir != "" {
		env = append(env, "DOCKER_CONFIG="+r.cfgDir)
	}
	return "env", append(append(env, r.e.docker), args...)
}

// container runs a custom step image with the workspace mounted at /workspace
// and the job + step env injected.
func (r *dockerJobRun) container(ctx context.Context, step proto.StepSpec, log func(string)) (StepResult, error) {
	if step.Image == "" {
		return StepResult{}, fmt.Errorf("step %q has no image to run", step.Name)
	}
	args := []string{"run", "--rm", "-w", "/workspace", "-v", r.workdir + ":/workspace"}

	// `-e NAME` (no value) tells docker to take it from our own environment, so a
	// resolved secret never lands in a command line every local user can read.
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
	// Mount the shared env file and point $MIABI_ENV at it so this step can export
	// its own vars to later steps (`echo KEY=VALUE >> $MIABI_ENV`).
	if r.envFile != "" {
		args = append(args, "-v", r.envFile+":/miabi/env", "-e", "MIABI_ENV=/miabi/env")
	}
	// A `run:` command overrides the image ENTRYPOINT. With no
	// command the image's own entrypoint/CMD runs unchanged.
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
