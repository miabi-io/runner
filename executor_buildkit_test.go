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

func newTestBuildkit(t *testing.T, cmd commander) *buildkitExecutor {
	t.Helper()
	return &buildkitExecutor{
		cmd: cmd, buildctl: "buildctl-daemonless.sh", git: "git", workRoot: t.TempDir(),
		readDigest: func(string) (string, error) { return "sha256:cafebabe", nil },
	}
}

// A build runs buildctl with a push output + metadata file, authenticated via a
// per-job DOCKER_CONFIG, and returns the digest read from the metadata.
func TestBuildkitBuildPushesByDigest(t *testing.T) {
	fc := &fakeCommander{}
	e := newTestBuildkit(t, fc)
	job := proto.JobSpec{
		RunID:      6,
		Repository: "reg.example.com/ws_42/app-1",
		Commit:     "abcdef1234567890",
		Env:        []string{"MIABI_REGISTRY=reg.example.com", "MIABI_REGISTRY_USER=miabi-job", "MIABI_REGISTRY_TOKEN=mb_secret"},
	}
	run, err := e.Begin(context.Background(), job, func(string) {})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer run.Close()

	res, err := run.Step(context.Background(), proto.StepSpec{Uses: "build"}, func(string) {})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if res.Digest != "sha256:cafebabe" {
		t.Errorf("digest = %q", res.Digest)
	}
	// buildctl invoked with a push output, the deploy-id (RunID) tag, and the
	// docker config env for auth.
	if !fc.called("buildctl-daemonless.sh build") {
		t.Errorf("buildctl not invoked: %v", fc.calls)
	}
	if !fc.called("type=image,name=reg.example.com/ws_42/app-1:6,push=true") {
		t.Errorf("push output ref wrong: %v", fc.calls)
	}
	if !fc.called("DOCKER_CONFIG=") {
		t.Errorf("DOCKER_CONFIG auth not passed: %v", fc.calls)
	}
	// The registry credential was written to config.json for the push.
	bkr := run.(*buildkitJobRun)
	if _, err := os.Stat(filepath.Join(bkr.cfgDir, "config.json")); err != nil {
		t.Errorf("registry config.json not written: %v", err)
	}
	// …and it lives OUTSIDE the build context (workdir is sent as --local context),
	// so a Dockerfile can't COPY the token out.
	if strings.HasPrefix(bkr.cfgDir, bkr.workdir) {
		t.Errorf("auth dir %q must not be inside the build context %q", bkr.cfgDir, bkr.workdir)
	}
}

// Container steps are unsupported on the buildkit backend (build-only).
func TestBuildkitRejectsContainerStep(t *testing.T) {
	e := newTestBuildkit(t, &fakeCommander{})
	run, _ := e.Begin(context.Background(), proto.JobSpec{}, func(string) {})
	defer run.Close()
	_, err := run.Step(context.Background(), proto.StepSpec{Name: "test", Image: "golang", Uses: ""}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "container") {
		t.Errorf("want container-unsupported error, got %v", err)
	}
}

func TestReadImageDigest(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "meta.json")
	_ = os.WriteFile(f, []byte(`{"containerimage.digest":"sha256:deadbeef","image.name":"x"}`), 0o600)
	got, err := readImageDigest(f)
	if err != nil || got != "sha256:deadbeef" {
		t.Fatalf("readImageDigest = %q, %v", got, err)
	}
	if _, err := readImageDigest(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("want error for missing metadata file")
	}
}
