package subtitle

import (
	"strings"
	"testing"
)

func TestRenderSRT(t *testing.T) {
	speaker := 2
	cues := CueSet{
		SchemaVersion: CueSchemaVersion,
		Cues: []Cue{
			{Start: 1.234, End: 3.456, Text: "Hello there.", Speaker: &speaker},
		},
	}

	got := RenderSRT(cues)
	if !strings.Contains(got, "00:00:01,234 --> 00:00:03,456") {
		t.Fatalf("missing SRT timestamp: %q", got)
	}
	if !strings.Contains(got, "[speaker 2]\nHello there.") {
		t.Fatalf("missing speaker text: %q", got)
	}
}

func TestRenderWebVTT(t *testing.T) {
	speaker := 1
	cues := CueSet{
		SchemaVersion: CueSchemaVersion,
		Cues: []Cue{
			{Start: 61.2, End: 62.3, Text: "Hello there.", Speaker: &speaker},
		},
	}

	got := RenderWebVTT(cues)
	if !strings.HasPrefix(got, "WEBVTT\n\n") {
		t.Fatalf("missing VTT header: %q", got)
	}
	if !strings.Contains(got, "00:01:01.200 --> 00:01:02.300") {
		t.Fatalf("missing VTT timestamp: %q", got)
	}
	if !strings.Contains(got, "<v Speaker 1>Hello there.") {
		t.Fatalf("missing voice tag: %q", got)
	}
}
