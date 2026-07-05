/*
 * Copyright 2026 Jonas Kaninda
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jkaninda/logger"
	"github.com/miabi-io/runner/proto"
)

// defaultBuilder is the CNB builder image used when a buildpack build supplies
// none (the control plane normally resolves and sends one). Overridable via
// MIABI_RUNNER_DEFAULT_BUILDER.
const defaultBuilder = "paketobuildpacks/builder-jammy-base"

// buildsDir is the parent directory for per-job workspaces
func buildsDir() string {
	dir := strings.TrimSpace(os.Getenv("MIABI_RUNNER_BUILDS_DIR"))
	if dir == "" {
		return os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logger.Warn("MIABI_RUNNER_BUILDS_DIR unusable, falling back to temp dir", "dir", dir, "error", err)
		return os.TempDir()
	}
	return dir
}

// resolveBuildMethod decides how a build step builds. No build config keeps the
// historical Dockerfile behavior; an explicit method is honored; "auto"/"" (with
// a config present) inspects the tree — a root Dockerfile selects Dockerfile,
// otherwise Cloud Native Buildpacks.
func resolveBuildMethod(dir string, cfg *proto.BuildConfig) string {
	if cfg == nil {
		return "dockerfile"
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Method)) {
	case "dockerfile":
		return "dockerfile"
	case "buildpack":
		return "buildpack"
	default: // "auto" or ""
		if hasFile(dir, dockerfilePath(cfg)) {
			return "dockerfile"
		}
		return "buildpack"
	}
}

// dockerfilePath is the configured Dockerfile name, defaulting to "Dockerfile".
func dockerfilePath(cfg *proto.BuildConfig) string {
	if cfg != nil && strings.TrimSpace(cfg.Dockerfile) != "" {
		return cfg.Dockerfile
	}
	return "Dockerfile"
}

// hasFile reports whether dir contains a regular file named name.
func hasFile(dir, name string) bool {
	info, err := os.Stat(filepath.Join(dir, name))
	return err == nil && !info.IsDir()
}

// packArgs assembles the `pack build` argv for a buildpack build. Env keys are
// sorted for a deterministic, testable argv.
func packArgs(tag, builder string, cfg *proto.BuildConfig) []string {
	args := []string{
		"build", tag,
		"--path", ".",
		"--builder", builder,
		// inherit: use the runner's Docker host; trust-builder: skip the prompt for
		// a known builder; if-not-present: reuse a cached builder image.
		"--docker-host", "inherit",
		"--trust-builder",
		"--pull-policy", "if-not-present",
	}
	if cfg == nil {
		return args
	}
	for _, bp := range cfg.Buildpacks {
		if strings.TrimSpace(bp) != "" {
			args = append(args, "--buildpack", bp)
		}
	}
	keys := make([]string, 0, len(cfg.BuildEnv))
	for k := range cfg.BuildEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env", k+"="+cfg.BuildEnv[k])
	}
	return args
}

// commander runs external commands; abstracted so the executors are unit-testable
// without a real docker/buildkit/git.
type commander interface {
	// run executes name+args in dir, streaming combined output to log line by
	// line, and returns the process exit code. A non-zero exit is (code, nil); a
	// failure to start is (-1, err).
	run(ctx context.Context, dir string, log func(string), name string, args ...string) (int, error)
	// capture runs a command and returns its trimmed stdout.
	capture(ctx context.Context, dir, name string, args ...string) (string, error)
	// loginStdin runs a command with secret piped to its stdin (registry login).
	loginStdin(ctx context.Context, secret, name string, args ...string) error
}

// gitCheckout clones a job's source and checks out its commit into workdir. The
// source URL is never logged (it may embed a credential).
func gitCheckout(ctx context.Context, cmd commander, gitBin, workdir string, job proto.JobSpec, log func(string)) error {
	log("cloning source")
	if code, err := cmd.run(ctx, "", log, gitBin, "clone", job.SourceURL, workdir); err != nil || code != 0 {
		return fmt.Errorf("git clone failed (exit %d): %w", code, err)
	}
	if job.Commit != "" {
		if code, err := cmd.run(ctx, workdir, log, gitBin, "checkout", "--detach", job.Commit); err != nil || code != 0 {
			return fmt.Errorf("git checkout %s failed (exit %d): %w", job.Commit, code, err)
		}
	}
	return nil
}

// writeDockerConfig writes a docker config.json into dir with a registry login,
// so daemonless builders (buildkit) authenticate their push with no login step.
// Returns the DOCKER_CONFIG dir. A blank token writes nothing (anonymous).
func writeDockerConfig(dir, registry, user, token string) error {
	if token == "" || registry == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
	cfg := fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, registry, auth)
	return os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600)
}

// envMap indexes a KEY=VALUE slice.
func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			m[k] = v
		}
	}
	return m
}

// execCommander is the real commander over os/exec.
type execCommander struct{}

func (execCommander) run(ctx context.Context, dir string, log func(string), name string, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	w := &lineWriter{emit: log}
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	w.flush()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil // ran, exited non-zero — the step handles it
		}
		return -1, err // failed to start
	}
	return 0, nil
}

func (execCommander) capture(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func (execCommander) loginStdin(ctx context.Context, secret, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(secret)
	return cmd.Run()
}

// lineWriter splits streamed output into lines and emits each via a callback.
type lineWriter struct {
	emit func(string)
	buf  []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.emit(string(w.buf))
		w.buf = nil
	}
}
