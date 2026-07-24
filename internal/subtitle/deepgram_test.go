package subtitle

import (
	"fmt"
	"strings"
	"testing"

	"github.com/andrerfcsantos/subkit-codex/internal/transcript"
)

// makeWords builds n sequential words, each 0.2s long starting at start.
func makeWords(start float64, texts ...string) []transcript.Word {
	words := make([]transcript.Word, 0, len(texts))
	for i, text := range texts {
		words = append(words, transcript.Word{
			Index:      i,
			Text:       strings.ToLower(strings.Trim(text, ".,!?")),
			Punctuated: text,
			Start:      start + float64(i)*0.2,
			End:        start + float64(i)*0.2 + 0.2,
		})
	}
	return words
}

func numberedWords(n int) []transcript.Word {
	texts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		texts = append(texts, fmt.Sprintf("word%d", i))
	}
	return makeWords(0, texts...)
}

func TestDeepgramChunksUtteranceIntoEightWordLines(t *testing.T) {
	words := numberedWords(10)
	tr := transcript.Transcript{
		SchemaVersion: transcript.SchemaVersion,
		Words:         words,
		Segments: []transcript.Segment{
			{Type: "utterance", Start: words[0].Start, End: words[9].End, WordStart: 0, WordEnd: 10},
		},
	}

	cues, err := BuildCues(tr, DefaultCueOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(cues.Cues) != 2 {
		t.Fatalf("expected 2 cues (8+2 words), got %d: %+v", len(cues.Cues), cues.Cues)
	}
	if got := len(strings.Fields(cues.Cues[0].Text)); got != 8 {
		t.Fatalf("first cue should have 8 words, got %d: %q", got, cues.Cues[0].Text)
	}
	if got := len(strings.Fields(cues.Cues[1].Text)); got != 2 {
		t.Fatalf("second cue should have 2 words, got %d: %q", got, cues.Cues[1].Text)
	}
	// Timing must be exactly first word start to last word end: the reference
	// implementation applies no minimum duration.
	if cues.Cues[1].Start != words[8].Start || cues.Cues[1].End != words[9].End {
		t.Fatalf("cue timing should match word timing exactly: %+v", cues.Cues[1])
	}
	if strings.Contains(cues.Cues[0].Text, "\n") {
		t.Fatalf("deepgram cues should be single-line: %q", cues.Cues[0].Text)
	}
}

func TestDeepgramShortUtteranceIsSingleCue(t *testing.T) {
	words := numberedWords(5)
	tr := transcript.Transcript{
		Words: words,
		Segments: []transcript.Segment{
			{Type: "utterance", Start: words[0].Start, End: words[4].End, WordStart: 0, WordEnd: 5},
		},
	}

	cues, err := BuildCues(tr, DefaultCueOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(cues.Cues) != 1 {
		t.Fatalf("expected a single cue, got %d", len(cues.Cues))
	}
}

func TestDeepgramWordPathSplitsOnSpeakerChange(t *testing.T) {
	words := numberedWords(6)
	speakerA, speakerB := 0, 1
	for i := range words {
		if i < 4 {
			words[i].Speaker = &speakerA
		} else {
			words[i].Speaker = &speakerB
		}
	}
	tr := transcript.Transcript{Words: words}

	cues, err := BuildCues(tr, DefaultCueOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(cues.Cues) != 2 {
		t.Fatalf("expected split on speaker change, got %d cues", len(cues.Cues))
	}
	if speakerValue(cues.Cues[0].Speaker) != speakerA || speakerValue(cues.Cues[1].Speaker) != speakerB {
		t.Fatalf("cue speakers wrong: %+v", cues.Cues)
	}
}

func TestDeepgramWordPathChunksWithoutDiarization(t *testing.T) {
	tr := transcript.Transcript{Words: numberedWords(17)}

	cues, err := BuildCues(tr, DefaultCueOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(cues.Cues) != 3 {
		t.Fatalf("expected 3 cues (8+8+1), got %d", len(cues.Cues))
	}
	if got := len(strings.Fields(cues.Cues[2].Text)); got != 1 {
		t.Fatalf("last cue should have 1 word, got %d", got)
	}
}

func TestDeepgramHonorsMaxWordsPerLine(t *testing.T) {
	tr := transcript.Transcript{Words: numberedWords(6)}
	opts := DefaultCueOptions()
	opts.MaxWordsPerLine = 3

	cues, err := BuildCues(tr, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(cues.Cues) != 2 {
		t.Fatalf("expected 2 cues of 3 words, got %d", len(cues.Cues))
	}
}

func TestBuildCuesRejectsUnknownAlgorithm(t *testing.T) {
	opts := DefaultCueOptions()
	opts.Algorithm = "readable"
	if _, err := BuildCues(transcript.Transcript{}, opts); err == nil {
		t.Fatal("expected error for unknown algorithm")
	}
}

func TestNormalizeAlgorithmDefaultsToDeepgram(t *testing.T) {
	got, err := NormalizeAlgorithm("")
	if err != nil {
		t.Fatal(err)
	}
	if got != AlgorithmDeepgram {
		t.Fatalf("empty algorithm should default to deepgram, got %q", got)
	}
	if _, err := NormalizeAlgorithm(" NETFLIX "); err != nil {
		t.Fatalf("algorithm names should be case and space insensitive: %v", err)
	}
}
