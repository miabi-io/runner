/*
 * Copyright 2026 Jonas Kaninda
 * SPDX-License-Identifier: Apache-2.0
 */

package proto

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestJobRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := JobSpec{
		RunID: 57, RunNumber: 3, Pipeline: "deploy", WorkspaceID: 42, Workspace: "acme",
		Commit: "abc123", Repository: "registry.example.com/ws-42/web",
		Steps:    []StepSpec{{Ordinal: 0, Name: "build", Uses: "build"}, {Ordinal: 1, Name: "deploy", Uses: "deploy"}},
		Deadline: time.Unix(1_900_000_000, 0).UTC(),
	}
	if err := WriteJob(&buf, want); err != nil {
		t.Fatalf("WriteJob: %v", err)
	}
	got, err := ReadJob(&buf)
	if err != nil {
		t.Fatalf("ReadJob: %v", err)
	}
	if got.RunID != want.RunID || got.Pipeline != want.Pipeline || len(got.Steps) != 2 || !got.Deadline.Equal(want.Deadline) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestFrameWriter(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	_ = fw.Step(0, "running")
	_ = fw.Log(0, "compiling…")
	_ = fw.Result(0, "sha256:deadbeef", 0)
	_ = fw.Done("succeeded")

	dec := json.NewDecoder(&buf)
	var frames []Frame
	for dec.More() {
		var f Frame
		if err := dec.Decode(&f); err != nil {
			t.Fatalf("decode: %v", err)
		}
		frames = append(frames, f)
	}
	if len(frames) != 4 {
		t.Fatalf("got %d frames, want 4", len(frames))
	}
	if frames[0].Type != FrameStep || frames[0].Status != "running" {
		t.Errorf("frame 0 = %+v", frames[0])
	}
	if frames[1].Type != FrameLog || frames[1].Line != "compiling…" {
		t.Errorf("frame 1 = %+v", frames[1])
	}
	if frames[2].Type != FrameResult || frames[2].Digest != "sha256:deadbeef" {
		t.Errorf("frame 2 = %+v", frames[2])
	}
	if frames[3].Type != FrameDone || frames[3].Status != "succeeded" {
		t.Errorf("frame 3 = %+v", frames[3])
	}
}
