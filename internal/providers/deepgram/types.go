package deepgram

type Response struct {
	Metadata Metadata `json:"metadata"`
	Results  Results  `json:"results"`
}

type Metadata struct {
	RequestID string               `json:"request_id"`
	SHA256    string               `json:"sha256"`
	Created   string               `json:"created"`
	Duration  float64              `json:"duration"`
	Channels  int                  `json:"channels"`
	Models    []string             `json:"models"`
	ModelInfo map[string]ModelInfo `json:"model_info"`
}

type ModelInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Arch    string `json:"arch"`
}

type Results struct {
	Channels   []Channel   `json:"channels"`
	Utterances []Utterance `json:"utterances"`
}

type Channel struct {
	Alternatives     []Alternative `json:"alternatives"`
	DetectedLanguage string        `json:"detected_language"`
}

type Alternative struct {
	Transcript string     `json:"transcript"`
	Confidence float64    `json:"confidence"`
	Words      []Word     `json:"words"`
	Paragraphs Paragraphs `json:"paragraphs"`
}

type Paragraphs struct {
	Transcript string      `json:"transcript"`
	Paragraphs []Paragraph `json:"paragraphs"`
}

type Paragraph struct {
	Sentences []Sentence `json:"sentences"`
	Speaker   *int       `json:"speaker"`
	NumWords  int        `json:"num_words"`
	Start     float64    `json:"start"`
	End       float64    `json:"end"`
}

type Sentence struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type Utterance struct {
	ID         string  `json:"id"`
	Start      float64 `json:"start"`
	End        float64 `json:"end"`
	Confidence float64 `json:"confidence"`
	Channel    *int    `json:"channel"`
	Transcript string  `json:"transcript"`
	Words      []Word  `json:"words"`
	Speaker    *int    `json:"speaker"`
}

type Word struct {
	Word              string   `json:"word"`
	Start             float64  `json:"start"`
	End               float64  `json:"end"`
	Confidence        *float64 `json:"confidence"`
	PunctuatedWord    string   `json:"punctuated_word"`
	Speaker           *int     `json:"speaker"`
	SpeakerConfidence *float64 `json:"speaker_confidence"`
	Channel           *int     `json:"channel"`
	Language          string   `json:"language"`
}
