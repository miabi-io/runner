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
	"testing"

	"github.com/miabi-io/runner/proto"
)

func TestBuildTag(t *testing.T) {
	cases := []struct {
		name string
		job  proto.JobSpec
		want string
	}{
		{"pipeline run uses run-<number>", proto.JobSpec{RunNumber: 2, RunID: 90, Commit: "abcdef1234567890"}, "run-2"},
		{"deploy build uses the deploy id (RunID)", proto.JobSpec{RunID: 139, Commit: "abcdef1234567890"}, "139"},
		{"no identity falls back to latest", proto.JobSpec{Commit: "abcdef1234567890"}, "latest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildTag(tc.job); got != tc.want {
				t.Errorf("buildTag(%+v) = %q, want %q", tc.job, got, tc.want)
			}
		})
	}
}
