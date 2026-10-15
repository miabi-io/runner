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
	"path/filepath"
	"strings"
	"testing"

	"github.com/miabi-io/runner/proto"
)

func TestTranslateHostPath(t *testing.T) {
	mounts := []hostMount{
		{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"},
		{Type: "bind", Source: "/mnt/ssd1/builds", Destination: "/mnt/ssd1/builds"},
		{Type: "bind", Source: "/data/ci", Destination: "/builds"},
		{Type: "volume", Source: "/var/lib/docker/volumes/cache/_data", Destination: "/builds/cache"},
	}
	for _, tc := range []struct{ path, want string }{
		{"/mnt/ssd1/builds/miabi-run-1-abc", "/mnt/ssd1/builds/miabi-run-1-abc"},
		{"/builds/miabi-run-2-def", "/data/ci/miabi-run-2-def"},
		{"/builds", "/data/ci"},
		{"/builds/cache/x", "/var/lib/docker/volumes/cache/_data/x"},
	} {
		got, err := translateHostPath(mounts, tc.path)
		if err != nil || got != tc.want {
			t.Errorf("translateHostPath(%q) = %q, %v; want %q", tc.path, got, err, tc.want)
		}
	}

	for _, path := range []string{"/tmp/miabi-run-3-ghi", "/buildsx/miabi-run-4"} {
		if _, err := translateHostPath(mounts, path); err == nil || !strings.Contains(err.Error(), "MIABI_RUNNER_BUILDS_DIR") {
			t.Errorf("translateHostPath(%q) error = %v, want a MIABI_RUNNER_BUILDS_DIR hint", path, err)
		}
	}
}

func TestContainerIDFromMountinfo(t *testing.T) {
	const id = "4f1c0f6d9a2b7e3c5d8a1b0c9e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9f"
	info := "1290 1270 0:52 / / rw,relatime - overlay overlay rw\n" +
		"1301 1290 8:1 /var/lib/docker/containers/" + id + "/hostname /etc/hostname rw - ext4 /dev/sda1 rw\n"
	if got := containerIDFromMountinfo(strings.NewReader(info)); got != id {
		t.Errorf("containerIDFromMountinfo = %q, want %q", got, id)
	}
	if got := containerIDFromMountinfo(strings.NewReader("1 0 0:1 / / rw - ext4 /dev/sda1 rw\n")); got != "" {
		t.Errorf("containerIDFromMountinfo on a host = %q, want empty", got)
	}
}

func newTestResolver(f *fakeCommander, id string) *hostPathResolver {
	return &hostPathResolver{cmd: f, docker: "docker", selfID: func() string { return id }}
}

func TestHostPathResolverOutsideContainer(t *testing.T) {
	f := &fakeCommander{}
	h := newTestResolver(f, "")
	if got, err := h.resolve("/tmp/miabi-run-1"); err != nil || got != "/tmp/miabi-run-1" {
		t.Errorf("resolve = %q, %v; want the path unchanged", got, err)
	}
	if f.called("inspect") {
		t.Errorf("inspected a container outside one: %v", f.calls)
	}
}

func TestHostPathResolverUninspectableContainer(t *testing.T) {
	f := &fakeCommander{answer: func(string) (string, error) { return "", errors.New("No such object") }}
	h := newTestResolver(f, "abc")
	if got, err := h.resolve("/tmp/miabi-run-1"); err != nil || got != "/tmp/miabi-run-1" {
		t.Errorf("resolve = %q, %v; want the path unchanged", got, err)
	}
}

// A runner enrolled without a builds-dir mount used to run container steps in an
// empty /workspace; the step must fail instead, naming the fix.
func TestContainerStepWorkspaceOutsideMounts(t *testing.T) {
	f := &fakeCommander{answer: func(string) (string, error) {
		return `[{"Type":"bind","Source":"/var/run/docker.sock","Destination":"/var/run/docker.sock"}]`, nil
	}}
	e := newTestExecutor(t, f)
	e.hostPaths = newTestResolver(f, "abc")
	run, err := e.Begin(context.Background(), proto.JobSpec{RunID: 1}, func(string) {})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer run.Close()

	_, err = run.Step(context.Background(), proto.StepSpec{Name: "test", Image: "maven", Run: []string{"sh", "-c", "cd spring-boot"}}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "MIABI_RUNNER_BUILDS_DIR") {
		t.Fatalf("Step error = %v, want a MIABI_RUNNER_BUILDS_DIR hint", err)
	}
	if f.called("docker run") {
		t.Errorf("ran the step container anyway: %v", f.calls)
	}
}

func TestContainerStepMountsHostPaths(t *testing.T) {
	f := &fakeCommander{}
	e := newTestExecutor(t, f)
	f.answer = func(string) (string, error) {
		return `[{"Type":"bind","Source":"/srv/ci","Destination":"` + e.workRoot + `"}]`, nil
	}
	e.hostPaths = newTestResolver(f, "abc")
	run, err := e.Begin(context.Background(), proto.JobSpec{RunID: 1}, func(string) {})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer run.Close()
	r := run.(*dockerJobRun)

	if _, err := run.Step(context.Background(), proto.StepSpec{Name: "test", Image: "maven", Run: []string{"sh", "-c", "true"}}, func(string) {}); err != nil {
		t.Fatalf("Step: %v", err)
	}
	workspace := filepath.Join("/srv/ci", filepath.Base(r.workdir)) + ":/workspace"
	envFile := filepath.Join("/srv/ci", filepath.Base(r.envFile)) + ":/miabi/env"
	if !f.called("-v "+workspace) || !f.called("-v "+envFile) {
		t.Errorf("step mounts not translated to host paths %q, %q: %v", workspace, envFile, f.calls)
	}
}
