/*
 * Copyright 2026 Jonas Kaninda
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miabi-io/runner/proto"
)

var twoPlatforms = &proto.BuildConfig{Method: "dockerfile", Platforms: []string{"linux/amd64", " linux/arm64 "}}

func beginDocker(t *testing.T, fc *fakeCommander) JobRun {
	t.Helper()
	e := newTestExecutor(t, fc)
	e.buildxConfig = filepath.Join(t.TempDir(), "buildx")
	e.readDigest = func(string) (string, error) { return "sha256:1dea", nil }
	run, err := e.Begin(context.Background(), proto.JobSpec{RunID: 6, Repository: "reg.example.com/ws-42/web"}, func(string) {})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(run.Close)
	return run
}

// A daemon without the containerd image store cannot build for several platforms, so the runner builds with
// a docker-container buildx builder it creates once, and pushes as part of the build.
func TestDockerMultiPlatformCreatesABuilder(t *testing.T) {
	fc := &fakeCommander{answer: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "docker info"):
			return `[["Backing Filesystem","extfs"]]`, nil
		case strings.Contains(cmd, "buildx inspect"):
			return "", errors.New("no builder")
		}
		return "", nil
	}}
	run := beginDocker(t, fc)
	res, err := run.Step(context.Background(), proto.StepSpec{Uses: "build", Build: twoPlatforms}, func(string) {})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if res.Digest != "sha256:1dea" {
		t.Errorf("digest = %q, want the index digest from the metadata", res.Digest)
	}
	if !fc.called("docker buildx create --name miabi-runner --driver docker-container") {
		t.Errorf("builder not created: %v", fc.calls)
	}
	if !fc.called("buildx build --builder miabi-runner --platform linux/amd64,linux/arm64 -t reg.example.com/ws-42/web:6 --push") {
		t.Errorf("buildx build wrong: %v", fc.calls)
	}
	if fc.called("docker push") || fc.called("docker build -t") {
		t.Errorf("a multi-platform build must not fall back to build + push: %v", fc.calls)
	}
	if !fc.called("BUILDX_CONFIG=") {
		t.Errorf("buildx state is not kept outside the per-job DOCKER_CONFIG: %v", fc.calls)
	}
}

// A daemon on the containerd image store builds for several platforms itself, with its own registry settings.
func TestDockerMultiPlatformUsesTheDaemonWhenItCan(t *testing.T) {
	fc := &fakeCommander{answer: func(cmd string) (string, error) {
		if strings.Contains(cmd, "docker info") {
			return `[["driver-type","io.containerd.snapshotter.v1"]]`, nil
		}
		return "", nil
	}}
	run := beginDocker(t, fc)
	if _, err := run.Step(context.Background(), proto.StepSpec{Uses: "build", Build: twoPlatforms}, func(string) {}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if fc.called("--builder") || fc.called("buildx create") {
		t.Errorf("the daemon's own builder should be used: %v", fc.calls)
	}
	if !fc.called("buildx build --platform linux/amd64,linux/arm64") {
		t.Errorf("buildx build wrong: %v", fc.calls)
	}
}

func TestBuildpackRefusesPlatforms(t *testing.T) {
	run := beginDocker(t, &fakeCommander{})
	cfg := &proto.BuildConfig{Method: "buildpack", Platforms: []string{"linux/arm64"}}
	if _, err := run.Step(context.Background(), proto.StepSpec{Uses: "build", Build: cfg}, func(string) {}); err == nil ||
		!strings.Contains(err.Error(), "own platform only") {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

func TestBuildkitPlatforms(t *testing.T) {
	fc := &fakeCommander{}
	e := newTestBuildkit(t, fc)
	run, err := e.Begin(context.Background(), proto.JobSpec{RunID: 6, Repository: "reg.example.com/ws/app"}, func(string) {})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer run.Close()
	if _, err := run.Step(context.Background(), proto.StepSpec{Uses: "build", Build: twoPlatforms}, func(string) {}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if !fc.called("--opt platform=linux/amd64,linux/arm64") {
		t.Errorf("platforms not passed to buildctl: %v", fc.calls)
	}
}

func TestMissingEmulators(t *testing.T) {
	dir := t.TempDir()
	orig := binfmtDir
	binfmtDir = dir
	t.Cleanup(func() { binfmtDir = orig })
	if err := os.WriteFile(filepath.Join(dir, "qemu-aarch64"), []byte("enabled"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := missingEmulators([]string{"linux/amd64", "linux/arm64", "linux/riscv64", "linux/arm/v7"}, "amd64")
	if want := []string{"linux/riscv64", "linux/arm/v7"}; !slices.Equal(got, want) {
		t.Errorf("missing = %v, want %v (native and registered platforms need nothing)", got, want)
	}
}

func TestRunnerAdvertisesMultiPlatform(t *testing.T) {
	if got := authHeader(Config{Token: "t"}).Get(proto.HeaderFeatures); !strings.Contains(got, proto.FeatureMultiPlatform) {
		t.Errorf("%s = %q, want %s", proto.HeaderFeatures, got, proto.FeatureMultiPlatform)
	}
}
