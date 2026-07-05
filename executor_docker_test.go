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
}

func (f *fakeCommander) run(_ context.Context, _ string, log func(string), name string, args ...string) (int, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	log(name + " output")
	// Unwrap an `env KEY=VAL … <bin> <sub> …` prefix (per-job DOCKER_CONFIG /
	// DOCKER_BUILDKIT) to the real command, so exit-code scripting still matches.
	bin, sub := name, ""
	if len(args) > 0 {
		sub = args[0]
	}
	if name == "env" {
		i := 0
		for i < len(args) && strings.Contains(args[i], "=") {
			i++
		}
		if i < len(args) {
			bin = args[i]
			if i+1 < len(args) {
				sub = args[i+1]
			}
		}
	}
	if bin == "docker" {
		switch sub {
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
	wantBuild := "env DOCKER_BUILDKIT=1 DOCKER_CONFIG=" + dr.cfgDir + " docker build -t reg.example.com/ws-42/web:8 ."
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
	_, err := run.Step(context.Background(), proto.StepSpec{
		Name: "test", Image: "golang:1.25", Env: []string{"CI=true"}, Run: []string{"go", "test", "./..."},
	}, func(string) {})
	if err != nil {
		t.Fatalf("container step: %v", err)
	}
	if !fc.called("docker run --rm -w /workspace -v") || !fc.called("golang:1.25 go test ./...") {
		t.Errorf("container run command wrong: %v", fc.calls)
	}
	if !fc.called("-e FOO=bar") || !fc.called("-e CI=true") {
		t.Errorf("job + step env not injected: %v", fc.calls)
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
