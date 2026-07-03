/*
 * Copyright 2026 Jonas Kaninda
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"runtime"
	"testing"
)

func TestAuthHeader(t *testing.T) {
	h := authHeader(Config{Token: "mbr_secret", Version: "1.2.3"})
	if got := h.Get("Authorization"); got != "Bearer mbr_secret" {
		t.Errorf("Authorization = %q, want bearer token", got)
	}
	if got := h.Get("X-Runner-Version"); got != "1.2.3" {
		t.Errorf("X-Runner-Version = %q, want 1.2.3", got)
	}
	if got := h.Get("X-Runner-Os"); got != runtime.GOOS {
		t.Errorf("X-Runner-OS = %q, want %q", got, runtime.GOOS)
	}
	if got := h.Get("X-Runner-Arch"); got != runtime.GOARCH {
		t.Errorf("X-Runner-Arch = %q, want %q", got, runtime.GOARCH)
	}
}
