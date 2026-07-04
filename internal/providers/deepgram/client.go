package deepgram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/andrerfcsantos/subkit-codex/internal/transcript"
)

const DefaultEndpoint = "https://api.deepgram.com/v1/listen"

type Options struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Punctuate     bool   `json:"punctuate"`
	Paragraphs    bool   `json:"paragraphs"`
	SmartFormat   bool   `json:"smart_format"`
	Language      string `json:"language"`
	Diarize       bool   `json:"diarize"`
	DiarizeModel  string `json:"diarize_model,omitempty"`
	Utterances    bool   `json:"utterances"`
	Endpoint      string `json:"endpoint,omitempty"`
	APIKeyEnvName string `json:"api_key_env_name,omitempty"`
}

func DefaultOptions() Options {
	return Options{
		Provider:      "deepgram",
		Model:         "nova-2-video",
		Punctuate:     true,
		Paragraphs:    true,
		SmartFormat:   true,
		Language:      "en-US",
		Diarize:       true,
		Utterances:    true,
		Endpoint:      DefaultEndpoint,
		APIKeyEnvName: "DEEPGRAM_API_KEY",
	}
}

type Client struct {
	HTTPClient *http.Client
}

func (c Client) TranscribeFile(ctx context.Context, audioPath string, contentType string, opts Options) (*transcript.Transcript, []byte, error) {
	if opts.Endpoint == "" {
		opts.Endpoint = DefaultEndpoint
	}
	if opts.APIKeyEnvName == "" {
		opts.APIKeyEnvName = "DEEPGRAM_API_KEY"
	}
	if strings.ToLower(opts.Provider) != "deepgram" {
		return nil, nil, fmt.Errorf("unsupported transcription provider %q", opts.Provider)
	}

	apiKey := os.Getenv(opts.APIKeyEnvName)
	if apiKey == "" {
		return nil, nil, fmt.Errorf("%s is not set", opts.APIKeyEnvName)
	}

	file, err := os.Open(audioPath)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	endpoint, err := url.Parse(opts.Endpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing Deepgram endpoint: %w", err)
	}
	query := endpoint.Query()
	query.Set("model", opts.Model)
	query.Set("punctuate", boolString(opts.Punctuate))
	query.Set("paragraphs", boolString(opts.Paragraphs))
	query.Set("smart_format", boolString(opts.SmartFormat))
	query.Set("language", opts.Language)
	query.Set("diarize", boolString(opts.Diarize))
	query.Set("utterances", boolString(opts.Utterances))
	if opts.DiarizeModel != "" {
		query.Set("diarize_model", opts.DiarizeModel)
	}
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), file)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Token "+apiKey)
	req.Header.Set("Content-Type", contentType)

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, body, fmt.Errorf("deepgram returned %s: %s", resp.Status, trimForError(body))
	}

	var dg Response
	if err := json.Unmarshal(body, &dg); err != nil {
		return nil, body, fmt.Errorf("decoding Deepgram response: %w", err)
	}

	normalized := Normalize(dg, opts)
	return &normalized, body, nil
}

func Normalize(resp Response, opts Options) transcript.Transcript {
	now := time.Now().UTC()
	channel := Channel{}
	if len(resp.Results.Channels) > 0 {
		channel = resp.Results.Channels[0]
	}
	alt := Alternative{}
	if len(channel.Alternatives) > 0 {
		alt = channel.Alternatives[0]
	}

	words, segments := normalizeUtterances(resp.Results.Utterances)
	if len(words) == 0 {
		words = normalizeWords(alt.Words, nil)
	}
	segments = append(segments, normalizeParagraphs(alt.Paragraphs)...)

	providerModel, providerVersion := modelInfo(resp.Metadata)
	if providerModel == "" {
		providerModel = opts.Model
	}

	text := alt.Transcript
	if text == "" {
		text = alt.Paragraphs.Transcript
	}

	return transcript.Transcript{
		SchemaVersion:    transcript.SchemaVersion,
		Provider:         "deepgram",
		ProviderModel:    providerModel,
		ProviderVersion:  providerVersion,
		Language:         opts.Language,
		DetectedLanguage: channel.DetectedLanguage,
		Text:             text,
		DurationSeconds:  resp.Metadata.Duration,
		Channels:         resp.Metadata.Channels,
		CreatedAt:        now,
		Words:            words,
		Segments:         segments,
		Metadata: map[string]any{
			"request_id": resp.Metadata.RequestID,
			"sha256":     resp.Metadata.SHA256,
			"models":     resp.Metadata.Models,
		},
	}
}

func normalizeUtterances(utterances []Utterance) ([]transcript.Word, []transcript.Segment) {
	var words []transcript.Word
	var segments []transcript.Segment
	for _, utt := range utterances {
		wordStart := len(words)
		words = append(words, normalizeWords(utt.Words, utt.Channel)...)
		for i := wordStart; i < len(words); i++ {
			words[i].Index = i
			if words[i].Speaker == nil && utt.Speaker != nil {
				words[i].Speaker = utt.Speaker
			}
			if words[i].Channel == nil && utt.Channel != nil {
				words[i].Channel = utt.Channel
			}
		}
		confidence := utt.Confidence
		segments = append(segments, transcript.Segment{
			ID:         utt.ID,
			Type:       "utterance",
			Text:       utt.Transcript,
			Start:      utt.Start,
			End:        utt.End,
			Confidence: &confidence,
			Speaker:    utt.Speaker,
			Channel:    utt.Channel,
			WordStart:  wordStart,
			WordEnd:    len(words),
		})
	}
	return words, segments
}

func normalizeWords(words []Word, fallbackChannel *int) []transcript.Word {
	normalized := make([]transcript.Word, 0, len(words))
	for i, word := range words {
		channel := word.Channel
		if channel == nil {
			channel = fallbackChannel
		}
		normalized = append(normalized, transcript.Word{
			Index:             i,
			Text:              word.Word,
			Punctuated:        word.PunctuatedWord,
			Start:             word.Start,
			End:               word.End,
			Confidence:        word.Confidence,
			Speaker:           word.Speaker,
			SpeakerConfidence: word.SpeakerConfidence,
			Channel:           channel,
			Language:          word.Language,
		})
	}
	return normalized
}

func normalizeParagraphs(paragraphs Paragraphs) []transcript.Segment {
	var segments []transcript.Segment
	for _, paragraph := range paragraphs.Paragraphs {
		if len(paragraph.Sentences) == 0 {
			continue
		}
		segments = append(segments, transcript.Segment{
			Type:      "paragraph",
			Text:      joinSentences(paragraph.Sentences),
			Start:     paragraph.Start,
			End:       paragraph.End,
			Speaker:   paragraph.Speaker,
			WordStart: 0,
			WordEnd:   paragraph.NumWords,
		})
		for _, sentence := range paragraph.Sentences {
			segments = append(segments, transcript.Segment{
				Type:    "sentence",
				Text:    sentence.Text,
				Start:   sentence.Start,
				End:     sentence.End,
				Speaker: paragraph.Speaker,
			})
		}
	}
	return segments
}

func joinSentences(sentences []Sentence) string {
	var buf bytes.Buffer
	for i, sentence := range sentences {
		if i > 0 {
			buf.WriteByte(' ')
		}
		buf.WriteString(sentence.Text)
	}
	return buf.String()
}

func modelInfo(metadata Metadata) (string, string) {
	for _, uuid := range metadata.Models {
		if info, ok := metadata.ModelInfo[uuid]; ok {
			if info.Name != "" {
				return info.Name, info.Version
			}
			if info.Arch != "" {
				return info.Arch, info.Version
			}
		}
	}
	for _, info := range metadata.ModelInfo {
		if info.Name != "" {
			return info.Name, info.Version
		}
		if info.Arch != "" {
			return info.Arch, info.Version
		}
	}
	return "", ""
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func trimForError(body []byte) string {
	const max = 2048
	if len(body) > max {
		body = body[:max]
	}
	return strings.TrimSpace(string(body))
}
