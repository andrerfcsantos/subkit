# subkit

Prototype Go CLI for generating subtitle and transcript artifacts from media files.

The implementation treats the work as an artifact pipeline:

```text
media file -> normalized audio -> normalized transcript -> cues -> rendered outputs
```

Each stage is cached independently. Re-running the same command with the same input and options should reuse cached artifacts, and if a final output file is missing it can be restored from the rendered artifact cache.

## Commands

```bash
subkit subtitle <media-file>
subkit generate <media-file> --output subtitle:srt --output subtitle:vtt --output script:txt --output words:json

subkit extract-audio <media-file>
subkit transcribe <media-file>
subkit cues <media-file>
subkit render <media-file> --output subtitle:srt

subkit cache path
subkit cache list
subkit cache clean
```

Run locally with:

```bash
go run ./cmd/subkit subtitle ./data/saudemental.mp4 --language pt-PT --model general --format srt --format vtt
```

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
subkit --cache-dir ./.subkit-cache subtitle movie.mp4
```

The default cache is based on Go's `os.UserCacheDir()`. On this Windows machine it resolves to:

```text
C:\Users\andre\AppData\Local\subkit
```

Cache keys include the source media hash, step options, pipeline version, and for audio extraction the ffmpeg version string.

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
