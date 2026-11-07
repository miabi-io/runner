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

// A containerised runner drives the host daemon, so a -v source is resolved on the host.
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

// Errors when path lives in the runner container's own filesystem, where no step can mount it.
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
		// The daemon does not know this container (remote DOCKER_HOST, dind), so its paths may match ours.
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
