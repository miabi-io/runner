/*
 * Copyright 2026 Jonas Kaninda
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

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
