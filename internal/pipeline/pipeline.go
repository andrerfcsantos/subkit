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

type Runner struct {
	Store          *cache.Store
	Opts           Options
	Reporter       Reporter
	tempDir        string
	audioMemo      *memoAudio
	transcriptMemo *memoTranscript
	cuesMemo       *memoCues
}

type Artifact struct {
	Kind      string
	Key       string
	Path      string
	FromCache bool
}

type memoAudio struct {
	MediaPath string
	Artifact  Artifact
}

type memoTranscript struct {
	MediaPath string
	Artifact  Artifact
	Data      *transcript.Transcript
}

type memoCues struct {
	MediaPath string
	Artifact  Artifact
	Data      subtitle.CueSet
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
	return &Runner{Store: store, Opts: opts, Reporter: reporter}, nil
}

func (r *Runner) Close() error {
	if r.tempDir == "" {
		return nil
	}
	tempDir := r.tempDir
	r.tempDir = ""
	return os.RemoveAll(tempDir)
}

func (r *Runner) audioIdentity(ctx context.Context, mediaPath string) (audioIdentity, error) {
	absMediaPath, err := filepath.Abs(mediaPath)
	if err != nil {
		return audioIdentity{}, err
	}
	sourceHash, err := cache.FileSHA256(absMediaPath)
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

func (r *Runner) EnsureAudio(ctx context.Context, mediaPath string) (Artifact, error) {
	identity, err := r.audioIdentity(ctx, mediaPath)
	if err != nil {
		return Artifact{}, err
	}
	return r.ensureAudio(ctx, identity)
}

func (r *Runner) ensureAudio(ctx context.Context, identity audioIdentity) (Artifact, error) {
	if r.audioMemo != nil && r.audioMemo.MediaPath == identity.MediaPath && r.audioMemo.Artifact.Key == identity.Key && r.Store.Exists(r.audioMemo.Artifact.Path) {
		return r.audioMemo.Artifact, nil
	}

	path, persistent, err := r.audioArtifactPath(identity.Key, identity.Ext)
	if err != nil {
		return Artifact{}, err
	}
	if persistent && r.Store.CanRead("audio") && r.Store.Exists(path) {
		r.report(StageAudio, "cache hit %s", path)
		artifact := Artifact{Kind: "audio", Key: identity.Key, Path: path, FromCache: true}
		r.audioMemo = &memoAudio{MediaPath: identity.MediaPath, Artifact: artifact}
		return artifact, nil
	}

	unlock := r.Store.LockPath(path)
	defer unlock()
	if persistent && r.Store.CanRead("audio") && r.Store.Exists(path) {
		r.report(StageAudio, "cache hit %s", path)
		artifact := Artifact{Kind: "audio", Key: identity.Key, Path: path, FromCache: true}
		r.audioMemo = &memoAudio{MediaPath: identity.MediaPath, Artifact: artifact}
		return artifact, nil
	}
	if !persistent && r.Store.Exists(path) {
		artifact := Artifact{Kind: "audio", Key: identity.Key, Path: path}
		r.audioMemo = &memoAudio{MediaPath: identity.MediaPath, Artifact: artifact}
		return artifact, nil
	}

	r.report(StageAudio, "extracting with ffmpeg")
	if err := r.Store.EnsureDir(path); err != nil {
		return Artifact{}, err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return Artifact{}, err
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return Artifact{}, err
	}
	defer func() {
		_ = os.Remove(tempPath)
	}()
	if err := media.ExtractAudio(ctx, identity.MediaPath, tempPath, r.Opts.Audio); err != nil {
		return Artifact{}, err
	}
	if err := r.Store.CommitFile(tempPath, path); err != nil {
		return Artifact{}, err
	}
	artifact := Artifact{Kind: "audio", Key: identity.Key, Path: path}
	r.audioMemo = &memoAudio{MediaPath: identity.MediaPath, Artifact: artifact}
	return artifact, nil
}

func (r *Runner) EnsureTranscript(ctx context.Context, mediaPath string) (Artifact, *transcript.Transcript, error) {
	absMediaPath, err := filepath.Abs(mediaPath)
	if err != nil {
		return Artifact{}, nil, err
	}
	if r.transcriptMemo != nil && r.transcriptMemo.MediaPath == absMediaPath {
		return r.transcriptMemo.Artifact, r.transcriptMemo.Data, nil
	}
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

	path := r.Store.Path("transcripts", key, "json")
	if r.Store.CanRead("transcribe") && r.Store.Exists(path) {
		var t transcript.Transcript
		if err := r.Store.ReadJSON(path, &t); err != nil {
			return Artifact{}, nil, err
		}
		r.report(StageTranscribe, "cache hit %s", path)
		artifact := Artifact{Kind: "transcript", Key: key, Path: path, FromCache: true}
		r.transcriptMemo = &memoTranscript{MediaPath: absMediaPath, Artifact: artifact, Data: &t}
		return artifact, &t, nil
	}

	unlock := r.Store.LockPath(path)
	defer unlock()
	if r.Store.CanRead("transcribe") && r.Store.Exists(path) {
		var t transcript.Transcript
		if err := r.Store.ReadJSON(path, &t); err != nil {
			return Artifact{}, nil, err
		}
		r.report(StageTranscribe, "cache hit %s", path)
		artifact := Artifact{Kind: "transcript", Key: key, Path: path, FromCache: true}
		r.transcriptMemo = &memoTranscript{MediaPath: absMediaPath, Artifact: artifact, Data: &t}
		return artifact, &t, nil
	}

	rawPath := r.Store.Path("deepgram-raw", key, "json")
	if r.Store.CanRead("transcribe") && r.Store.Exists(rawPath) {
		raw, err := os.ReadFile(rawPath)
		if err != nil {
			return Artifact{}, nil, err
		}
		var response deepgram.Response
		if err := json.Unmarshal(raw, &response); err != nil {
			return Artifact{}, nil, err
		}
		t := deepgram.Normalize(response, r.Opts.Deepgram)
		if err := r.Store.WriteJSON(path, t); err != nil {
			return Artifact{}, nil, err
		}
		r.report(StageTranscribe, "rebuilt normalized transcript from raw cache")
		artifact := Artifact{Kind: "transcript", Key: key, Path: path, FromCache: true}
		r.transcriptMemo = &memoTranscript{MediaPath: absMediaPath, Artifact: artifact, Data: &t}
		return artifact, &t, nil
	}

	audioArtifact, err := r.ensureAudio(ctx, audioIdentity)
	if err != nil {
		return Artifact{}, nil, err
	}
	r.report(StageTranscribe, "calling Deepgram")
	client := deepgram.Client{}
	contentType := media.AudioContentType(r.Opts.Audio.Format)
	t, raw, err := client.TranscribeFile(ctx, audioArtifact.Path, contentType, r.Opts.Deepgram)
	if err != nil {
		return Artifact{}, nil, err
	}
	if err := r.Store.WriteFile(rawPath, raw, 0o644); err != nil {
		return Artifact{}, nil, err
	}
	if err := r.Store.WriteJSON(path, t); err != nil {
		return Artifact{}, nil, err
	}
	artifact := Artifact{Kind: "transcript", Key: key, Path: path}
	r.transcriptMemo = &memoTranscript{MediaPath: absMediaPath, Artifact: artifact, Data: t}
	return artifact, t, nil
}

func (r *Runner) EnsureCues(ctx context.Context, mediaPath string) (Artifact, subtitle.CueSet, error) {
	absMediaPath, err := filepath.Abs(mediaPath)
	if err != nil {
		return Artifact{}, subtitle.CueSet{}, err
	}
	if r.cuesMemo != nil && r.cuesMemo.MediaPath == absMediaPath {
		return r.cuesMemo.Artifact, r.cuesMemo.Data, nil
	}
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

	path := r.Store.Path("cues", key, "json")
	if r.Store.CanRead("cues") && r.Store.Exists(path) {
		var cues subtitle.CueSet
		if err := r.Store.ReadJSON(path, &cues); err != nil {
			return Artifact{}, subtitle.CueSet{}, err
		}
		r.report(StageCues, "cache hit %s", path)
		artifact := Artifact{Kind: "cues", Key: key, Path: path, FromCache: true}
		r.cuesMemo = &memoCues{MediaPath: absMediaPath, Artifact: artifact, Data: cues}
		return artifact, cues, nil
	}

	unlock := r.Store.LockPath(path)
	defer unlock()
	if r.Store.CanRead("cues") && r.Store.Exists(path) {
		var cues subtitle.CueSet
		if err := r.Store.ReadJSON(path, &cues); err != nil {
			return Artifact{}, subtitle.CueSet{}, err
		}
		r.report(StageCues, "cache hit %s", path)
		artifact := Artifact{Kind: "cues", Key: key, Path: path, FromCache: true}
		r.cuesMemo = &memoCues{MediaPath: absMediaPath, Artifact: artifact, Data: cues}
		return artifact, cues, nil
	}

	r.report(StageCues, "building subtitle cues")
	cues := subtitle.BuildCues(*t, r.Opts.Cues)
	if err := r.Store.WriteJSON(path, cues); err != nil {
		return Artifact{}, subtitle.CueSet{}, err
	}
	artifact := Artifact{Kind: "cues", Key: key, Path: path}
	r.cuesMemo = &memoCues{MediaPath: absMediaPath, Artifact: artifact, Data: cues}
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
	if !(r.Store.CanRead("render") && r.Store.Exists(cachePath)) {
		unlock := r.Store.LockPath(cachePath)
		if !(r.Store.CanRead("render") && r.Store.Exists(cachePath)) {
			r.report(StageRender, "writing %s subtitle", format)
			rendered, err := subtitle.Render(cues, format)
			if err != nil {
				unlock()
				return Artifact{}, "", false, err
			}
			if err := r.Store.WriteFile(cachePath, []byte(rendered), 0o644); err != nil {
				unlock()
				return Artifact{}, "", false, err
			}
		} else {
			r.report(StageRender, "cache hit %s", cachePath)
		}
		unlock()
	} else {
		r.report(StageRender, "cache hit %s", cachePath)
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
	if !(r.Store.CanRead("render") && r.Store.Exists(cachePath)) {
		unlock := r.Store.LockPath(cachePath)
		if !(r.Store.CanRead("render") && r.Store.Exists(cachePath)) {
			if format != "txt" && format != "md" {
				unlock()
				return Artifact{}, "", false, fmt.Errorf("unsupported script format %q", format)
			}
			r.report(StageRender, "writing %s script", format)
			content := strings.TrimSpace(t.Text) + "\n"
			if err := r.Store.WriteFile(cachePath, []byte(content), 0o644); err != nil {
				unlock()
				return Artifact{}, "", false, err
			}
		} else {
			r.report(StageRender, "cache hit %s", cachePath)
		}
		unlock()
	} else {
		r.report(StageRender, "cache hit %s", cachePath)
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
	if !(r.Store.CanRead("render") && r.Store.Exists(cachePath)) {
		unlock := r.Store.LockPath(cachePath)
		if !(r.Store.CanRead("render") && r.Store.Exists(cachePath)) {
			r.report(StageRender, "writing words json")
			if err := r.Store.WriteJSON(cachePath, t.Words); err != nil {
				unlock()
				return Artifact{}, "", false, err
			}
		} else {
			r.report(StageRender, "cache hit %s", cachePath)
		}
		unlock()
	} else {
		r.report(StageRender, "cache hit %s", cachePath)
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

func (r *Runner) audioArtifactPath(key string, ext string) (string, bool, error) {
	if r.persistAudio() {
		return r.Store.Path("audio", key, ext), true, nil
	}
	tempDir, err := r.ensureTempDir()
	if err != nil {
		return "", false, err
	}
	return filepath.Join(tempDir, "audio", artifactFileName(key, ext)), false, nil
}

func (r *Runner) persistAudio() bool {
	return r.Opts.Cache.CacheAudio && r.Store.CanWrite()
}

func (r *Runner) ensureTempDir() (string, error) {
	if r.tempDir != "" {
		return r.tempDir, nil
	}
	dir, err := os.MkdirTemp("", "subkit-audio-*")
	if err != nil {
		return "", fmt.Errorf("creating temporary audio dir: %w", err)
	}
	r.tempDir = dir
	return dir, nil
}

func artifactFileName(key string, ext string) string {
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return key + ext
}

func (r *Runner) log(format string, args ...any) {
	r.report("", format, args...)
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
