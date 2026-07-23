package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/andrerfcsantos/subkit-codex/internal/cache"
	"github.com/andrerfcsantos/subkit-codex/internal/media"
	"github.com/andrerfcsantos/subkit-codex/internal/providers/deepgram"
	"github.com/andrerfcsantos/subkit-codex/internal/subtitle"
	"github.com/andrerfcsantos/subkit-codex/internal/transcript"
)

const PipelineVersion = "subkit.pipeline.v1"

type CacheOptions struct {
	Dir        string
	NoCache    bool
	Refresh    bool
	Rerun      []string
	CacheAudio bool
}

type Options struct {
	Audio    media.AudioOptions  `json:"audio"`
	Deepgram deepgram.Options    `json:"deepgram"`
	Cues     subtitle.CueOptions `json:"cues"`
	Cache    CacheOptions        `json:"-"`
	Verbose  bool                `json:"-"`
}

func DefaultOptions() Options {
	return Options{
		Audio:    media.DefaultAudioOptions(),
		Deepgram: deepgram.DefaultOptions(),
		Cues:     subtitle.DefaultCueOptions(),
	}
}

// Transcriber turns a normalized audio artifact into a transcript, returning
// the normalized form together with the provider's raw response body. It is
// the seam between the pipeline and a speech-to-text provider: deepgram.Client
// is the default implementation, and tests or future providers can inject
// their own.
type Transcriber interface {
	TranscribeFile(ctx context.Context, audioPath string, contentType string, opts deepgram.Options) (*transcript.Transcript, []byte, error)
}

type Runner struct {
	Store          *cache.Store
	Opts           Options
	Reporter       Reporter
	Transcriber    Transcriber
	audioMemo      *memoAudio
	transcriptMemo *memoTranscript
	cuesMemo       *memoCues
	sourceHashes   map[string]string
}

type Artifact struct {
	Kind      string
	Key       string
	Path      string
	FromCache bool
}

// The memo types short-circuit repeated work within a single runner. They are
// keyed on the artifact key, which already folds in the source content hash
// and the step options, so a runner whose options change between calls can
// never serve a stale artifact.
type memoAudio struct {
	Artifact Artifact
}

type memoTranscript struct {
	Artifact Artifact
	Data     *transcript.Transcript
}

type memoCues struct {
	Artifact Artifact
	Data     subtitle.CueSet
}

type audioIdentity struct {
	MediaPath string
	Key       string
	Ext       string
}

func NewRunner(opts Options, out io.Writer) (*Runner, error) {
	return NewRunnerWithReporter(opts, &WriterReporter{Out: out})
}

func NewRunnerWithReporter(opts Options, reporter Reporter) (*Runner, error) {
	read := !opts.Cache.NoCache
	write := !opts.Cache.NoCache
	store, err := cache.NewStore(opts.Cache.Dir, read, write, opts.Cache.Refresh, opts.Cache.Rerun)
	if err != nil {
		return nil, err
	}
	return &Runner{Store: store, Opts: opts, Reporter: reporter, Transcriber: deepgram.Client{}}, nil
}

// transcriber returns the runner's transcriber, defaulting to the Deepgram
// client for runners constructed without one.
func (r *Runner) transcriber() Transcriber {
	if r.Transcriber != nil {
		return r.Transcriber
	}
	return deepgram.Client{}
}

func (r *Runner) Close() error {
	return r.Store.Close()
}

// ensureArtifact runs the cache dance shared by every pipeline step: attempt a
// cached read, then take the per-path lock, re-check under the lock, and
// finally build the artifact. read is only invoked when canRead is true and
// path already exists; build must leave the finished artifact at path. The
// returned bool reports whether the artifact came from a cached read.
func ensureArtifact[T any](store *cache.Store, canRead bool, path string, read func() (T, error), build func() (T, error)) (T, bool, error) {
	if canRead && store.Exists(path) {
		value, err := read()
		return value, true, err
	}
	unlock := store.LockPath(path)
	defer unlock()
	if canRead && store.Exists(path) {
		value, err := read()
		return value, true, err
	}
	value, err := build()
	return value, false, err
}

func (r *Runner) audioIdentity(ctx context.Context, mediaPath string) (audioIdentity, error) {
	absMediaPath, err := filepath.Abs(mediaPath)
	if err != nil {
		return audioIdentity{}, err
	}
	sourceHash, err := r.sourceHash(absMediaPath)
	if err != nil {
		return audioIdentity{}, err
	}
	key, err := cache.HashJSON(map[string]any{
		"pipeline":       PipelineVersion,
		"step":           "audio",
		"source_sha256":  sourceHash,
		"options":        r.Opts.Audio,
		"ffmpeg_version": media.FFmpegVersion(ctx),
	})
	if err != nil {
		return audioIdentity{}, err
	}

	return audioIdentity{
		MediaPath: absMediaPath,
		Key:       key,
		Ext:       media.AudioExtension(r.Opts.Audio.Format),
	}, nil
}

// sourceHash memoizes the SHA-256 of source media per absolute path so that
// computing an artifact identity twice doesn't re-read a potentially large
// file from disk.
func (r *Runner) sourceHash(absMediaPath string) (string, error) {
	if hash, ok := r.sourceHashes[absMediaPath]; ok {
		return hash, nil
	}
	hash, err := cache.FileSHA256(absMediaPath)
	if err != nil {
		return "", err
	}
	if r.sourceHashes == nil {
		r.sourceHashes = map[string]string{}
	}
	r.sourceHashes[absMediaPath] = hash
	return hash, nil
}

func (r *Runner) EnsureAudio(ctx context.Context, mediaPath string) (Artifact, error) {
	identity, err := r.audioIdentity(ctx, mediaPath)
	if err != nil {
		return Artifact{}, err
	}
	return r.ensureAudio(ctx, identity)
}

func (r *Runner) ensureAudio(ctx context.Context, identity audioIdentity) (Artifact, error) {
	if r.audioMemo != nil && r.audioMemo.Artifact.Key == identity.Key && r.Store.Exists(r.audioMemo.Artifact.Path) {
		return r.audioMemo.Artifact, nil
	}

	path, persistent, err := r.audioArtifactPath(identity.Key, identity.Ext)
	if err != nil {
		return Artifact{}, err
	}
	canRead := true
	if persistent {
		canRead = r.Store.CanRead("audio")
	}

	_, fromCache, err := ensureArtifact(r.Store, canRead, path, func() (struct{}, error) {
		return struct{}{}, nil
	}, func() (struct{}, error) {
		r.report(StageAudio, "extracting with ffmpeg")
		if err := r.Store.EnsureDir(path); err != nil {
			return struct{}{}, err
		}
		temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
		if err != nil {
			return struct{}{}, err
		}
		tempPath := temp.Name()
		if err := temp.Close(); err != nil {
			_ = os.Remove(tempPath)
			return struct{}{}, err
		}
		defer func() {
			_ = os.Remove(tempPath)
		}()
		if err := media.ExtractAudio(ctx, identity.MediaPath, tempPath, r.Opts.Audio); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, r.Store.CommitFile(tempPath, path)
	})
	if err != nil {
		return Artifact{}, err
	}

	fromCache = fromCache && persistent
	if fromCache {
		r.report(StageAudio, "cache hit %s", path)
	}
	artifact := Artifact{Kind: "audio", Key: identity.Key, Path: path, FromCache: fromCache}
	r.audioMemo = &memoAudio{Artifact: artifact}
	return artifact, nil
}

func (r *Runner) EnsureTranscript(ctx context.Context, mediaPath string) (Artifact, *transcript.Transcript, error) {
	audioIdentity, err := r.audioIdentity(ctx, mediaPath)
	if err != nil {
		return Artifact{}, nil, err
	}
	key, err := cache.HashJSON(map[string]any{
		"pipeline":  PipelineVersion,
		"step":      "transcript",
		"audio_key": audioIdentity.Key,
		"options":   r.Opts.Deepgram,
	})
	if err != nil {
		return Artifact{}, nil, err
	}
	if r.transcriptMemo != nil && r.transcriptMemo.Artifact.Key == key {
		return r.transcriptMemo.Artifact, r.transcriptMemo.Data, nil
	}

	path := r.Store.Path("transcripts", key, "json")
	rawPath := r.Store.Path("deepgram-raw", key, "json")
	fromRaw := false
	t, fromCache, err := ensureArtifact(r.Store, r.Store.CanRead("transcribe"), path, func() (*transcript.Transcript, error) {
		var t transcript.Transcript
		if err := r.Store.ReadJSON(path, &t); err != nil {
			return nil, err
		}
		r.report(StageTranscribe, "cache hit %s", path)
		return &t, nil
	}, func() (*transcript.Transcript, error) {
		if r.Store.CanRead("transcribe") && r.Store.Exists(rawPath) {
			raw, err := os.ReadFile(rawPath)
			if err != nil {
				return nil, err
			}
			var response deepgram.Response
			if err := json.Unmarshal(raw, &response); err != nil {
				return nil, err
			}
			t := deepgram.Normalize(response, r.Opts.Deepgram)
			if err := r.Store.WriteJSON(path, t); err != nil {
				return nil, err
			}
			r.report(StageTranscribe, "rebuilt normalized transcript from raw cache")
			fromRaw = true
			return &t, nil
		}

		audioArtifact, err := r.ensureAudio(ctx, audioIdentity)
		if err != nil {
			return nil, err
		}
		r.report(StageTranscribe, "calling Deepgram")
		contentType := media.AudioContentType(r.Opts.Audio.Format)
		t, raw, err := r.transcriber().TranscribeFile(ctx, audioArtifact.Path, contentType, r.Opts.Deepgram)
		if err != nil {
			return nil, err
		}
		if err := r.Store.WriteFile(rawPath, raw, 0o644); err != nil {
			return nil, err
		}
		if err := r.Store.WriteJSON(path, t); err != nil {
			return nil, err
		}
		return t, nil
	})
	if err != nil {
		return Artifact{}, nil, err
	}

	artifact := Artifact{Kind: "transcript", Key: key, Path: path, FromCache: fromCache || fromRaw}
	r.transcriptMemo = &memoTranscript{Artifact: artifact, Data: t}
	return artifact, t, nil
}

func (r *Runner) EnsureCues(ctx context.Context, mediaPath string) (Artifact, subtitle.CueSet, error) {
	transcriptArtifact, t, err := r.EnsureTranscript(ctx, mediaPath)
	if err != nil {
		return Artifact{}, subtitle.CueSet{}, err
	}
	key, err := cache.HashJSON(map[string]any{
		"pipeline":       PipelineVersion,
		"step":           "cues",
		"transcript_key": transcriptArtifact.Key,
		"options":        r.Opts.Cues,
	})
	if err != nil {
		return Artifact{}, subtitle.CueSet{}, err
	}
	if r.cuesMemo != nil && r.cuesMemo.Artifact.Key == key {
		return r.cuesMemo.Artifact, r.cuesMemo.Data, nil
	}

	path := r.Store.Path("cues", key, "json")
	cues, fromCache, err := ensureArtifact(r.Store, r.Store.CanRead("cues"), path, func() (subtitle.CueSet, error) {
		var cues subtitle.CueSet
		if err := r.Store.ReadJSON(path, &cues); err != nil {
			return subtitle.CueSet{}, err
		}
		r.report(StageCues, "cache hit %s", path)
		return cues, nil
	}, func() (subtitle.CueSet, error) {
		r.report(StageCues, "building subtitle cues")
		cues := subtitle.BuildCues(*t, r.Opts.Cues)
		return cues, r.Store.WriteJSON(path, cues)
	})
	if err != nil {
		return Artifact{}, subtitle.CueSet{}, err
	}

	artifact := Artifact{Kind: "cues", Key: key, Path: path, FromCache: fromCache}
	r.cuesMemo = &memoCues{Artifact: artifact, Data: cues}
	return artifact, cues, nil
}

func (r *Runner) EnsureSubtitle(ctx context.Context, mediaPath string, format string, outputPath string) (Artifact, string, bool, error) {
	format = strings.ToLower(format)
	cuesArtifact, cues, err := r.EnsureCues(ctx, mediaPath)
	if err != nil {
		return Artifact{}, "", false, err
	}
	key, err := cache.HashJSON(map[string]any{
		"pipeline": PipelineVersion,
		"step":     "render",
		"kind":     "subtitle",
		"format":   format,
		"cues_key": cuesArtifact.Key,
	})
	if err != nil {
		return Artifact{}, "", false, err
	}

	cachePath := r.Store.Path("outputs", key, format)
	_, _, err = ensureArtifact(r.Store, r.Store.CanRead("render"), cachePath, r.renderCacheHit(cachePath), func() (struct{}, error) {
		r.report(StageRender, "writing %s subtitle", format)
		rendered, err := subtitle.Render(cues, format)
		if err != nil {
			return struct{}{}, err
		}
		return struct{}{}, r.Store.WriteFile(cachePath, []byte(rendered), 0o644)
	})
	if err != nil {
		return Artifact{}, "", false, err
	}

	if outputPath == "" {
		outputPath = defaultOutputPath(mediaPath, format)
	}
	copied, err := cache.CopyFileIfDifferent(cachePath, outputPath)
	if err != nil {
		return Artifact{}, "", false, err
	}
	r.reportWrite(outputPath, copied)
	return Artifact{Kind: "subtitle", Key: key, Path: cachePath, FromCache: !copied}, outputPath, copied, nil
}

func (r *Runner) EnsureScript(ctx context.Context, mediaPath string, format string, outputPath string) (Artifact, string, bool, error) {
	if format == "" {
		format = "txt"
	}
	if format != "txt" && format != "md" {
		return Artifact{}, "", false, fmt.Errorf("unsupported script format %q", format)
	}
	transcriptArtifact, t, err := r.EnsureTranscript(ctx, mediaPath)
	if err != nil {
		return Artifact{}, "", false, err
	}
	key, err := cache.HashJSON(map[string]any{
		"pipeline":       PipelineVersion,
		"step":           "render",
		"kind":           "script",
		"format":         format,
		"transcript_key": transcriptArtifact.Key,
	})
	if err != nil {
		return Artifact{}, "", false, err
	}

	cachePath := r.Store.Path("outputs", key, format)
	_, _, err = ensureArtifact(r.Store, r.Store.CanRead("render"), cachePath, r.renderCacheHit(cachePath), func() (struct{}, error) {
		r.report(StageRender, "writing %s script", format)
		content := strings.TrimSpace(t.Text) + "\n"
		return struct{}{}, r.Store.WriteFile(cachePath, []byte(content), 0o644)
	})
	if err != nil {
		return Artifact{}, "", false, err
	}

	if outputPath == "" {
		outputPath = defaultNamedOutputPath(mediaPath, "script", format)
	}
	copied, err := cache.CopyFileIfDifferent(cachePath, outputPath)
	if err != nil {
		return Artifact{}, "", false, err
	}
	r.reportWrite(outputPath, copied)
	return Artifact{Kind: "script", Key: key, Path: cachePath, FromCache: !copied}, outputPath, copied, nil
}

func (r *Runner) EnsureWords(ctx context.Context, mediaPath string, outputPath string) (Artifact, string, bool, error) {
	transcriptArtifact, t, err := r.EnsureTranscript(ctx, mediaPath)
	if err != nil {
		return Artifact{}, "", false, err
	}
	key, err := cache.HashJSON(map[string]any{
		"pipeline":       PipelineVersion,
		"step":           "render",
		"kind":           "words",
		"format":         "json",
		"transcript_key": transcriptArtifact.Key,
	})
	if err != nil {
		return Artifact{}, "", false, err
	}

	cachePath := r.Store.Path("outputs", key, "json")
	_, _, err = ensureArtifact(r.Store, r.Store.CanRead("render"), cachePath, r.renderCacheHit(cachePath), func() (struct{}, error) {
		r.report(StageRender, "writing words json")
		return struct{}{}, r.Store.WriteJSON(cachePath, t.Words)
	})
	if err != nil {
		return Artifact{}, "", false, err
	}

	if outputPath == "" {
		outputPath = defaultNamedOutputPath(mediaPath, "words", "json")
	}
	copied, err := cache.CopyFileIfDifferent(cachePath, outputPath)
	if err != nil {
		return Artifact{}, "", false, err
	}
	r.reportWrite(outputPath, copied)
	return Artifact{Kind: "words", Key: key, Path: cachePath, FromCache: !copied}, outputPath, copied, nil
}

func (r *Runner) CacheRoot() string {
	return r.Store.Root
}

// renderCacheHit is the shared read step for rendered outputs: the cached file
// itself is the artifact, so a hit only needs to be reported.
func (r *Runner) renderCacheHit(path string) func() (struct{}, error) {
	return func() (struct{}, error) {
		r.report(StageRender, "cache hit %s", path)
		return struct{}{}, nil
	}
}

func (r *Runner) audioArtifactPath(key string, ext string) (string, bool, error) {
	if r.persistAudio() {
		return r.Store.Path("audio", key, ext), true, nil
	}
	scratch, err := r.Store.Scratch()
	if err != nil {
		return "", false, err
	}
	return filepath.Join(scratch, "audio", artifactFileName(key, ext)), false, nil
}

func (r *Runner) persistAudio() bool {
	return r.Opts.Cache.CacheAudio && r.Store.CanWrite()
}

func artifactFileName(key string, ext string) string {
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return key + ext
}

func (r *Runner) report(stage Stage, format string, args ...any) {
	if r.Reporter == nil {
		return
	}
	r.Reporter.Report(Event{Stage: stage, Message: fmt.Sprintf(format, args...)})
}

func (r *Runner) reportWrite(path string, copied bool) {
	if copied {
		r.report(StageWrite, "wrote %s", path)
		return
	}
	r.report(StageWrite, "cached %s", path)
}

func defaultOutputPath(mediaPath string, ext string) string {
	return replaceExt(mediaPath, ext)
}

func defaultNamedOutputPath(mediaPath string, suffix string, ext string) string {
	dir := filepath.Dir(mediaPath)
	base := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))
	return filepath.Join(dir, base+"."+suffix+"."+ext)
}

func replaceExt(path string, ext string) string {
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return strings.TrimSuffix(path, filepath.Ext(path)) + ext
}
