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

Run locally with:

```bash
go run ./cmd/subkit subtitle ./data/saudemental.mp4 --language pt-PT --model general --format srt --format vtt
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

The default Deepgram model is `nova-2-video`, per the prototype spec. Deepgram rejected `nova-2-video` with `pt-PT` during verification, so the sample Portuguese video currently needs `--model general --language pt-PT`.

## Defaults

Deepgram defaults:

```text
model:        nova-2-video
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
algorithm:          readable
max chars per line: 42
max lines:          2
min duration:       0.8s
max duration:       6.0s
max gap:            0.9s
prefer segments:    true
```

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

## Notes

This is a standalone CLI that uses ffmpeg, not an ffmpeg plugin. ffmpeg owns media extraction and future muxing/burn-in work; `subkit` owns provider calls, normalized artifacts, caching, cue generation, and multi-output rendering.
