/*
 * Copyright 2026 Jonas Kaninda
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
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
	return &dockerExecutor{cmd: cmd, docker: "docker", git: "git", workRoot: t.TempDir()}
}

func TestBeginCheckoutAndLogin(t *testing.T) {
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
	if fc.logins != 1 {
		t.Errorf("expected one registry login, got %d", fc.logins)
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
	// Tag derives from the short commit; build then push then inspect.
	if !fc.called("docker build -t reg.example.com/ws-42/web:abcdef123456 .") {
		t.Errorf("build command wrong: %v", fc.calls)
	}
	if !fc.called("docker push reg.example.com/ws-42/web:abcdef123456") {
		t.Errorf("push command missing: %v", fc.calls)
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
