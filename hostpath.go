/*
 * Copyright 2026 Jonas Kaninda
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jkaninda/logger"
)

// hostMount is one of the runner container's mounts, as `docker inspect` reports it.
type hostMount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
}

// hostPathResolver translates a path inside the runner to the path the docker
// daemon sees. A containerised runner drives the host daemon through its socket,
// so a `-v` source is resolved on the host, not in the runner's filesystem.
type hostPathResolver struct {
	cmd    commander
	docker string
	selfID func() string // the runner's own container ID, "" when not containerised

	once   sync.Once
	mounts []hostMount // nil: paths are passed through unchanged
}

func newHostPathResolver(cmd commander, docker string) *hostPathResolver {
	return &hostPathResolver{cmd: cmd, docker: docker, selfID: selfContainerID}
}

// resolve returns path as the docker daemon sees it, or an error when path lives
// in the runner container's own filesystem, where no step container can mount it.
func (h *hostPathResolver) resolve(path string) (string, error) {
	h.once.Do(h.load)
	if h.mounts == nil {
		return path, nil
	}
	return translateHostPath(h.mounts, path)
}

func (h *hostPathResolver) load() {
	id := h.selfID()
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := h.cmd.capture(ctx, "", h.docker, "inspect", "--format", "{{json .Mounts}}", id)
	if err != nil {
		// The daemon does not know this container (remote DOCKER_HOST, a dind
		// sidecar), so its paths may well match ours: pass them through.
		logger.Warn("cannot inspect the runner container; passing workspace paths to docker unchanged", "container", id, "error", err)
		return
	}
	var mounts []hostMount
	if err := json.Unmarshal([]byte(out), &mounts); err != nil {
		logger.Warn("cannot parse the runner container's mounts; passing workspace paths to docker unchanged", "error", err)
		return
	}
	h.mounts = make([]hostMount, 0, len(mounts))
	for _, m := range mounts {
		if m.Source != "" && m.Type != "tmpfs" {
			h.mounts = append(h.mounts, m)
		}
	}
}

// translateHostPath maps path through the mount whose destination is its longest prefix.
func translateHostPath(mounts []hostMount, path string) (string, error) {
	path = filepath.Clean(path)
	best := -1
	for i, m := range mounts {
		dest := filepath.Clean(m.Destination)
		if path != dest && !strings.HasPrefix(path, strings.TrimSuffix(dest, "/")+"/") {
			continue
		}
		if best < 0 || len(dest) > len(filepath.Clean(mounts[best].Destination)) {
			best = i
		}
	}
	if best < 0 {
		return "", fmt.Errorf("job workspace %s is inside the runner container, so step containers cannot mount it: "+
			"mount a host directory and point MIABI_RUNNER_BUILDS_DIR at it, e.g. "+
			"-v /srv/miabi/builds:/srv/miabi/builds -e MIABI_RUNNER_BUILDS_DIR=/srv/miabi/builds", path)
	}
	m := mounts[best]
	rel := strings.TrimPrefix(path, filepath.Clean(m.Destination))
	return filepath.Clean(m.Source) + rel, nil
}

// selfContainerID returns the runner's container ID, or "" outside a container.
func selfContainerID() string {
	if !fileExists("/.dockerenv") && !fileExists("/run/.containerenv") {
		return ""
	}
	if f, err := os.Open("/proc/self/mountinfo"); err == nil {
		defer f.Close()
		if id := containerIDFromMountinfo(f); id != "" {
			return id
		}
	}
	// Docker names the container's host after its short ID unless --hostname is set.
	host, _ := os.Hostname()
	return host
}

var containerIDPattern = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// containerIDFromMountinfo finds the container ID in the paths of the
// hostname/hosts/resolv.conf files docker bind-mounts into every container.
func containerIDFromMountinfo(r io.Reader) string {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if m := containerIDPattern.FindStringSubmatch(sc.Text()); m != nil {
			return m[1]
		}
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
