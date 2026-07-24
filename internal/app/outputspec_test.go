package app

import (
	"strings"
	"testing"
)

func TestParseOutputSpecsWithAlgorithm(t *testing.T) {
	specs, err := parseOutputSpecs([]string{"subtitle:srt:algorithm=netflix", "subtitle:vtt", "script:txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 3 {
		t.Fatalf("expected 3 specs, got %d", len(specs))
	}
	if specs[0].Algorithm != "netflix" {
		t.Fatalf("first spec should carry the netflix algorithm: %+v", specs[0])
	}
	if specs[1].Algorithm != "" || specs[2].Algorithm != "" {
		t.Fatalf("other specs should not carry an algorithm: %+v", specs)
	}
}

func TestParseOutputSpecsRejectsUnknownAlgorithm(t *testing.T) {
	_, err := parseOutputSpecs([]string{"subtitle:srt:algorithm=nonsense"})
	if err == nil || !strings.Contains(err.Error(), "unknown subtitle algorithm") {
		t.Fatalf("expected unknown algorithm error, got %v", err)
	}
}

func TestParseOutputSpecsRejectsAlgorithmOnNonSubtitleOutputs(t *testing.T) {
	_, err := parseOutputSpecs([]string{"script:txt:algorithm=netflix"})
	if err == nil || !strings.Contains(err.Error(), "only applies to subtitle outputs") {
		t.Fatalf("expected non-subtitle algorithm error, got %v", err)
	}
}

func TestParseOutputSpecsRejectsMalformedOptions(t *testing.T) {
	for _, value := range []string{"subtitle:srt:algorithm", "subtitle:srt:algorithm=", "subtitle:srt:frames=24"} {
		if _, err := parseOutputSpecs([]string{value}); err == nil {
			t.Fatalf("expected error for %q", value)
		}
	}
}
