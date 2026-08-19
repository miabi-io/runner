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

// The buildkit backend spells a cold build `buildctl build --no-cache`, and
// leaves the cache in place when the step did not ask for one.
func TestBuildkitNoCache(t *testing.T) {
	for _, tc := range []struct {
		name    string
		build   *proto.BuildConfig
		wantArg bool
	}{
		{"cached by default", nil, false},
		{"no-cache requested", &proto.BuildConfig{Method: "dockerfile", NoCache: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeCommander{}
			e := newTestBuildkit(t, fc)
			job := proto.JobSpec{RunID: 6, Repository: "reg.example.com/ws_42/app-1", Commit: "abcdef1234567890"}
			run, err := e.Begin(context.Background(), job, func(string) {})
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			defer run.Close()
			if _, err := run.Step(context.Background(), proto.StepSpec{Uses: "build", Build: tc.build}, func(string) {}); err != nil {
				t.Fatalf("build: %v", err)
			}
			if got := fc.called("--no-cache"); got != tc.wantArg {
				t.Errorf("--no-cache present = %v, want %v: %v", got, tc.wantArg, fc.calls)
			}
		})
	}
}

// Registry cache is what makes a bumped cache generation actually cold and the next build warm
// again: imports are dropped for a cold build, but the export never is — it repopulates the ref.
func TestBuildkitRegistryCache(t *testing.T) {
	for _, tc := range []struct {
		name       string
		build      *proto.BuildConfig
		wantImport []string
		noImport   []string
		wantExport string
	}{
		{
			"imports and exports",
			&proto.BuildConfig{Method: "dockerfile", CacheFrom: []string{"reg/app:cache-feat-g1", "reg/app:cache-main-g1"}, CacheTo: "reg/app:cache-feat-g1"},
			[]string{"type=registry,ref=reg/app:cache-feat-g1", "type=registry,ref=reg/app:cache-main-g1"},
			nil,
			"type=registry,ref=reg/app:cache-feat-g1,mode=max",
		},
		{
			"cold build exports but imports nothing",
			&proto.BuildConfig{Method: "dockerfile", NoCache: true, CacheFrom: []string{"reg/app:cache-main-g2"}, CacheTo: "reg/app:cache-main-g2"},
			nil,
			[]string{"--import-cache"},
			"type=registry,ref=reg/app:cache-main-g2,mode=max",
		},
		{
			"no refs, no flags",
			&proto.BuildConfig{Method: "dockerfile"},
			nil,
			[]string{"--import-cache", "--export-cache"},
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeCommander{}
			e := newTestBuildkit(t, fc)
			job := proto.JobSpec{RunID: 6, Repository: "reg.example.com/ws_42/app-1", Commit: "abcdef1234567890"}
			run, err := e.Begin(context.Background(), job, func(string) {})
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			defer run.Close()
			if _, err := run.Step(context.Background(), proto.StepSpec{Uses: "build", Build: tc.build}, func(string) {}); err != nil {
				t.Fatalf("build: %v", err)
			}
			for _, want := range tc.wantImport {
				if !fc.called(want) {
					t.Errorf("missing import %q: %v", want, fc.calls)
				}
			}
			for _, bad := range tc.noImport {
				if fc.called(bad) {
					t.Errorf("unexpected %q: %v", bad, fc.calls)
				}
			}
			if tc.wantExport != "" && !fc.called(tc.wantExport) {
				t.Errorf("missing export %q: %v", tc.wantExport, fc.calls)
			}
		})
	}
}
