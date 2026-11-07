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
	"net"
	"net/http"
	"runtime"

	"github.com/hashicorp/yamux"
	"github.com/jkaninda/logger"
	"github.com/jkaninda/wstunnel"
	"github.com/miabi-io/runner/proto"
)

// Distinct scope from the node agent's /api/v1/agent/connect.
const connectPath = "/api/v1/runner/connect"

// Config configures the runner runtime.
type Config struct {
	ControlURL string // e.g. https://miabi.example.com
	Token      string // registration token (mbr_...)
	Insecure   bool   // skip TLS verification of the control plane
	Version    string // runner build version, reported to the control plane
	Builder    string // build backend: "docker" (default) or "buildkit" (rootless)
}

// Run stays registered until ctx is cancelled, reconnecting with exponential backoff.
func Run(ctx context.Context, cfg Config) error {
	var exec Executor
	if cfg.Builder == "buildkit" {
		exec = newBuildkitExecutor()
	} else {
		docker := newDockerExecutor()
		docker.checkHostPaths()
		exec = docker
	}
	opts := wstunnel.ClientOptions{
		URL:      wstunnel.URL(cfg.ControlURL, connectPath),
		Header:   authHeader(cfg),
		Insecure: cfg.Insecure,
		OnConnect: func() {
			logger.Info("connected to control plane", "control_url", cfg.ControlURL)
		},
		OnError: func(err error) {
			logger.Warn("runner disconnected", "error", err)
		},
	}

	return wstunnel.Serve(ctx, opts, func(connCtx context.Context, sess *yamux.Session) error {
		for {
			stream, err := sess.AcceptStream()
			if err != nil {
				return err
			}
			go serveJob(connCtx, stream, exec)
		}
	})
}

func serveJob(ctx context.Context, stream net.Conn, exec Executor) {
	defer func() { _ = stream.Close() }()
	_ = runJob(ctx, stream, exec)
}

// The control plane records these platform facts and uses them for label/arch scheduling.
func authHeader(cfg Config) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+cfg.Token)
	h.Set("X-Runner-OS", runtime.GOOS)
	h.Set("X-Runner-Arch", runtime.GOARCH)
	h.Set("X-Runner-Version", cfg.Version)
	h.Set(proto.HeaderFeatures, proto.FeatureMultiPlatform)
	return h
}
