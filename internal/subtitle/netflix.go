package subtitle

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/andrerfcsantos/subkit-codex/internal/transcript"
)

// Netflix Timed Text Style Guide defaults. Character and line limits come
// from the general requirements and subtitle template guides for Latin-script
// languages; timing values come from the subtitle timing guidelines, using
// 24fps for the frame-based rules.
const (
	netflixMaxCharsPerLine = 42
	netflixMaxLines        = 2
	// Minimum event duration: 5/6 of a second (20 frames at 24fps).
	netflixMinDuration = 5.0 / 6.0
	// Maximum event duration: 7 seconds.
	netflixMaxDuration = 7.0
	// Reading speed for adult templates: up to 17 characters per second.
	netflixReadingSpeed = 17.0
	// Events must keep a minimum of 2 frames between them.
	netflixMinGap = 2.0 / 24.0
	// Gaps shorter than half a second must be closed to the minimum gap.
	netflixChainGap = 0.5
	// Out-times should ideally extend about half a second past the audio.
	netflixOutPadding = 0.5
	// Silence long enough to force a new event even mid-sentence.
	netflixDefaultMaxGap = 1.0
)

// netflixEvent is a cue candidate before timing polish: its words, the
// unwrapped text, and the audio span they cover.
type netflixEvent struct {
	words     []transcript.Word
	start     float64
	end       float64
	speaker   *int
	synthetic bool
}

// netflixCues builds cues following the Netflix Timed Text Style Guide:
// sentence-aware segmentation into events of at most two 42-character lines,
// linguistically informed line breaks, reading-speed aware out-times, and
// 2-frame gap chaining between consecutive events.
func netflixCues(t transcript.Transcript, opts CueOptions) []Cue {
	maxChars := opts.MaxCharsPerLine
	if maxChars <= 0 {
		maxChars = netflixMaxCharsPerLine
	}
	maxLines := opts.MaxLines
	if maxLines <= 0 {
		maxLines = netflixMaxLines
	}
	minDuration := opts.MinDuration
	if minDuration <= 0 {
		minDuration = netflixMinDuration
	}
	maxDuration := opts.MaxDuration
	if maxDuration <= 0 {
		maxDuration = netflixMaxDuration
	}
	maxGap := opts.MaxGap
	if maxGap <= 0 {
		maxGap = netflixDefaultMaxGap
	}
	readingSpeed := opts.ReadingSpeed
	if readingSpeed <= 0 {
		readingSpeed = netflixReadingSpeed
	}
	capacity := maxChars * maxLines

	var events []netflixEvent
	for _, run := range collectRuns(t, opts.PreferSegments) {
		words := run.Words
		synthetic := false
		if len(words) == 0 && run.Segment != nil {
			words = synthesizeWords(run.Segment)
			synthetic = true
		}
		for _, block := range splitBlocks(nonEmptyWords(words), maxGap) {
			for _, group := range groupBlock(block, capacity, maxDuration) {
				events = append(events, netflixEvent{
					words:     group,
					start:     group[0].Start,
					end:       group[len(group)-1].End,
					speaker:   group[0].Speaker,
					synthetic: synthetic,
				})
			}
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].start < events[j].start })

	timeEvents(events, minDuration, maxDuration, readingSpeed)

	cues := make([]Cue, 0, len(events))
	for _, event := range events {
		cue := Cue{
			Start:   event.start,
			End:     event.end,
			Text:    breakLines(event.words, maxChars, maxLines),
			Speaker: event.speaker,
		}
		if !event.synthetic {
			cue.Words = wordIndexes(event.words)
		}
		cues = append(cues, cue)
	}
	return cues
}

// splitBlocks cuts a word run at speaker changes and at silences longer than
// maxGap, so no event ever spans either boundary.
func splitBlocks(words []transcript.Word, maxGap float64) [][]transcript.Word {
	var blocks [][]transcript.Word
	var current []transcript.Word
	for _, word := range words {
		if len(current) > 0 {
			last := current[len(current)-1]
			if speakerValue(last.Speaker) != speakerValue(word.Speaker) || word.Start-last.End > maxGap {
				blocks = append(blocks, current)
				current = nil
			}
		}
		current = append(current, word)
	}
	if len(current) > 0 {
		blocks = append(blocks, current)
	}
	return blocks
}

// groupBlock turns one block into event word groups: the block is split into
// sentences, sentences that cannot fit an event are divided at the best
// linguistic break points, and adjacent pieces are packed together while they
// fit the character capacity and maximum duration.
func groupBlock(block []transcript.Word, capacity int, maxDuration float64) [][]transcript.Word {
	if len(block) == 0 {
		return nil
	}
	var pieces [][]transcript.Word
	for _, sentence := range splitSentences(block) {
		pieces = append(pieces, splitToFit(sentence, capacity, maxDuration)...)
	}

	var groups [][]transcript.Word
	var current []transcript.Word
	for _, piece := range pieces {
		if len(current) == 0 {
			current = piece
			continue
		}
		if charLen(current)+1+charLen(piece) <= capacity &&
			piece[len(piece)-1].End-current[0].Start <= maxDuration {
			current = append(current, piece...)
			continue
		}
		groups = append(groups, current)
		current = piece
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

// splitSentences divides words into sentence groups using terminal
// punctuation on the display text.
func splitSentences(words []transcript.Word) [][]transcript.Word {
	var sentences [][]transcript.Word
	var current []transcript.Word
	for _, word := range words {
		current = append(current, word)
		if endsSentence(word.DisplayText()) {
			sentences = append(sentences, current)
			current = nil
		}
	}
	if len(current) > 0 {
		sentences = append(sentences, current)
	}
	return sentences
}

// splitToFit recursively splits a sentence that exceeds the event character
// capacity or maximum duration, choosing the boundary with the best break
// quality closest to the middle.
func splitToFit(words []transcript.Word, capacity int, maxDuration float64) [][]transcript.Word {
	if len(words) < 2 {
		return [][]transcript.Word{words}
	}
	if charLen(words) <= capacity && words[len(words)-1].End-words[0].Start <= maxDuration {
		return [][]transcript.Word{words}
	}

	total := charLen(words)
	bestIndex := 1
	bestScore := -1 << 30
	prefix := 0
	for i := 1; i < len(words); i++ {
		prefix += utf8.RuneCountInString(words[i-1].DisplayText())
		if i > 1 {
			prefix++
		}
		balance := prefix*2 - total
		if balance < 0 {
			balance = -balance
		}
		score := breakQuality(words, i)*1000 - balance
		if score > bestScore {
			bestScore = score
			bestIndex = i
		}
	}

	left := splitToFit(words[:bestIndex], capacity, maxDuration)
	right := splitToFit(words[bestIndex:], capacity, maxDuration)
	return append(left, right...)
}

// breakLines renders a word group as cue text of at most maxLines lines,
// breaking at the highest-quality boundary and favoring bottom-heavy lines
// when quality ties, per the subtitle template guidelines.
func breakLines(words []transcript.Word, maxChars int, maxLines int) string {
	if charLen(words) <= maxChars || maxLines < 2 {
		return wordsText(words)
	}

	total := charLen(words)
	bestIndex := -1
	bestScore := -1 << 30
	prefix := 0
	for i := 1; i < len(words); i++ {
		prefix += utf8.RuneCountInString(words[i-1].DisplayText())
		if i > 1 {
			prefix++
		}
		suffix := total - prefix - 1
		if prefix > maxChars || suffix > maxChars {
			continue
		}
		balance := prefix - suffix
		if balance < 0 {
			balance = -balance
		}
		score := breakQuality(words, i) * 1000
		if prefix <= suffix {
			score += 500
		}
		score -= balance
		if score > bestScore {
			bestScore = score
			bestIndex = i
		}
	}
	if bestIndex < 0 {
		return greedyLines(words, maxChars)
	}
	return wordsText(words[:bestIndex]) + "\n" + wordsText(words[bestIndex:])
}

// greedyLines is the fallback when no boundary satisfies the per-line limit,
// which only happens around words longer than a whole line.
func greedyLines(words []transcript.Word, maxChars int) string {
	var lines []string
	var line []string
	length := 0
	for _, word := range words {
		text := word.DisplayText()
		runes := utf8.RuneCountInString(text)
		if len(line) > 0 && length+1+runes > maxChars {
			lines = append(lines, strings.Join(line, " "))
			line = nil
			length = 0
		}
		if len(line) > 0 {
			length++
		}
		line = append(line, text)
		length += runes
	}
	if len(line) > 0 {
		lines = append(lines, strings.Join(line, " "))
	}
	return strings.Join(lines, "\n")
}

// timeEvents applies the timing guidelines to the ordered events: out-times
// extend to satisfy the minimum duration and reading speed and ideally run
// half a second past the audio, capped by the maximum duration and the next
// event's in-time minus the 2-frame gap. Remaining sub-half-second gaps are
// then chained closed to 2 frames.
func timeEvents(events []netflixEvent, minDuration float64, maxDuration float64, readingSpeed float64) {
	for i := range events {
		event := &events[i]
		chars := float64(charLen(event.words))
		end := event.end + netflixOutPadding
		if minEnd := event.start + minDuration; end < minEnd {
			end = minEnd
		}
		if readEnd := event.start + chars/readingSpeed; end < readEnd {
			end = readEnd
		}
		if maxEnd := event.start + maxDuration; end > maxEnd {
			end = maxEnd
		}
		if i+1 < len(events) {
			// The next event's in-time minus the 2-frame gap is a hard cap:
			// events must never overlap, even if that trims the audio span.
			if limit := events[i+1].start - netflixMinGap; end > limit {
				end = limit
			}
		}
		if end > event.start {
			event.end = end
		}
	}

	for i := 0; i+1 < len(events); i++ {
		gap := events[i+1].start - events[i].end
		if gap > netflixMinGap && gap < netflixChainGap {
			events[i].end = events[i+1].start - netflixMinGap
		}
	}
}

// synthesizeWords fabricates proportionally timed words for utterance
// segments that carry text but no word range, so the same segmentation and
// timing rules apply to them.
func synthesizeWords(segment *transcript.Segment) []transcript.Word {
	fields := strings.Fields(segment.Text)
	if len(fields) == 0 {
		return nil
	}
	total := 0
	for _, field := range fields {
		total += utf8.RuneCountInString(field) + 1
	}
	duration := segment.End - segment.Start
	words := make([]transcript.Word, 0, len(fields))
	elapsed := 0
	for _, field := range fields {
		weight := utf8.RuneCountInString(field) + 1
		start := segment.Start + duration*float64(elapsed)/float64(total)
		elapsed += weight
		end := segment.Start + duration*float64(elapsed)/float64(total)
		words = append(words, transcript.Word{
			Index:   -1,
			Text:    field,
			Start:   start,
			End:     end,
			Speaker: segment.Speaker,
		})
	}
	return words
}

// charLen is the rendered length of a word group on a single line: the rune
// count of the display words joined by spaces.
func charLen(words []transcript.Word) int {
	length := 0
	for i, word := range words {
		if i > 0 {
			length++
		}
		length += utf8.RuneCountInString(word.DisplayText())
	}
	return length
}

// endsSentence reports whether display text terminates a sentence, ignoring
// trailing closing quotes or brackets.
func endsSentence(text string) bool {
	trimmed := strings.TrimRightFunc(text, func(r rune) bool {
		switch r {
		case '"', '\'', ')', ']', '»', '”', '’':
			return true
		}
		return false
	})
	last, size := utf8.DecodeLastRuneInString(trimmed)
	if size == 0 {
		return false
	}
	switch last {
	case '.', '!', '?', '…':
		return true
	}
	return false
}

// endsClause reports whether display text ends with clause punctuation such
// as a comma, semicolon, colon, or dash.
func endsClause(text string) bool {
	last, size := utf8.DecodeLastRuneInString(text)
	if size == 0 {
		return false
	}
	switch last {
	case ',', ';', ':', '—', '–':
		return true
	}
	return false
}

// breakQuality scores the boundary before words[i]. Positive scores mark
// breaks the style guide asks for (after punctuation, before conjunctions and
// prepositions); negative scores mark separations it forbids, such as
// splitting an article or preposition from what follows it.
func breakQuality(words []transcript.Word, i int) int {
	prev := words[i-1].DisplayText()
	next := words[i].DisplayText()
	prevWord := normalizeWord(prev)
	nextWord := normalizeWord(next)

	score := 0
	switch {
	case endsSentence(prev):
		score += 100
	case endsClause(prev):
		score += 80
	case conjunctionWords[nextWord]:
		score += 60
	case prepositionWords[nextWord]:
		score += 50
	}
	if determinerWords[prevWord] {
		score -= 80
	}
	if prepositionWords[prevWord] {
		score -= 40
	}
	if conjunctionWords[prevWord] {
		score -= 40
	}
	return score
}

// normalizeWord lowercases display text and strips surrounding punctuation so
// it can be matched against the function-word lists.
func normalizeWord(text string) string {
	return strings.ToLower(strings.TrimFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}))
}

// Function-word lists used by breakQuality, covering English plus the
// Portuguese the project's sample media uses. Other languages fall back to
// punctuation- and balance-driven breaks.
var determinerWords = wordSet(
	"a", "an", "the", "this", "that", "these", "those",
	"my", "your", "his", "her", "its", "our", "their",
	"o", "os", "as", "um", "uma", "uns", "umas",
	"meu", "minha", "seu", "sua", "este", "esta", "esse", "essa", "aquele", "aquela",
)

var conjunctionWords = wordSet(
	"and", "but", "or", "nor", "so", "yet", "because", "although", "though",
	"while", "when", "if", "unless", "until", "since", "after", "before",
	"e", "mas", "ou", "porque", "embora", "enquanto", "quando", "se",
	"pois", "porém", "contudo",
)

var prepositionWords = wordSet(
	"in", "on", "at", "to", "of", "with", "from", "by", "about", "into",
	"over", "under", "between", "through", "during", "against", "without", "for",
	"em", "no", "na", "nos", "nas", "de", "do", "da", "dos", "das",
	"para", "por", "pelo", "pela", "com", "sem", "sobre", "entre", "contra", "durante", "até",
)

func wordSet(words ...string) map[string]bool {
	set := make(map[string]bool, len(words))
	for _, word := range words {
		set[word] = true
	}
	return set
}
