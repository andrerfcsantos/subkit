package subtitle

import (
	"strings"

	"github.com/andrerfcsantos/subkit-codex/internal/transcript"
)

const CueSchemaVersion = "subkit.cues.v1"

type CueOptions struct {
	Algorithm       string  `json:"algorithm"`
	MaxCharsPerLine int     `json:"max_chars_per_line"`
	MaxLines        int     `json:"max_lines"`
	MinDuration     float64 `json:"min_duration"`
	MaxDuration     float64 `json:"max_duration"`
	MaxGap          float64 `json:"max_gap"`
	PreferSegments  bool    `json:"prefer_segments"`
}

func DefaultCueOptions() CueOptions {
	return CueOptions{
		Algorithm:       "readable",
		MaxCharsPerLine: 42,
		MaxLines:        2,
		MinDuration:     0.8,
		MaxDuration:     6.0,
		MaxGap:          0.9,
		PreferSegments:  true,
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

func BuildCues(t transcript.Transcript, opts CueOptions) CueSet {
	if opts.Algorithm == "" {
		opts.Algorithm = "readable"
	}
	if opts.MaxCharsPerLine <= 0 {
		opts.MaxCharsPerLine = 42
	}
	if opts.MaxLines <= 0 {
		opts.MaxLines = 2
	}
	if opts.MinDuration <= 0 {
		opts.MinDuration = 0.8
	}
	if opts.MaxDuration <= 0 {
		opts.MaxDuration = 6.0
	}
	if opts.MaxGap <= 0 {
		opts.MaxGap = 0.9
	}

	var cues []Cue
	if opts.PreferSegments {
		cues = cuesFromSegments(t, opts)
	}
	if len(cues) == 0 {
		cues = cuesFromWords(t.Words, opts)
	}
	for i := range cues {
		cues[i].Index = i + 1
	}

	return CueSet{
		SchemaVersion: CueSchemaVersion,
		SourceSchema:  t.SchemaVersion,
		Options:       opts,
		Cues:          cues,
	}
}

func cuesFromSegments(t transcript.Transcript, opts CueOptions) []Cue {
	var cues []Cue
	for _, segment := range t.Segments {
		if segment.Type != "utterance" {
			continue
		}
		if segment.WordEnd > segment.WordStart && segment.WordEnd <= len(t.Words) {
			cues = append(cues, cuesFromWords(t.Words[segment.WordStart:segment.WordEnd], opts)...)
			continue
		}
		if strings.TrimSpace(segment.Text) != "" && segment.End > segment.Start {
			cues = append(cues, Cue{
				Start:   segment.Start,
				End:     segment.End,
				Text:    wrapText(segment.Text, opts.MaxCharsPerLine, opts.MaxLines),
				Speaker: segment.Speaker,
			})
		}
	}
	return cues
}

func cuesFromWords(words []transcript.Word, opts CueOptions) []Cue {
	var cues []Cue
	var current []transcript.Word
	limit := opts.MaxCharsPerLine * opts.MaxLines

	flush := func() {
		if len(current) == 0 {
			return
		}
		first := current[0]
		last := current[len(current)-1]
		end := last.End
		if end-first.Start < opts.MinDuration {
			end = first.Start + opts.MinDuration
		}
		cues = append(cues, Cue{
			Start:   first.Start,
			End:     end,
			Text:    wrapText(wordsText(current), opts.MaxCharsPerLine, opts.MaxLines),
			Speaker: first.Speaker,
			Words:   wordIndexes(current),
		})
		current = nil
	}

	for _, word := range words {
		if word.Text == "" && word.Punctuated == "" {
			continue
		}
		if len(current) == 0 {
			current = append(current, word)
			continue
		}

		last := current[len(current)-1]
		nextTextLen := len(wordsText(current)) + 1 + len(word.DisplayText())
		speakerChanged := speakerValue(last.Speaker) != speakerValue(word.Speaker)
		gapTooLarge := word.Start-last.End > opts.MaxGap
		durationTooLong := word.End-current[0].Start > opts.MaxDuration
		lineTooLong := nextTextLen > limit
		sentenceBreak := endsSentence(last.DisplayText()) && nextTextLen > opts.MaxCharsPerLine

		if speakerChanged || gapTooLarge || durationTooLong || lineTooLong || sentenceBreak {
			flush()
		}
		current = append(current, word)
	}
	flush()

	return cues
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

func wrapText(text string, maxChars int, maxLines int) string {
	text = strings.Join(strings.Fields(text), " ")
	if maxChars <= 0 || maxLines <= 0 || len(text) <= maxChars {
		return text
	}

	words := strings.Fields(text)
	var lines []string
	var line []string
	for _, word := range words {
		next := word
		if len(line) > 0 {
			next = strings.Join(append(append([]string{}, line...), word), " ")
		}
		if len(next) > maxChars && len(line) > 0 {
			lines = append(lines, strings.Join(line, " "))
			line = []string{word}
			continue
		}
		line = append(line, word)
	}
	if len(line) > 0 {
		lines = append(lines, strings.Join(line, " "))
	}
	if len(lines) <= maxLines {
		return strings.Join(lines, "\n")
	}

	merged := strings.Join(lines, " ")
	return merged
}

func endsSentence(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	last := text[len(text)-1]
	return last == '.' || last == '?' || last == '!'
}

func speakerValue(speaker *int) int {
	if speaker == nil {
		return -1
	}
	return *speaker
}
