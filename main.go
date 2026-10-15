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

// Command miabi-runner is the machine-side runtime for Miabi's dedicated build &
// pipeline execution. It dials the control plane over an outbound WebSocket
// (NAT-friendly, no inbound ports) authenticated by its registration token, and
// appears online. Unlike the node agent it exposes no Docker socket to the
// control plane — a runner uses its OWN local Docker/BuildKit daemon; the tunnel
// exists so the control plane can lease build jobs to it and stream logs/status
// back. Job leasing lands in a later phase; this phase registers and heartbeats.
//
// Configuration (environment):
//
//	MIABI_CONTROL_URL                  control plane base URL, e.g. https://miabi.example.com
//	                                       (falls back to MIABI_API_URL)
//	MIABI_RUNNER_TOKEN                 registration token issued when the runner was added (mbr_...)
//	MIABI_RUNNER_INSECURE_SKIP_VERIFY skip TLS verification of the control plane (default false)
//	MIABI_RUNNER_BUILDER              build backend: "docker" (default) or "buildkit" (rootless)
//	MIABI_RUNNER_BUILDS_DIR           parent dir for per-job workspaces, default: OS temp dir.
package main

import (
	"context"
	"os/signal"
	"strings"
	"syscall"

	goutils "github.com/jkaninda/go-utils"
	"github.com/jkaninda/logger"
)

// version is set at build time: -ldflags "-X main.version=1.0.0".
var version = "dev"

func main() {
	controlURL := strings.TrimRight(goutils.Env("MIABI_CONTROL_URL", goutils.Env("MIABI_API_URL", "")), "/")
	token := goutils.Env("MIABI_RUNNER_TOKEN", "")
	insecure := goutils.EnvBool("MIABI_RUNNER_INSECURE_SKIP_VERIFY", false)
	if goutils.EnvBool("MIABI_DEV_MODE", false) {
		logger.New(logger.WithDebugLevel())
	} else {
		logger.New(logger.WithJSONFormat(), logger.WithInfoLevel())
	}

	cfg := Config{
		ControlURL: controlURL,
		Token:      token,
		Insecure:   insecure,
		Version:    version,
		Builder:    goutils.Env("MIABI_RUNNER_BUILDER", "docker"),
	}
	if cfg.ControlURL == "" || cfg.Token == "" {
		logger.Fatal("MIABI_CONTROL_URL and MIABI_RUNNER_TOKEN are required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("miabi-runner starting", "version", version, "control_url", cfg.ControlURL)
	if err := Run(ctx, cfg); err != nil && err != context.Canceled {
		logger.Fatal("runner error", "error", err)
	}
	logger.Info("runner stopped")
}
