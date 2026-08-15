/*
 * Copyright 2026 Jonas Kaninda
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miabi-io/runner/proto"
)

// fakeCommander records invocations and returns scripted exit codes / output so
// the executor's command construction and result handling are testable without a
// real docker/git.
type fakeCommander struct {
	calls     []string
	buildExit int
	pushExit  int
	runExit   int
	digestOut string
	loginErr  error
	logins    int
	env       []string // child env of the last run
}

func (f *fakeCommander) run(_ context.Context, _ string, env []string, log func(string), name string, args ...string) (int, error) {
	f.env = env
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	log(name + " output")
	if name == "docker" && len(args) > 0 {
		switch args[0] {
		case "build":
			return f.buildExit, nil
		case "push":
			return f.pushExit, nil
		case "run":
			return f.runExit, nil
		}
	}
	return 0, nil // git clone/checkout
}

// inEnv reports whether the last run received this KEY=VALUE in its child env.
func (f *fakeCommander) inEnv(want string) bool {
	for _, e := range f.env {
		if e == want {
			return true
		}
	}
	return false
}

func (f *fakeCommander) capture(_ context.Context, _, name string, args ...string) (string, error) {
	f.calls = append(f.calls, "capture "+name+" "+strings.Join(args, " "))
	return f.digestOut, nil
}

func (f *fakeCommander) loginStdin(context.Context, string, string, ...string) error {
	f.logins++
	return f.loginErr
}

func (f *fakeCommander) called(substr string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func newTestExecutor(t *testing.T, cmd commander) *dockerExecutor {
	t.Helper()
	return &dockerExecutor{cmd: cmd, docker: "docker", pack: "pack", git: "git", workRoot: t.TempDir(), defaultBuilder: defaultBuilder}
}

func TestBeginCheckoutAndAuth(t *testing.T) {
	fc := &fakeCommander{}
	e := newTestExecutor(t, fc)
	job := proto.JobSpec{
		RunID:     5,
		SourceURL: "https://git.example.com/acme/web.git",
		Commit:    "abcdef1234567890",
		Env:       []string{"MIABI_REGISTRY=reg.example.com", "MIABI_REGISTRY_USER=miabi-job", "MIABI_REGISTRY_TOKEN=mb_secret"},
	}
	run, err := e.Begin(context.Background(), job, func(string) {})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer run.Close()

	if !fc.called("git clone https://git.example.com/acme/web.git") {
		t.Errorf("expected git clone, calls=%v", fc.calls)
	}
	if !fc.called("git checkout --detach abcdef1234567890") {
		t.Errorf("expected git checkout, calls=%v", fc.calls)
	}
	// No shared/global `docker login` — the credential is isolated per job instead.
	if fc.logins != 0 {
		t.Errorf("expected no global docker login, got %d", fc.logins)
	}
	dr := run.(*dockerJobRun)
	if dr.cfgDir == "" {
		t.Fatal("expected a per-job DOCKER_CONFIG dir")
	}
	// The auth dir must live OUTSIDE the build context, so a Dockerfile can't COPY
	// the token out of the build context.
	if strings.HasPrefix(dr.cfgDir, dr.workdir) {
		t.Errorf("auth dir %q must not be inside the build context %q", dr.cfgDir, dr.workdir)
	}
	data, err := os.ReadFile(filepath.Join(dr.cfgDir, "config.json"))
	if err != nil {
		t.Fatalf("read per-job config.json: %v", err)
	}
	if !strings.Contains(string(data), "reg.example.com") {
		t.Errorf("per-job config missing registry auth: %s", data)
	}
}

// With a registry credential, build and push must run under the job's private
// DOCKER_CONFIG (via an `env DOCKER_CONFIG=<dir>` prefix) so concurrent jobs never
// share a global docker login.
func TestBuildPushUsePerJobDockerConfig(t *testing.T) {
	fc := &fakeCommander{digestOut: "reg.example.com/ws-42/web@sha256:cafebabe"}
	e := newTestExecutor(t, fc)
	job := proto.JobSpec{
		RunID:      8,
		Repository: "reg.example.com/ws-42/web",
		Commit:     "abcdef1234567890",
		Env:        []string{"MIABI_REGISTRY=reg.example.com", "MIABI_REGISTRY_USER=miabi-job", "MIABI_REGISTRY_TOKEN=mb_secret"},
	}
	run, err := e.Begin(context.Background(), job, func(string) {})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer run.Close()
	dr := run.(*dockerJobRun)

	if _, err := run.Step(context.Background(), proto.StepSpec{Name: "build", Uses: "build"}, func(string) {}); err != nil {
		t.Fatalf("build step: %v", err)
	}
	wantBuild := "env DOCKER_CONFIG=" + dr.cfgDir + " docker build -t reg.example.com/ws-42/web:8 ."
	if !fc.called(wantBuild) {
		t.Errorf("build not wrapped with per-job DOCKER_CONFIG: %v", fc.calls)
	}
	wantPush := "env DOCKER_CONFIG=" + dr.cfgDir + " docker push reg.example.com/ws-42/web:8"
	if !fc.called(wantPush) {
		t.Errorf("push not wrapped with per-job DOCKER_CONFIG: %v", fc.calls)
	}
}

func TestBuildStepPushesByDigest(t *testing.T) {
	fc := &fakeCommander{digestOut: "reg.example.com/ws-42/web@sha256:cafebabe"}
	e := newTestExecutor(t, fc)
	job := proto.JobSpec{RunID: 6, Repository: "reg.example.com/ws-42/web", Commit: "abcdef1234567890"}
	run, err := e.Begin(context.Background(), job, func(string) {})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer run.Close()

	res, err := run.Step(context.Background(), proto.StepSpec{Ordinal: 0, Name: "build", Uses: "build"}, func(string) {})
	if err != nil {
		t.Fatalf("build step: %v", err)
	}
	if res.Digest != "sha256:cafebabe" {
		t.Errorf("digest = %q, want sha256:cafebabe", res.Digest)
	}
	// Tag is the deploy id (RunID) for a deploy build; build then push then inspect.
	if !fc.called("docker build -t reg.example.com/ws-42/web:6 .") {
		t.Errorf("build command wrong: %v", fc.calls)
	}
	if !fc.called("docker push reg.example.com/ws-42/web:6") {
		t.Errorf("push command missing: %v", fc.calls)
	}
}

func TestBuildStepBuildpack(t *testing.T) {
	fc := &fakeCommander{digestOut: "reg.example.com/ws-42/web@sha256:cafebabe"}
	e := newTestExecutor(t, fc)
	job := proto.JobSpec{RunID: 7, Repository: "reg.example.com/ws-42/web", Commit: "abcdef1234567890"}
	run, err := e.Begin(context.Background(), job, func(string) {})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer run.Close()

	step := proto.StepSpec{Name: "build", Uses: "build", Build: &proto.BuildConfig{
		Method:     "buildpack",
		Builder:    "paketobuildpacks/builder-jammy-base",
		Buildpacks: []string{"paketo-buildpacks/nodejs"},
		BuildEnv:   map[string]string{"BP_NODE_VERSION": "20"},
	}}
	res, err := run.Step(context.Background(), step, func(string) {})
	if err != nil {
		t.Fatalf("build step: %v", err)
	}
	if res.Digest != "sha256:cafebabe" {
		t.Errorf("digest = %q, want sha256:cafebabe", res.Digest)
	}
	if !fc.called("pack build reg.example.com/ws-42/web:7 --path . --builder paketobuildpacks/builder-jammy-base") {
		t.Errorf("pack build command wrong: %v", fc.calls)
	}
	if !fc.called("--buildpack paketo-buildpacks/nodejs") || !fc.called("--env BP_NODE_VERSION=20") {
		t.Errorf("buildpack/env flags missing: %v", fc.calls)
	}
	if fc.called("docker build") {
		t.Error("buildpack build must not invoke docker build")
	}
	if !fc.called("docker push reg.example.com/ws-42/web:7") {
		t.Errorf("push missing after buildpack build: %v", fc.calls)
	}
}

func TestResolveBuildMethod(t *testing.T) {
	dir := t.TempDir()
	if got := resolveBuildMethod(dir, nil); got != "dockerfile" {
		t.Errorf("nil config = %q, want dockerfile (historical default)", got)
	}
	if got := resolveBuildMethod(dir, &proto.BuildConfig{Method: "buildpack"}); got != "buildpack" {
		t.Errorf("explicit buildpack = %q", got)
	}
	if got := resolveBuildMethod(dir, &proto.BuildConfig{Method: "auto"}); got != "buildpack" {
		t.Errorf("auto with no Dockerfile = %q, want buildpack", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveBuildMethod(dir, &proto.BuildConfig{Method: "auto"}); got != "dockerfile" {
		t.Errorf("auto with a Dockerfile = %q, want dockerfile", got)
	}
}

func TestBuildStepFailsOnNonZeroBuild(t *testing.T) {
	fc := &fakeCommander{buildExit: 1}
	e := newTestExecutor(t, fc)
	run, _ := e.Begin(context.Background(), proto.JobSpec{Repository: "reg/x"}, func(string) {})
	defer run.Close()
	res, err := run.Step(context.Background(), proto.StepSpec{Uses: "build"}, func(string) {})
	if err != nil {
		t.Fatalf("a failed build is not a runner error: %v", err)
	}
	if res.Exit != 1 {
		t.Errorf("exit = %d, want 1", res.Exit)
	}
	if fc.called("docker push") {
		t.Error("must not push after a failed build")
	}
}

func TestContainerStepMountsWorkspace(t *testing.T) {
	fc := &fakeCommander{}
	e := newTestExecutor(t, fc)
	run, _ := e.Begin(context.Background(), proto.JobSpec{Env: []string{"FOO=bar"}}, func(string) {})
	defer run.Close()
	// The control plane sends Run as ["sh", "-c", "<script>"]; the runner runs it
	// as the container entrypoint so an image's own ENTRYPOINT can't swallow it.
	_, err := run.Step(context.Background(), proto.StepSpec{
		Name: "test", Image: "golang:1.25", Env: []string{"CI=true"}, Run: []string{"sh", "-c", "go test ./..."},
	}, func(string) {})
	if err != nil {
		t.Fatalf("container step: %v", err)
	}
	if !fc.called("docker run --rm -w /workspace -v") || !fc.called("--entrypoint sh golang:1.25 -c go test ./...") {
		t.Errorf("container run command wrong: %v", fc.calls)
	}
	if !fc.called("-e FOO") || !fc.called("-e CI") {
		t.Errorf("job + step env not passed through: %v", fc.calls)
	}
	if !fc.inEnv("FOO=bar") || !fc.inEnv("CI=true") {
		t.Errorf("job + step env not injected: %v", fc.env)
	}
}

// After a build step, a later container step sees the produced image reference
// as MIABI_IMAGE / MIABI_IMAGE_DIGEST (so it can scan/test the built artifact).
func TestContainerStepSeesBuiltImage(t *testing.T) {
	fc := &fakeCommander{digestOut: "reg.example.com/ws-42/web@sha256:cafebabe"}
	e := newTestExecutor(t, fc)
	job := proto.JobSpec{RunNumber: 57, Repository: "reg.example.com/ws-42/web"}
	run, _ := e.Begin(context.Background(), job, func(string) {})
	defer run.Close()

	if _, err := run.Step(context.Background(), proto.StepSpec{Name: "build", Uses: "build"}, func(string) {}); err != nil {
		t.Fatalf("build step: %v", err)
	}
	if _, err := run.Step(context.Background(), proto.StepSpec{
		Name: "scan", Image: "aquasec/trivy:latest",
		Run: []string{"sh", "-c", "trivy image $MIABI_IMAGE"},
	}, func(string) {}); err != nil {
		t.Fatalf("scan step: %v", err)
	}
	if !fc.inEnv("MIABI_IMAGE=reg.example.com/ws-42/web:run-57") {
		t.Errorf("MIABI_IMAGE not exported to the scan step: %v", fc.env)
	}
	if !fc.inEnv("MIABI_IMAGE_DIGEST=reg.example.com/ws-42/web@sha256:cafebabe") {
		t.Errorf("MIABI_IMAGE_DIGEST not exported: %v", fc.env)
	}
}

// Any variable a step writes to $MIABI_ENV is visible to later steps, and every
// container step gets the env file mounted so it can export more — the generic
// mechanism the build step's MIABI_IMAGE also rides on, with no per-var runner code.
func TestStepEnvExportPropagates(t *testing.T) {
	fc := &fakeCommander{}
	e := newTestExecutor(t, fc)
	run, _ := e.Begin(context.Background(), proto.JobSpec{}, func(string) {})
	defer run.Close()
	// Stand in for an earlier step running `echo VERSION=1.2.3 >> $MIABI_ENV`.
	run.(*dockerJobRun).exportEnv("VERSION", "1.2.3")

	if _, err := run.Step(context.Background(), proto.StepSpec{
		Name: "use", Image: "alpine", Run: []string{"sh", "-c", "echo $VERSION"},
	}, func(string) {}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if !fc.inEnv("VERSION=1.2.3") {
		t.Errorf("exported var not propagated to the next step: %v", fc.env)
	}
	if !fc.called(":/miabi/env") || !fc.called("-e MIABI_ENV=/miabi/env") {
		t.Errorf("$MIABI_ENV file not mounted into the step: %v", fc.calls)
	}
}

// A step with no run command runs the image's own entrypoint/CMD — no override.
func TestContainerStepNoRunKeepsEntrypoint(t *testing.T) {
	fc := &fakeCommander{}
	e := newTestExecutor(t, fc)
	run, _ := e.Begin(context.Background(), proto.JobSpec{}, func(string) {})
	defer run.Close()
	if _, err := run.Step(context.Background(), proto.StepSpec{
		Name: "smoke", Image: "ghcr.io/acme/smoke:latest",
	}, func(string) {}); err != nil {
		t.Fatalf("container step: %v", err)
	}
	for _, c := range fc.calls {
		if strings.Contains(c, "--entrypoint") {
			t.Errorf("no run command should leave the entrypoint untouched: %v", fc.calls)
		}
	}
}

func TestDeployStepIsNoop(t *testing.T) {
	fc := &fakeCommander{}
	e := newTestExecutor(t, fc)
	run, _ := e.Begin(context.Background(), proto.JobSpec{}, func(string) {})
	defer run.Close()
	res, err := run.Step(context.Background(), proto.StepSpec{Uses: "deploy"}, func(string) {})
	if err != nil || res.Exit != 0 {
		t.Fatalf("deploy step should be a no-op success, got exit=%d err=%v", res.Exit, err)
	}
	if len(fc.calls) != 0 {
		t.Errorf("deploy must not run any command, got %v", fc.calls)
	}
}

// The reported bug: `dockerfile:` reached the runner but a custom path has to
// become `-f`, and the context has to stay independent of it — a monorepo keeps
// docker/Dockerfile while still building from the root.
func TestBuildStepDockerfileAndContext(t *testing.T) {
	cases := []struct {
		name  string
		build *proto.BuildConfig
		want  string
	}{
		{
			"defaults are unchanged",
			nil,
			"docker build -t reg.example.com/ws-42/web:9 .",
		},
		{
			"custom dockerfile, default context",
			&proto.BuildConfig{Method: "dockerfile", Dockerfile: "docker/Dockerfile"},
			"docker build -t reg.example.com/ws-42/web:9 -f docker/Dockerfile .",
		},
		{
			"custom context, default dockerfile",
			&proto.BuildConfig{Method: "dockerfile", Context: "services/api"},
			"docker build -t reg.example.com/ws-42/web:9 services/api",
		},
		{
			"both, independently",
			&proto.BuildConfig{Method: "dockerfile", Dockerfile: "docker/Dockerfile", Context: "services/api"},
			"docker build -t reg.example.com/ws-42/web:9 -f docker/Dockerfile services/api",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeCommander{digestOut: "reg.example.com/ws-42/web@sha256:cafebabe"}
			e := newTestExecutor(t, fc)
			job := proto.JobSpec{RunID: 9, Repository: "reg.example.com/ws-42/web", Commit: "abcdef1234567890"}
			run, err := e.Begin(context.Background(), job, func(string) {})
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			defer run.Close()
			// The context directory must exist in the checked-out source.
			if tc.build != nil && tc.build.Context != "" {
				if err := os.MkdirAll(filepath.Join(run.(*dockerJobRun).workdir, tc.build.Context), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := run.Step(context.Background(),
				proto.StepSpec{Ordinal: 0, Name: "build", Uses: "build", Build: tc.build},
				func(string) {}); err != nil {
				t.Fatalf("build step: %v", err)
			}
			if !fc.called(tc.want) {
				t.Errorf("build command wrong:\n got %v\n want %q", fc.calls, tc.want)
			}
		})
	}
}

// A context is joined against the checked-out source, so an absolute path or a
// climbing one would hand the build the runner's own filesystem. The runner is
// shared across a workspace's pipelines and a pipeline file is editable by anyone
// who can push a branch, so it re-checks rather than trusting the control plane.
func TestBuildStepRejectsEscapingContext(t *testing.T) {
	for _, bad := range []string{"/etc", "../../etc", "sub/../../.."} {
		t.Run(bad, func(t *testing.T) {
			fc := &fakeCommander{digestOut: "reg.example.com/ws-42/web@sha256:cafebabe"}
			e := newTestExecutor(t, fc)
			job := proto.JobSpec{RunID: 10, Repository: "reg.example.com/ws-42/web", Commit: "abcdef1234567890"}
			run, err := e.Begin(context.Background(), job, func(string) {})
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			defer run.Close()
			_, err = run.Step(context.Background(), proto.StepSpec{
				Ordinal: 0, Name: "build", Uses: "build",
				Build: &proto.BuildConfig{Method: "dockerfile", Context: bad},
			}, func(string) {})
			if err == nil {
				t.Fatalf("accepted context %q — the build would read outside the repository", bad)
			}
			for _, c := range fc.calls {
				if strings.HasPrefix(c, "docker build") {
					t.Errorf("ran a build despite the bad context: %q", c)
				}
			}
		})
	}
}

// Build args must be deterministic (sorted) so the argv is testable, and must sit
// before the positional context — docker reads the context as the last argument.
func TestBuildStepBuildArgs(t *testing.T) {
	fc := &fakeCommander{digestOut: "reg.example.com/ws-42/web@sha256:cafebabe"}
	e := newTestExecutor(t, fc)
	job := proto.JobSpec{RunID: 11, Repository: "reg.example.com/ws-42/web", Commit: "abcdef1234567890"}
	run, err := e.Begin(context.Background(), job, func(string) {})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer run.Close()

	if _, err := run.Step(context.Background(), proto.StepSpec{
		Ordinal: 0, Name: "build", Uses: "build",
		Build: &proto.BuildConfig{
			Method:     "dockerfile",
			Dockerfile: "docker/Dockerfile",
			BuildArgs:  map[string]string{"VERSION": "1.2.3", "APP_ENV": "prod"},
		},
	}, func(string) {}); err != nil {
		t.Fatalf("build step: %v", err)
	}

	want := "docker build -t reg.example.com/ws-42/web:11 -f docker/Dockerfile " +
		"--build-arg APP_ENV=prod --build-arg VERSION=1.2.3 ."
	if !fc.called(want) {
		t.Errorf("build command wrong:\n got %v\n want %q", fc.calls, want)
	}
}

// A resolved workspace secret must not reach the command line: argv is readable
// by every local user on the runner host via ps.
func TestContainerStepKeepsEnvValuesOutOfArgv(t *testing.T) {
	f := &fakeCommander{}
	e := &dockerExecutor{cmd: f, docker: "docker", git: "git", workRoot: t.TempDir()}
	job := proto.JobSpec{RunID: 1, Env: []string{"MIABI_REGISTRY_TOKEN=reg_s3cret"}}
	run, err := e.Begin(context.Background(), job, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	step := proto.StepSpec{
		Ordinal: 0, Name: "test", Image: "node:22",
		Run: []string{"sh", "-c", "npm test"},
		Env: []string{"NPM_TOKEN=npm_live_SECRET", "CI=true"},
	}
	if _, err := run.Step(context.Background(), step, func(string) {}); err != nil {
		t.Fatal(err)
	}

	argv := strings.Join(f.calls, " ")
	for _, secret := range []string{"npm_live_SECRET", "reg_s3cret"} {
		if strings.Contains(argv, secret) {
			t.Errorf("secret %q appeared in the command line: %s", secret, argv)
		}
	}
	// The names are still passed, so docker forwards them from our environment.
	for _, name := range []string{"-e NPM_TOKEN", "-e CI", "-e MIABI_REGISTRY_TOKEN"} {
		if !strings.Contains(argv, name) {
			t.Errorf("argv is missing %q: %s", name, argv)
		}
	}
	child := strings.Join(f.env, " ")
	for _, want := range []string{"NPM_TOKEN=npm_live_SECRET", "CI=true", "MIABI_REGISTRY_TOKEN=reg_s3cret"} {
		if !strings.Contains(child, want) {
			t.Errorf("child env is missing %q: %v", want, f.env)
		}
	}
}

// Step env wins over the job's on a collision, matching the control plane's
// documented precedence.
func TestContainerStepEnvPrecedence(t *testing.T) {
	f := &fakeCommander{}
	e := &dockerExecutor{cmd: f, docker: "docker", git: "git", workRoot: t.TempDir()}
	job := proto.JobSpec{RunID: 1, Env: []string{"SHARED=pipeline"}}
	run, _ := e.Begin(context.Background(), job, func(string) {})
	defer run.Close()

	step := proto.StepSpec{Ordinal: 0, Name: "s", Image: "x", Env: []string{"SHARED=step"}}
	if _, err := run.Step(context.Background(), step, func(string) {}); err != nil {
		t.Fatal(err)
	}
	var last string
	for _, e := range f.env {
		if strings.HasPrefix(e, "SHARED=") {
			last = e
		}
	}
	if last != "SHARED=step" {
		t.Errorf("effective SHARED = %q, want the step's value", last)
	}
}
