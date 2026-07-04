package subtitle

import (
	"fmt"
	"strconv"
	"strings"
)

func Render(cues CueSet, format string) (string, error) {
	switch strings.ToLower(format) {
	case "srt":
		return RenderSRT(cues), nil
	case "vtt", "webvtt":
		return RenderWebVTT(cues), nil
	default:
		return "", fmt.Errorf("unsupported subtitle format %q", format)
	}
}

func RenderSRT(cues CueSet) string {
	var output []string
	for i, cue := range cues.Cues {
		output = append(output, strconv.Itoa(i+1))
		output = append(output, fmt.Sprintf("%s --> %s", timestampSRT(cue.Start), timestampSRT(cue.End)))
		if cue.Speaker != nil {
			output = append(output, fmt.Sprintf("[speaker %d]", *cue.Speaker))
		}
		output = append(output, cue.Text)
		output = append(output, "")
	}
	return strings.Join(output, "\n")
}

func RenderWebVTT(cues CueSet) string {
	output := []string{"WEBVTT", ""}
	for _, cue := range cues.Cues {
		output = append(output, fmt.Sprintf("%s --> %s", timestampVTT(cue.Start), timestampVTT(cue.End)))
		text := cue.Text
		if cue.Speaker != nil {
			text = fmt.Sprintf("<v Speaker %d>%s", *cue.Speaker, text)
		}
		output = append(output, text)
		output = append(output, "")
	}
	return strings.Join(output, "\n")
}

func timestampSRT(seconds float64) string {
	hours, minutes, secs, millis := splitTime(seconds)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", hours, minutes, secs, millis)
}

func timestampVTT(seconds float64) string {
	hours, minutes, secs, millis := splitTime(seconds)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, secs, millis)
}

func splitTime(seconds float64) (int, int, int, int) {
	if seconds < 0 {
		seconds = 0
	}
	totalMillis := int(seconds*1000 + 0.5)
	millis := totalMillis % 1000
	totalSeconds := totalMillis / 1000
	secs := totalSeconds % 60
	totalMinutes := totalSeconds / 60
	minutes := totalMinutes % 60
	hours := totalMinutes / 60
	return hours, minutes, secs, millis
}
