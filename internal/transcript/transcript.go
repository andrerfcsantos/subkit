package transcript

import "time"

const SchemaVersion = "subkit.transcript.v1"

type Transcript struct {
	SchemaVersion    string         `json:"schema_version"`
	Provider         string         `json:"provider"`
	ProviderModel    string         `json:"provider_model,omitempty"`
	ProviderVersion  string         `json:"provider_version,omitempty"`
	Language         string         `json:"language,omitempty"`
	DetectedLanguage string         `json:"detected_language,omitempty"`
	Text             string         `json:"text"`
	DurationSeconds  float64        `json:"duration_seconds,omitempty"`
	Channels         int            `json:"channels,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	Words            []Word         `json:"words"`
	Segments         []Segment      `json:"segments,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`
}

type Word struct {
	Index             int      `json:"index"`
	Text              string   `json:"text"`
	Punctuated        string   `json:"punctuated,omitempty"`
	Start             float64  `json:"start"`
	End               float64  `json:"end"`
	Confidence        *float64 `json:"confidence,omitempty"`
	Speaker           *int     `json:"speaker,omitempty"`
	SpeakerConfidence *float64 `json:"speaker_confidence,omitempty"`
	Channel           *int     `json:"channel,omitempty"`
	Language          string   `json:"language,omitempty"`
}

func (w Word) DisplayText() string {
	if w.Punctuated != "" {
		return w.Punctuated
	}
	return w.Text
}

type Segment struct {
	ID         string   `json:"id,omitempty"`
	Type       string   `json:"type"`
	Text       string   `json:"text"`
	Start      float64  `json:"start"`
	End        float64  `json:"end"`
	Confidence *float64 `json:"confidence,omitempty"`
	Speaker    *int     `json:"speaker,omitempty"`
	Channel    *int     `json:"channel,omitempty"`
	WordStart  int      `json:"word_start,omitempty"`
	WordEnd    int      `json:"word_end,omitempty"`
}
