# subkit

Prototype Go CLI for generating subtitle and transcript artifacts from media files.

The implementation treats the work as an artifact pipeline:

```text
media file -> normalized audio -> normalized transcript -> cues -> rendered outputs
```

Each stage is cached independently. Re-running the same command with the same input and options should reuse cached artifacts, and if a final output file is missing it can be restored from the rendered artifact cache.

Audio artifacts are temporary by default because they can grow quickly. Transcripts, raw provider responses, cues, and rendered outputs remain cached by default. Use `--cache-audio` when you want normalized audio artifacts to be stored and reused persistently.

## Commands

```bash
subkit subtitle <media-file> [media-file...]
subkit generate <media-file> [media-file...] --output subtitle:srt --output subtitle:vtt --output script:txt --output words:json

subkit extract-audio <media-file> [media-file...]
subkit transcribe <media-file> [media-file...]
subkit cues <media-file> [media-file...]
subkit render <media-file> [media-file...] --output subtitle:srt

subkit version [--verbose]

subkit cache path
subkit cache list
subkit cache clean
```

Run locally with your own media file (sample media is not checked into the repository):

```bash
go run ./cmd/subkit subtitle ./your-video.mp4 --language pt-PT --model general --format srt --format vtt
```

Batch runs accept explicit files or globs. Inputs are processed concurrently with a default limit of four files at a time:

```bash
subkit generate "./data/*.mp4" --output subtitle:srt --output script:txt --output-dir ./out
subkit subtitle video-a.mp4 video-b.mp4 --format srt --format vtt --concurrency 2
```

Use `--output-template` when batch outputs need custom names. Supported tokens are `{dir}`, `{base}`, `{input_ext}`, `{kind}`, and `{format}`:

```bash
subkit generate "./data/*.mp4" --output subtitle:srt --output words:json --output-template "./out/{base}.{kind}.{format}"
```

Progress defaults to an interactive Bubble Tea view for multi-file terminal runs and plain logs when output is redirected. Override with `--progress auto|tui|plain|off`. Batch runs continue after per-file failures and print an error summary at the end; use `--fail-fast` to cancel remaining queued work after the first failure.

For `extract-audio`, `transcribe`, and `cues`, `--out` remains a single-input exact path. Use `--output-dir` or `--output-template` for batch copies.

## Defaults

Deepgram defaults:

```text
model:        nova-3
punctuate:    true
paragraphs:   true
smart_format: true
language:     en-US
diarize:      true
utterances:   true
```

Audio defaults:

```text
format:   flac
stream:   0
channels: 1
```

Subtitle cue defaults:

```text
algorithm:       deepgram
prefer segments: true
```

Numeric cue flags (`--subtitle-max-chars`, `--subtitle-max-lines`, `--subtitle-max-words`, `--subtitle-min-duration`, `--subtitle-max-duration`, `--subtitle-max-gap`, `--subtitle-reading-speed`) default to `0`, which means "use the selected algorithm's own defaults".

## Subtitle algorithms

Select the cue algorithm with `--subtitle-algorithm deepgram|netflix` (available on `subtitle`, `generate`, `cues`, and `render`). For `generate` and `render`, individual subtitle outputs can also override it inside the output spec:

```bash
subkit generate movie.mp4 --output subtitle:srt:algorithm=netflix
```

`deepgram` (default) is a port of [deepgram-go-captions](https://github.com/andrerfcsantos/deepgram-go-captions): utterances (or the flat word list) are chunked into fixed-size groups of 8 words (`--subtitle-max-words`), the flat word path also splits on speaker changes when diarization is on, and cue timing is exactly the first word's start to the last word's end. Speaker labels are emitted when the speaker changes between cues.

`netflix` follows the Netflix Timed Text Style Guide (general requirements, subtitle timing guidelines, and subtitle template guides):

```text
max chars per line: 42 (Latin scripts)
max lines:          2, favoring bottom-heavy line splits
min duration:       5/6s per event
max duration:       7s per event
reading speed:      17 chars/second (adult templates)
event gaps:         minimum 2 frames at 24fps; gaps under 0.5s are closed to 2 frames
out-times:          extended ~0.5s past the audio when the next event allows
```

Events are segmented sentence-first: cues never span speaker changes or silences longer than `--subtitle-max-gap` (default 1s), whole sentences are packed together while they fit, and oversized sentences are split at the best linguistic break point (after punctuation, before conjunctions and prepositions, never right after an article). Line breaks inside a cue use the same scoring, with English and Portuguese function-word lists; other languages fall back to punctuation and balance. Netflix-styled output carries no speaker labels, per the style guide. Frame-based rules assume 24fps, and shot-change alignment is out of scope since cue generation never inspects video frames.

## Cache controls

```bash
subkit subtitle movie.mp4 --refresh
subkit subtitle movie.mp4 --rerun transcribe
subkit subtitle movie.mp4 --rerun audio,transcribe
subkit subtitle movie.mp4 --no-cache
subkit subtitle movie.mp4 --cache-audio
subkit --cache-dir ./.subkit-cache subtitle movie.mp4
```

The default cache is based on Go's `os.UserCacheDir()`. On this Windows machine it resolves to:

```text
C:\Users\andre\AppData\Local\subkit
```

Cache keys include the source media hash, step options, pipeline version, and for audio extraction the ffmpeg version string.

By default, audio extraction writes a temporary run-local artifact for provider upload or explicit copies. `extract-audio` therefore needs `--out`, `--output-dir`, or `--output-template` unless `--cache-audio` is set.

## Transcript schema

The normalized transcript model is intentionally provider-neutral, but Deepgram-shaped enough to preserve useful data from the first implementation:

```text
provider, provider model/version
language and detected language
full transcript text
duration and channel count
words with text, punctuated text, start/end, confidence, speaker, channel, language
segments with type, text, start/end, confidence, speaker, channel, and word range
provider metadata
```

Those fields map reasonably well to common STT providers: Deepgram utterances/paragraphs, AssemblyAI words/utterances, Google/AWS alternatives and word timings, and Whisper-style segments.

## Releases

Releases are cut by pushing a `v*` tag, which runs GoReleaser through GitHub Actions. The release configuration depends on [GoReleaser Pro](https://goreleaser.com/pro/) features (MSI, DMG, and winget packaging), so reproducing a full release locally requires a Pro license and the `GORELEASER_KEY`, `WINGET_GITHUB_TOKEN` secrets. Plain `go build ./cmd/subkit` is enough for local development builds.

## Notes

This is a standalone CLI that uses ffmpeg, not an ffmpeg plugin. ffmpeg owns media extraction and future muxing/burn-in work; `subkit` owns provider calls, normalized artifacts, caching, cue generation, and multi-output rendering.
