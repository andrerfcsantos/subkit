package subtitle

import (
	"strings"
	"testing"

	"github.com/andrerfcsantos/subkit-codex/internal/transcript"
)

func netflixOptions() CueOptions {
	opts := DefaultCueOptions()
	opts.Algorithm = AlgorithmNetflix
	return opts
}

func buildNetflix(t *testing.T, tr transcript.Transcript) CueSet {
	t.Helper()
	cues, err := BuildCues(tr, netflixOptions())
	if err != nil {
		t.Fatal(err)
	}
	return cues
}

func TestNetflixRespectsLineAndCueLimits(t *testing.T) {
	// A long unpunctuated stretch has to be cut into events of at most two
	// 42-character lines.
	texts := strings.Fields(strings.Repeat("wonderful mountain scenery keeps appearing ", 6))
	tr := transcript.Transcript{Words: makeWords(0, texts...)}

	cues := buildNetflix(t, tr)
	if len(cues.Cues) < 2 {
		t.Fatalf("long text should produce multiple cues, got %d", len(cues.Cues))
	}
	for _, cue := range cues.Cues {
		lines := strings.Split(cue.Text, "\n")
		if len(lines) > 2 {
			t.Fatalf("cue has more than 2 lines: %q", cue.Text)
		}
		for _, line := range lines {
			if len([]rune(line)) > 42 {
				t.Fatalf("line longer than 42 chars: %q", line)
			}
		}
	}
}

func TestNetflixKeepsShortSentenceOnOneLine(t *testing.T) {
	tr := transcript.Transcript{Words: makeWords(0, "Keep", "this", "on", "one", "line.")}

	cues := buildNetflix(t, tr)
	if len(cues.Cues) != 1 {
		t.Fatalf("expected one cue, got %d", len(cues.Cues))
	}
	if strings.Contains(cues.Cues[0].Text, "\n") {
		t.Fatalf("text within the line limit must stay on a single line: %q", cues.Cues[0].Text)
	}
}

func TestNetflixMinimumDuration(t *testing.T) {
	words := []transcript.Word{{Index: 0, Text: "Hi.", Punctuated: "Hi.", Start: 1.0, End: 1.2}}
	tr := transcript.Transcript{Words: words}

	cues := buildNetflix(t, tr)
	if got := cues.Cues[0].End - cues.Cues[0].Start; got < netflixMinDuration {
		t.Fatalf("cue shorter than minimum duration: %v", got)
	}
}

func TestNetflixReadingSpeedExtendsOutTime(t *testing.T) {
	// 60+ characters spoken in one second need well over 3 seconds at
	// 17 characters per second.
	texts := strings.Fields("an extremely fast talker saying quite many characters quickly here")
	words := makeWords(0, texts...)
	for i := range words {
		words[i].Start = float64(i) * 0.1
		words[i].End = float64(i)*0.1 + 0.1
	}
	tr := transcript.Transcript{Words: words}

	cues := buildNetflix(t, tr)
	if len(cues.Cues) != 1 {
		t.Fatalf("expected one cue, got %d", len(cues.Cues))
	}
	cue := cues.Cues[0]
	chars := len([]rune(strings.ReplaceAll(cue.Text, "\n", " ")))
	minEnd := cue.Start + float64(chars)/netflixReadingSpeed
	if cue.End < minEnd-1e-9 {
		t.Fatalf("out-time %v does not satisfy reading speed (needs %v)", cue.End, minEnd)
	}
}

func TestNetflixMaximumDuration(t *testing.T) {
	words := makeWords(0, "One.", "Two.")
	// A pause under the gap threshold keeps both words in one block, but the
	// span stays under the 7 second cap.
	words[1].Start = 0.9
	words[1].End = 1.1
	tr := transcript.Transcript{Words: words}

	cues := buildNetflix(t, tr)
	for _, cue := range cues.Cues {
		if cue.End-cue.Start > netflixMaxDuration+1e-9 {
			t.Fatalf("cue exceeds max duration: %+v", cue)
		}
	}
}

func TestNetflixChainsSmallGaps(t *testing.T) {
	first := makeWords(0, "First", "sentence", "here.")
	second := makeWords(8, "Second", "sentence", "here.")
	words := append(append([]transcript.Word{}, first...), second...)
	tr := transcript.Transcript{Words: words}

	cues := buildNetflix(t, tr)
	if len(cues.Cues) != 2 {
		t.Fatalf("expected 2 cues, got %d", len(cues.Cues))
	}
	gap := cues.Cues[1].Start - cues.Cues[0].End
	if gap < netflixMinGap-1e-9 {
		t.Fatalf("gap smaller than 2 frames: %v", gap)
	}
	// The first cue's audio ends 7.4s before the next one starts, far beyond
	// the padded out-time, so the gap must be at least half a second.
	if gap < netflixChainGap && gap > netflixMinGap+1e-9 {
		t.Fatalf("gap between 2 frames and half a second should have been chained: %v", gap)
	}
}

func TestNetflixClosesSubHalfSecondGapsToTwoFrames(t *testing.T) {
	// A speaker change forces two cues whose audio sits 0.3s apart; the first
	// cue's padded out-time must stop exactly 2 frames before the next one.
	first := makeWords(0, "First", "sentence", "spoken", "now.")
	second := makeWords(1.1, "Second", "sentence", "spoken", "now.")
	speakerA, speakerB := 0, 1
	for i := range first {
		first[i].Speaker = &speakerA
	}
	for i := range second {
		second[i].Speaker = &speakerB
	}
	words := append(append([]transcript.Word{}, first...), second...)
	tr := transcript.Transcript{Words: words}

	cues := buildNetflix(t, tr)
	if len(cues.Cues) != 2 {
		t.Fatalf("expected 2 cues, got %d", len(cues.Cues))
	}
	gap := cues.Cues[1].Start - cues.Cues[0].End
	if diff := gap - netflixMinGap; diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("close gap should be chained to exactly 2 frames, got %v", gap)
	}
}

func TestNetflixSplitsOnSpeakerChange(t *testing.T) {
	words := makeWords(0, "Hello", "there", "friend.", "General", "Kenobi", "here.")
	speakerA, speakerB := 0, 1
	for i := range words {
		if i < 3 {
			words[i].Speaker = &speakerA
		} else {
			words[i].Speaker = &speakerB
		}
	}
	tr := transcript.Transcript{Words: words}

	cues := buildNetflix(t, tr)
	if len(cues.Cues) != 2 {
		t.Fatalf("expected split on speaker change, got %d cues", len(cues.Cues))
	}
}

func TestNetflixSplitsAtLongSilence(t *testing.T) {
	first := makeWords(0, "before", "the", "pause")
	second := makeWords(10, "after", "the", "pause")
	words := append(append([]transcript.Word{}, first...), second...)
	tr := transcript.Transcript{Words: words}

	cues := buildNetflix(t, tr)
	if len(cues.Cues) != 2 {
		t.Fatalf("expected silence to split cues, got %d", len(cues.Cues))
	}
	if cues.Cues[0].End >= cues.Cues[1].Start {
		t.Fatalf("cues overlap across the silence: %+v", cues.Cues)
	}
}

func TestNetflixPacksShortSentencesTogether(t *testing.T) {
	tr := transcript.Transcript{Words: makeWords(0, "Yes.", "I", "agree.")}

	cues := buildNetflix(t, tr)
	if len(cues.Cues) != 1 {
		t.Fatalf("short adjacent sentences should share a cue, got %d cues", len(cues.Cues))
	}
}

func TestNetflixPrefersBreakBeforeConjunction(t *testing.T) {
	tr := transcript.Transcript{
		Words: makeWords(0,
			"The", "team", "finished", "the", "project", "early",
			"because", "everyone", "worked", "together", "nicely."),
	}

	cues := buildNetflix(t, tr)
	if len(cues.Cues) != 1 {
		t.Fatalf("expected one cue, got %d", len(cues.Cues))
	}
	lines := strings.Split(cues.Cues[0].Text, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two lines, got %q", cues.Cues[0].Text)
	}
	if !strings.HasPrefix(lines[1], "because") {
		t.Fatalf("second line should start at the conjunction: %q", cues.Cues[0].Text)
	}
}

func TestNetflixUsesSegmentTextWithoutWordRange(t *testing.T) {
	tr := transcript.Transcript{
		Segments: []transcript.Segment{
			{Type: "utterance", Text: "Text only utterance without word timings.", Start: 0, End: 3},
		},
	}
	opts := netflixOptions()

	cues, err := BuildCues(tr, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(cues.Cues) != 1 {
		t.Fatalf("expected one cue from segment text, got %d", len(cues.Cues))
	}
	if cues.Cues[0].Words != nil {
		t.Fatalf("synthetic cues must not reference word indexes: %+v", cues.Cues[0])
	}
}
