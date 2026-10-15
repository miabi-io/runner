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
