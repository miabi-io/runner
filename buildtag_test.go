/*
 * Copyright 2026 Jonas Kaninda
 * SPDX-License-Identifier: Apache-2.0
 */

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
