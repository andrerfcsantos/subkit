package subtitle

import (
	"github.com/andrerfcsantos/subkit-codex/internal/transcript"
)

// defaultDeepgramWordsPerLine matches the reference converter's default
// LineLength of 8 words per caption.
const defaultDeepgramWordsPerLine = 8

// deepgramCues ports the DeepgramConverter from
// github.com/andrerfcsantos/deepgram-go-captions. Utterances are chunked into
// fixed-size word groups; without utterances the flat word list is buffered,
// splitting on speaker changes (when diarized) and on full buffers. Timing is
// exactly the first word's start to the last word's end.
func deepgramCues(t transcript.Transcript, opts CueOptions) []Cue {
	lineLength := opts.MaxWordsPerLine
	if lineLength <= 0 {
		lineLength = defaultDeepgramWordsPerLine
	}

	var cues []Cue
	for _, run := range collectRuns(t, opts.PreferSegments) {
		switch {
		case len(run.Words) > 0 && run.FromSegment:
			// Utterance run: chunk into lineLength groups with no other splits.
			cues = append(cues, deepgramChunkCues(run.Words, lineLength)...)
		case len(run.Words) > 0:
			// Flat word list: split on speaker changes and full buffers.
			cues = append(cues, deepgramWordCues(run.Words, lineLength)...)
		case run.Segment != nil:
			cues = append(cues, Cue{
				Start:   run.Segment.Start,
				End:     run.Segment.End,
				Text:    collapseSpaces(run.Segment.Text),
				Speaker: run.Segment.Speaker,
			})
		}
	}
	return cues
}

func deepgramChunkCues(words []transcript.Word, lineLength int) []Cue {
	words = nonEmptyWords(words)
	var cues []Cue
	for start := 0; start < len(words); start += lineLength {
		end := min(start+lineLength, len(words))
		cues = append(cues, deepgramCue(words[start:end]))
	}
	return cues
}

func deepgramWordCues(words []transcript.Word, lineLength int) []Cue {
	words = nonEmptyWords(words)
	if len(words) == 0 {
		return nil
	}
	diarize := words[0].Speaker != nil

	var cues []Cue
	var buffer []transcript.Word
	currentSpeaker := 0
	for _, word := range words {
		if diarize {
			speaker := speakerValue(word.Speaker)
			if speaker != currentSpeaker && len(buffer) > 0 {
				cues = append(cues, deepgramCue(buffer))
				buffer = nil
			}
			currentSpeaker = speaker
		}
		if len(buffer) == lineLength {
			cues = append(cues, deepgramCue(buffer))
			buffer = nil
		}
		buffer = append(buffer, word)
	}
	if len(buffer) > 0 {
		cues = append(cues, deepgramCue(buffer))
	}
	return cues
}

func deepgramCue(words []transcript.Word) Cue {
	return Cue{
		Start:   words[0].Start,
		End:     words[len(words)-1].End,
		Text:    wordsText(words),
		Speaker: words[0].Speaker,
		Words:   wordIndexes(words),
	}
}

func nonEmptyWords(words []transcript.Word) []transcript.Word {
	filtered := words[:0:0]
	for _, word := range words {
		if word.Text == "" && word.Punctuated == "" {
			continue
		}
		filtered = append(filtered, word)
	}
	return filtered
}
