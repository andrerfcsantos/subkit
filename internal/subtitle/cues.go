package subtitle

import (
	"fmt"
	"strings"

	"github.com/andrerfcsantos/subkit-codex/internal/transcript"
)

const CueSchemaVersion = "subkit.cues.v1"

const (
	// AlgorithmDeepgram mirrors the deepgram-go-captions reference: cues are
	// fixed-size word chunks taken from utterances (or the flat word list),
	// with no character, duration, or gap shaping.
	AlgorithmDeepgram = "deepgram"
	// AlgorithmNetflix follows the Netflix Timed Text Style Guide: 42-char
	// lines, sentence-aware segmentation, reading-speed timing, and gap
	// chaining.
	AlgorithmNetflix = "netflix"
)

// Algorithms lists the valid cue algorithm names.
func Algorithms() []string {
	return []string{AlgorithmDeepgram, AlgorithmNetflix}
}

// NormalizeAlgorithm lowercases and validates an algorithm name. An empty name
// selects the default deepgram algorithm.
func NormalizeAlgorithm(name string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	switch normalized {
	case "":
		return AlgorithmDeepgram, nil
	case AlgorithmDeepgram, AlgorithmNetflix:
		return normalized, nil
	}
	return "", fmt.Errorf("unknown subtitle algorithm %q (valid: %s)", name, strings.Join(Algorithms(), ", "))
}

// CueOptions tunes cue generation. Numeric fields left at zero use the
// selected algorithm's own defaults, so each algorithm can honor its
// specification without one shared set of numbers.
type CueOptions struct {
	Algorithm       string  `json:"algorithm"`
	MaxCharsPerLine int     `json:"max_chars_per_line,omitempty"`
	MaxLines        int     `json:"max_lines,omitempty"`
	MaxWordsPerLine int     `json:"max_words_per_line,omitempty"`
	MinDuration     float64 `json:"min_duration,omitempty"`
	MaxDuration     float64 `json:"max_duration,omitempty"`
	MaxGap          float64 `json:"max_gap,omitempty"`
	ReadingSpeed    float64 `json:"reading_speed,omitempty"`
	PreferSegments  bool    `json:"prefer_segments"`
}

func DefaultCueOptions() CueOptions {
	return CueOptions{
		Algorithm:      AlgorithmDeepgram,
		PreferSegments: true,
	}
}

type CueSet struct {
	SchemaVersion string     `json:"schema_version"`
	SourceSchema  string     `json:"source_schema"`
	Options       CueOptions `json:"options"`
	Cues          []Cue      `json:"cues"`
}

type Cue struct {
	Index   int     `json:"index"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Text    string  `json:"text"`
	Speaker *int    `json:"speaker,omitempty"`
	Words   []int   `json:"words,omitempty"`
}

func BuildCues(t transcript.Transcript, opts CueOptions) (CueSet, error) {
	algorithm, err := NormalizeAlgorithm(opts.Algorithm)
	if err != nil {
		return CueSet{}, err
	}
	opts.Algorithm = algorithm

	var cues []Cue
	switch algorithm {
	case AlgorithmNetflix:
		cues = netflixCues(t, opts)
	default:
		cues = deepgramCues(t, opts)
	}
	for i := range cues {
		cues[i].Index = i + 1
	}

	return CueSet{
		SchemaVersion: CueSchemaVersion,
		SourceSchema:  t.SchemaVersion,
		Options:       opts,
		Cues:          cues,
	}, nil
}

// wordRun is a contiguous stretch of transcript content an algorithm cues
// independently. Runs with Words came from an utterance segment (or the flat
// word list); runs with only a Segment carry text without word timings.
type wordRun struct {
	Words   []transcript.Word
	Segment *transcript.Segment
	// FromSegment marks runs cut from an utterance segment, as opposed to the
	// flat word-list fallback.
	FromSegment bool
}

// collectRuns gathers the word runs to cue. When preferSegments is set and
// the transcript has utterance segments, each utterance is its own run;
// otherwise the whole word list forms a single run.
func collectRuns(t transcript.Transcript, preferSegments bool) []wordRun {
	var runs []wordRun
	if preferSegments {
		for i := range t.Segments {
			segment := &t.Segments[i]
			if segment.Type != "utterance" {
				continue
			}
			if segment.WordEnd > segment.WordStart && segment.WordEnd <= len(t.Words) {
				runs = append(runs, wordRun{Words: t.Words[segment.WordStart:segment.WordEnd], FromSegment: true})
				continue
			}
			if strings.TrimSpace(segment.Text) != "" && segment.End > segment.Start {
				runs = append(runs, wordRun{Segment: segment, FromSegment: true})
			}
		}
	}
	if len(runs) == 0 && len(t.Words) > 0 {
		runs = []wordRun{{Words: t.Words}}
	}
	return runs
}

func wordsText(words []transcript.Word) string {
	parts := make([]string, 0, len(words))
	for _, word := range words {
		parts = append(parts, word.DisplayText())
	}
	return strings.Join(parts, " ")
}

func wordIndexes(words []transcript.Word) []int {
	indexes := make([]int, 0, len(words))
	for _, word := range words {
		indexes = append(indexes, word.Index)
	}
	return indexes
}

func collapseSpaces(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func speakerValue(speaker *int) int {
	if speaker == nil {
		return -1
	}
	return *speaker
}
