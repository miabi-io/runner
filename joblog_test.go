/*
 * Copyright 2026 Jonas Kaninda
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jkaninda/logger"
	"github.com/miabi-io/runner/proto"
)

func TestShortCommit(t *testing.T) {
	if got := shortCommit("1c5d4c8cbcedad8209fe49d1bcd9731aec63ec78"); got != "1c5d4c8c" {
		t.Errorf("got %q", got)
	}
	if got := shortCommit("abc"); got != "abc" {
		t.Errorf("a short sha must survive intact, got %q", got)
	}
	if got := shortCommit(""); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestTookIsReadable(t *testing.T) {
	tests := map[time.Duration]string{
		1500 * time.Microsecond:               "2ms",
		2*time.Second + 3*time.Millisecond:    "2s",
		83*time.Second + 478*time.Millisecond: "1m23s",
	}
	for d, want := range tests {
		if got := took(d); got != want {
			t.Errorf("took(%v) = %q, want %q", d, got, want)
		}
	}
}

// jobFields is the base every line appends to. If `with` handed back a slice
// sharing its backing array, two concurrent jobs would overwrite each other's
// fields — and the corruption would only show under load.
func TestWithDoesNotAliasTheBase(t *testing.T) {
	base := jobFields(proto.JobSpec{RunID: 1, Pipeline: "ci", Workspace: "acme"})
	a := with(base, "step", 1)
	b := with(base, "step", 2)
	if a[len(a)-1] == b[len(b)-1] {
		t.Fatal("the two field sets share storage")
	}
	if len(base) != len(a)-2 {
		t.Errorf("base was mutated: len %d", len(base))
	}
}

func TestJobFieldsOmitsSecrets(t *testing.T) {
	job := proto.JobSpec{
		RunID: 7, Pipeline: "ci", Workspace: "acme",
		SourceURL: "https://oauth2:ghp_SECRET@github.com/acme/app.git",
		Env:       []string{"REGISTRY_PASSWORD=hunter2", "MIABI_APP=web"},
	}
	rendered := strings.Join(fieldsToStrings(jobFields(job)), " ")
	for _, leak := range []string{"ghp_SECRET", "hunter2", "REGISTRY_PASSWORD"} {
		if strings.Contains(rendered, leak) {
			t.Errorf("job fields leaked %q: %s", leak, rendered)
		}
	}
}

func fieldsToStrings(f []any) []string {
	out := make([]string, 0, len(f))
	for _, v := range f {
		out = append(out, strings.TrimSpace(strings.Join(strings.Fields(sprint(v)), " ")))
	}
	return out
}

func sprint(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		return ""
	}
}

// captureLogs points the global logger at a file for the duration of one test
// and returns everything written to it.
func captureLogs(t *testing.T, run func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.log")
	logger.New(logger.WithOutputFile(path), logger.WithJSONFormat(), logger.WithDebugLevel())
	// Leave the global logger as the rest of the suite expects to find it.
	defer logger.New(logger.WithJSONFormat(), logger.WithInfoLevel())

	run()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(b)
}

func loggedJob() proto.JobSpec {
	return proto.JobSpec{
		RunID: 42, RunNumber: 7, Pipeline: "ci", Workspace: "acme", App: "web",
		Commit:    "1c5d4c8cbcedad8209fe49d1bcd9731aec63ec78",
		Branch:    "main",
		SourceURL: "https://oauth2:ghp_SECRET@github.com/acme/app.git",
		Env:       []string{"REGISTRY_PASSWORD=hunter2"},
		Steps: []proto.StepSpec{
			{Ordinal: 0, Name: "test", Uses: "run"},
			{Ordinal: 1, Name: "build", Uses: "build"},
		},
	}
}

func TestSuccessfulJobLogsItsLifecycle(t *testing.T) {
	out := captureLogs(t, func() {
		drive(t, loggedJob(), fakeExecutor{results: map[int]StepResult{}})
	})
	for _, want := range []string{"job started", "step succeeded", "job completed", `"run":42`, `"pipeline":"ci"`, `"took"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log is missing %q\n---\n%s", want, out)
		}
	}
}

func TestFailedJobNamesTheStep(t *testing.T) {
	job := loggedJob()
	out := captureLogs(t, func() {
		drive(t, job, fakeExecutor{results: map[int]StepResult{1: {Exit: 2}}})
	})
	for _, want := range []string{"step failed", "job failed", `"failed_step":1`, `"exit":2`} {
		if !strings.Contains(out, want) {
			t.Errorf("log is missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "job completed") {
		t.Error("a failed job must not also report completion")
	}
}

// A run that finished green after a tolerated failure is not the same thing as a
// clean run, and the log should not imply it was.
func TestToleratedFailureIsReported(t *testing.T) {
	job := loggedJob()
	job.Steps[0].ContinueOnError = true
	out := captureLogs(t, func() {
		drive(t, job, fakeExecutor{results: map[int]StepResult{0: {Exit: 1}}})
	})
	for _, want := range []string{"step failed", "job completed", `"tolerated_failures":1`} {
		if !strings.Contains(out, want) {
			t.Errorf("log is missing %q\n---\n%s", want, out)
		}
	}
}

// The whole point of the field discipline: nothing a job carries as a credential
// may reach the runner's log.
func TestJobLogsCarryNoCredentials(t *testing.T) {
	out := captureLogs(t, func() {
		drive(t, loggedJob(), fakeExecutor{results: map[int]StepResult{1: {Exit: 2}}})
	})
	for _, leak := range []string{"ghp_SECRET", "hunter2", "oauth2:"} {
		if strings.Contains(out, leak) {
			t.Errorf("the log leaked %q\n---\n%s", leak, out)
		}
	}
	// …while still saying where the code came from.
	if !strings.Contains(out, "github.com/acme/app.git") {
		t.Errorf("the source host was stripped along with the credential\n---\n%s", out)
	}
}
