package deepgram

import "testing"

func TestNormalizePrefersUtteranceWords(t *testing.T) {
	confidence := 0.9
	speaker := 3
	channel := 1
	resp := Response{
		Metadata: Metadata{
			RequestID: "req-1",
			Duration:  2.5,
			Channels:  1,
			Models:    []string{"model-id"},
			ModelInfo: map[string]ModelInfo{
				"model-id": {Name: "nova-test", Version: "v1"},
			},
		},
		Results: Results{
			Channels: []Channel{
				{
					DetectedLanguage: "en",
					Alternatives: []Alternative{
						{
							Transcript: "hello world",
							Words: []Word{
								{Word: "fallback", Start: 0, End: 1},
							},
						},
					},
				},
			},
			Utterances: []Utterance{
				{
					ID:         "utt-1",
					Start:      0,
					End:        2,
					Confidence: confidence,
					Speaker:    &speaker,
					Channel:    &channel,
					Transcript: "Hello world.",
					Words: []Word{
						{Word: "hello", PunctuatedWord: "Hello", Start: 0, End: 0.4, Confidence: &confidence},
						{Word: "world", PunctuatedWord: "world.", Start: 0.5, End: 1.0, Confidence: &confidence},
					},
				},
			},
		},
	}

	got := Normalize(resp, DefaultOptions())
	if got.Provider != "deepgram" {
		t.Fatalf("provider = %q", got.Provider)
	}
	if got.ProviderModel != "nova-test" || got.ProviderVersion != "v1" {
		t.Fatalf("model info = %q %q", got.ProviderModel, got.ProviderVersion)
	}
	if len(got.Words) != 2 {
		t.Fatalf("words len = %d", len(got.Words))
	}
	if got.Words[0].DisplayText() != "Hello" {
		t.Fatalf("first word = %q", got.Words[0].DisplayText())
	}
	if got.Words[0].Speaker == nil || *got.Words[0].Speaker != speaker {
		t.Fatalf("speaker was not propagated: %#v", got.Words[0].Speaker)
	}
	if len(got.Segments) == 0 || got.Segments[0].Type != "utterance" {
		t.Fatalf("missing utterance segment: %#v", got.Segments)
	}
}
