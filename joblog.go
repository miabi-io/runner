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
	"strings"
	"time"

	"github.com/miabi-io/runner/proto"
)

func jobFields(job proto.JobSpec) []any {
	f := []any{
		"run", job.RunID,
		"number", job.RunNumber,
		"pipeline", job.Pipeline,
		"workspace", job.Workspace,
		"steps", len(job.Steps),
	}
	if job.App != "" {
		f = append(f, "app", job.App)
	}
	if c := shortCommit(job.Commit); c != "" {
		f = append(f, "commit", c)
	}
	if job.Branch != "" {
		f = append(f, "branch", job.Branch)
	} else if job.Ref != "" {
		f = append(f, "ref", job.Ref)
	}
	return f
}

func jobRef(job proto.JobSpec) []any {
	return []any{"run", job.RunID, "number", job.RunNumber}
}

// stepFields identifies one step within its job.
func stepFields(step proto.StepSpec) []any {
	f := []any{"step", step.Ordinal}
	if step.Name != "" {
		f = append(f, "name", step.Name)
	}
	if step.Uses != "" {
		f = append(f, "uses", step.Uses)
	}
	return f
}

// shortCommit trims a full SHA to the 8 characters people actually read.
func shortCommit(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func took(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return d.Round(time.Second).String()
	case d >= time.Second:
		return d.Round(10 * time.Millisecond).String()
	default:
		return d.Round(time.Millisecond).String()
	}
}

func with(base []any, extra ...any) []any {
	out := make([]any, 0, len(base)+len(extra))
	out = append(out, base...)
	return append(out, extra...)
}
