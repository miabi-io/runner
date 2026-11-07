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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/jkaninda/logger"
	"github.com/miabi-io/runner/proto"
)

// Used when a buildpack build supplies no builder; overridable via MIABI_RUNNER_DEFAULT_BUILDER.
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

// No build config keeps Dockerfile behavior; "auto" picks Dockerfile if one is at the root, else buildpacks.
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

// Re-checks escapes here: pipeline files are editable by anyone who can push, so don't trust the control plane alone.
func contextDir(workdir string, cfg *proto.BuildConfig) (string, error) {
	if cfg == nil || strings.TrimSpace(cfg.Context) == "" {
		return workdir, nil
	}
	rel := strings.TrimSpace(cfg.Context)
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("build context %q must be relative to the repository root", rel)
	}
	abs := filepath.Join(workdir, rel)
	// Join cleans the result, so a contained path keeps workdir as its prefix.
	if abs != workdir && !strings.HasPrefix(abs, workdir+string(filepath.Separator)) {
		return "", fmt.Errorf("build context %q escapes the repository", rel)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("build context %q not found in the repository", rel)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("build context %q is not a directory", rel)
	}
	return abs, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// buildctl values need the "build-arg:" prefix, supplied by the caller.
func buildArgFlags(cfg *proto.BuildConfig, flag, prefix string) []string {
	if cfg == nil || len(cfg.BuildArgs) == 0 {
		return nil
	}
	out := make([]string, 0, len(cfg.BuildArgs)*2)
	for _, k := range sortedKeys(cfg.BuildArgs) {
		out = append(out, flag, prefix+k+"="+cfg.BuildArgs[k])
	}
	return out
}

func noCache(cfg *proto.BuildConfig) bool {
	return cfg != nil && cfg.NoCache
}

func cacheNote(cfg *proto.BuildConfig) string {
	if noCache(cfg) {
		return ", no cache"
	}
	return ""
}

// Imports are skipped for a cold build; the export is not, so the next build reads the rebuilt layers.
func cacheFlags(cfg *proto.BuildConfig) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	if !cfg.NoCache {
		for _, ref := range cfg.CacheFrom {
			if strings.TrimSpace(ref) != "" {
				out = append(out, "--import-cache", "type=registry,ref="+ref)
			}
		}
	}
	// mode=max caches intermediate stages too, the expensive half of a multi-stage build.
	if strings.TrimSpace(cfg.CacheTo) != "" {
		out = append(out, "--export-cache", "type=registry,ref="+cfg.CacheTo+",mode=max")
	}
	return out
}

// platforms is the build's target platforms, blanks dropped; empty builds for the runner's own platform.
func platforms(cfg *proto.BuildConfig) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	for _, p := range cfg.Platforms {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func platformNote(cfg *proto.BuildConfig) string {
	if ps := platforms(cfg); len(ps) > 0 {
		return ", platforms " + strings.Join(ps, ",")
	}
	return ""
}

// binfmtDir is where the kernel lists the interpreters it runs foreign binaries through. A var for tests.
var binfmtDir = "/proc/sys/fs/binfmt_misc"

// qemuArch maps a platform's architecture to the name QEMU registers its binfmt handler under.
var qemuArch = map[string]string{
	"amd64": "x86_64", "arm64": "aarch64", "arm": "arm", "386": "i386",
	"ppc64le": "ppc64le", "s390x": "s390x", "riscv64": "riscv64",
}

// A build still succeeds when the Dockerfile cross-compiles on $BUILDPLATFORM, so this warns rather than refuses.
func missingEmulators(ps []string, native string) []string {
	var out []string
	for _, p := range ps {
		parts := strings.Split(p, "/")
		if len(parts) < 2 || parts[1] == native {
			continue
		}
		name, ok := qemuArch[parts[1]]
		if !ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(binfmtDir, "qemu-"+name)); err != nil {
			out = append(out, p)
		}
	}
	return out
}

// warnEmulators logs which platforms lack an emulator, and how to add one.
func warnEmulators(cfg *proto.BuildConfig, log func(string)) {
	if missing := missingEmulators(platforms(cfg), runtime.GOARCH); len(missing) > 0 {
		log("warning: no QEMU emulator registered on this host for " + strings.Join(missing, ", ") +
			"; RUN instructions for them will fail unless the Dockerfile cross-compiles on $BUILDPLATFORM. " +
			"Register emulators on the host with: docker run --privileged --rm tonistiigi/binfmt --install all")
	}
}

// For a multi-platform build this is the index digest, which a node of any listed platform deploys by.
func readImageDigest(metaFile string) (string, error) {
	b, err := os.ReadFile(metaFile)
	if err != nil {
		return "", err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return "", err
	}
	if d, ok := m["containerimage.digest"].(string); ok && d != "" {
		return d, nil
	}
	return "", fmt.Errorf("no containerimage.digest in build metadata")
}

// buildxCacheFlags is cacheFlags in docker buildx's spelling.
func buildxCacheFlags(cfg *proto.BuildConfig) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	if !cfg.NoCache {
		for _, ref := range cfg.CacheFrom {
			if strings.TrimSpace(ref) != "" {
				out = append(out, "--cache-from", "type=registry,ref="+ref)
			}
		}
	}
	if strings.TrimSpace(cfg.CacheTo) != "" {
		out = append(out, "--cache-to", "type=registry,ref="+cfg.CacheTo+",mode=max")
	}
	return out
}

// contextLabel renders a context directory for the build log
func contextLabel(workdir, dir string) string {
	rel, err := filepath.Rel(workdir, dir)
	if err != nil || rel == "" {
		return "."
	}
	return rel
}

// hasFile reports whether dir contains a regular file named name.
func hasFile(dir, name string) bool {
	info, err := os.Stat(filepath.Join(dir, name))
	return err == nil && !info.IsDir()
}

func packArgs(tag, builder string, cfg *proto.BuildConfig) []string {
	args := []string{
		"build", tag,
		"--path", ".",
		"--builder", builder,
		"--docker-host", "inherit",
		"--trust-builder",
		"--pull-policy", "if-not-present",
	}
	if cfg == nil {
		return args
	}

	if cfg.NoCache {
		args = append(args, "--clear-cache")
	}
	for _, bp := range cfg.Buildpacks {
		if strings.TrimSpace(bp) != "" {
			args = append(args, "--buildpack", bp)
		}
	}
	for _, k := range sortedKeys(cfg.BuildEnv) {
		args = append(args, "--env", k+"="+cfg.BuildEnv[k])
	}
	return args
}

type commander interface {
	// A non-zero exit is (code, nil); a failure to start is (-1, err).
	run(ctx context.Context, dir string, env []string, log func(string), name string, args ...string) (int, error)
	// capture runs a command and returns its trimmed stdout.
	capture(ctx context.Context, dir, name string, args ...string) (string, error)
	// loginStdin runs a command with secret piped to its stdin (registry login).
	loginStdin(ctx context.Context, secret, name string, args ...string) error
}

// The source URL is never logged: it may embed a credential.
func gitCheckout(ctx context.Context, cmd commander, gitBin, workdir string, job proto.JobSpec, log func(string)) error {
	log("cloning source")
	if code, err := cmd.run(ctx, "", nil, log, gitBin, "clone", job.SourceURL, workdir); err != nil || code != 0 {
		return fmt.Errorf("git clone failed (exit %d): %w", code, err)
	}
	if job.Commit != "" {
		if code, err := cmd.run(ctx, workdir, nil, log, gitBin, "checkout", "--detach", job.Commit); err != nil || code != 0 {
			return fmt.Errorf("git checkout %s failed (exit %d): %w", job.Commit, code, err)
		}
	}
	return nil
}

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

func (execCommander) run(ctx context.Context, dir string, env []string, log func(string), name string, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
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
