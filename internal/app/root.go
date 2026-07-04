package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/andrerfcsantos/subkit-codex/internal/cache"
	"github.com/andrerfcsantos/subkit-codex/internal/media"
	"github.com/andrerfcsantos/subkit-codex/internal/pipeline"
	"github.com/andrerfcsantos/subkit-codex/internal/providers/deepgram"
	"github.com/andrerfcsantos/subkit-codex/internal/subtitle"
	"github.com/spf13/cobra"
)

type outputSpec struct {
	Kind   string
	Format string
}

func NewRootCommand() *cobra.Command {
	opts := pipeline.DefaultOptions()
	var formats []string
	var outputs []string
	var outputDir string
	var outPath string

	root := &cobra.Command{
		Use:           "subkit",
		Short:         "Generate subtitles and transcript artifacts from media files",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&opts.Cache.Dir, "cache-dir", "", "cache directory")
	root.PersistentFlags().BoolVar(&opts.Cache.NoCache, "no-cache", false, "do not read or write the persistent cache")
	root.PersistentFlags().BoolVar(&opts.Cache.Refresh, "refresh", false, "ignore cache reads and rebuild artifacts")
	root.PersistentFlags().StringArrayVar(&opts.Cache.Rerun, "rerun", nil, "rerun selected steps: audio, transcribe, cues, render, or all")

	subtitleCmd := &cobra.Command{
		Use:   "subtitle <media-file>",
		Short: "Generate subtitle files for a media file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runner, err := pipeline.NewRunner(opts, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			mediaPath := args[0]
			selectedFormats := splitValues(formats)
			if len(selectedFormats) == 0 {
				selectedFormats = []string{"srt"}
			}
			for _, format := range selectedFormats {
				target := outputInDir(mediaPath, outputDir, format)
				_, path, copied, err := runner.EnsureSubtitle(cmd.Context(), mediaPath, format, target)
				if err != nil {
					return err
				}
				printResult(cmd, path, copied)
			}
			return nil
		},
	}
	addPipelineFlags(subtitleCmd, &opts)
	subtitleCmd.Flags().StringArrayVarP(&formats, "format", "f", []string{"srt"}, "subtitle format; repeat or comma-separate values: srt,vtt")
	subtitleCmd.Flags().StringVar(&outputDir, "output-dir", "", "directory for generated subtitle files")
	root.AddCommand(subtitleCmd)

	generateCmd := &cobra.Command{
		Use:   "generate <media-file>",
		Short: "Generate one or more outputs from a media file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runner, err := pipeline.NewRunner(opts, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			specs, err := parseOutputSpecs(outputs)
			if err != nil {
				return err
			}
			if len(specs) == 0 {
				specs = []outputSpec{{Kind: "subtitle", Format: "srt"}}
			}
			for _, spec := range specs {
				path, copied, err := runOutput(cmd.Context(), runner, args[0], outputDir, spec)
				if err != nil {
					return err
				}
				printResult(cmd, path, copied)
			}
			return nil
		},
	}
	addPipelineFlags(generateCmd, &opts)
	generateCmd.Flags().StringArrayVarP(&outputs, "output", "o", nil, "output spec; repeat or comma-separate values like subtitle:srt,script:txt,words:json")
	generateCmd.Flags().StringVar(&outputDir, "output-dir", "", "directory for generated output files")
	root.AddCommand(generateCmd)

	extractCmd := &cobra.Command{
		Use:   "extract-audio <media-file>",
		Short: "Extract and cache the normalized audio artifact",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runner, err := pipeline.NewRunner(opts, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			artifact, err := runner.EnsureAudio(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if outPath != "" {
				_, err := cache.CopyFileIfDifferent(artifact.Path, outPath)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", outPath)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", artifact.Path)
			return nil
		},
	}
	addAudioFlags(extractCmd, &opts.Audio)
	extractCmd.Flags().StringVar(&outPath, "out", "", "copy artifact to this path")
	root.AddCommand(extractCmd)

	transcribeCmd := &cobra.Command{
		Use:   "transcribe <media-file>",
		Short: "Extract audio if needed, transcribe it, and cache normalized transcript JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runner, err := pipeline.NewRunner(opts, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			artifact, _, err := runner.EnsureTranscript(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if outPath != "" {
				_, err := cache.CopyFileIfDifferent(artifact.Path, outPath)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", outPath)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", artifact.Path)
			return nil
		},
	}
	addPipelineFlags(transcribeCmd, &opts)
	transcribeCmd.Flags().StringVar(&outPath, "out", "", "copy normalized transcript JSON to this path")
	root.AddCommand(transcribeCmd)

	cuesCmd := &cobra.Command{
		Use:   "cues <media-file>",
		Short: "Build and cache subtitle cues from a transcript",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runner, err := pipeline.NewRunner(opts, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			artifact, _, err := runner.EnsureCues(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if outPath != "" {
				_, err := cache.CopyFileIfDifferent(artifact.Path, outPath)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", outPath)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", artifact.Path)
			return nil
		},
	}
	addPipelineFlags(cuesCmd, &opts)
	cuesCmd.Flags().StringVar(&outPath, "out", "", "copy cue JSON to this path")
	root.AddCommand(cuesCmd)

	renderCmd := &cobra.Command{
		Use:   "render <media-file>",
		Short: "Render a subtitle/script/words output from cached or rebuilt artifacts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runner, err := pipeline.NewRunner(opts, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			specs, err := parseOutputSpecs(outputs)
			if err != nil {
				return err
			}
			if len(specs) == 0 {
				specs = []outputSpec{{Kind: "subtitle", Format: "srt"}}
			}
			for _, spec := range specs {
				path, copied, err := runOutput(cmd.Context(), runner, args[0], outputDir, spec)
				if err != nil {
					return err
				}
				printResult(cmd, path, copied)
			}
			return nil
		},
	}
	addPipelineFlags(renderCmd, &opts)
	renderCmd.Flags().StringArrayVarP(&outputs, "output", "o", []string{"subtitle:srt"}, "output spec; repeat or comma-separate values like subtitle:srt,script:txt,words:json")
	renderCmd.Flags().StringVar(&outputDir, "output-dir", "", "directory for generated output files")
	root.AddCommand(renderCmd)

	root.AddCommand(cacheCommand(&opts))
	return root
}

func cacheCommand(opts *pipeline.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect and clean the artifact cache",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "Print the cache path",
		RunE: func(cmd *cobra.Command, args []string) error {
			runner, err := pipeline.NewRunner(*opts, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), runner.CacheRoot())
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "Print cache size and top-level artifact groups",
		RunE: func(cmd *cobra.Command, args []string) error {
			runner, err := pipeline.NewRunner(*opts, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			size, err := cache.DirSize(runner.CacheRoot())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s (%s)\n", runner.CacheRoot(), humanBytes(size))
			entries, err := os.ReadDir(runner.CacheRoot())
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			for _, entry := range entries {
				fmt.Fprintf(cmd.OutOrStdout(), "- %s\n", entry.Name())
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "clean",
		Short: "Delete the artifact cache",
		RunE: func(cmd *cobra.Command, args []string) error {
			runner, err := pipeline.NewRunner(*opts, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			if err := cache.RemoveAll(runner.CacheRoot()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", runner.CacheRoot())
			return nil
		},
	})
	return cmd
}

func addPipelineFlags(cmd *cobra.Command, opts *pipeline.Options) {
	addAudioFlags(cmd, &opts.Audio)
	addDeepgramFlags(cmd, &opts.Deepgram)
	addCueFlags(cmd, &opts.Cues)
}

func addAudioFlags(cmd *cobra.Command, opts *media.AudioOptions) {
	cmd.Flags().StringVar(&opts.Format, "audio-format", opts.Format, "intermediate audio format")
	cmd.Flags().IntVar(&opts.Stream, "audio-stream", opts.Stream, "zero-based input audio stream to extract")
	cmd.Flags().IntVar(&opts.Channels, "audio-channels", opts.Channels, "number of output audio channels")
	cmd.Flags().IntVar(&opts.SampleRate, "audio-sample-rate", opts.SampleRate, "output sample rate; 0 keeps ffmpeg default")
}

func addDeepgramFlags(cmd *cobra.Command, opts *deepgram.Options) {
	cmd.Flags().StringVar(&opts.Provider, "provider", opts.Provider, "transcription provider")
	cmd.Flags().StringVar(&opts.Model, "model", opts.Model, "transcription model")
	cmd.Flags().StringVar(&opts.Language, "language", opts.Language, "BCP-47 language hint")
	cmd.Flags().BoolVar(&opts.Punctuate, "punctuate", opts.Punctuate, "ask provider for punctuation and capitalization")
	cmd.Flags().BoolVar(&opts.Paragraphs, "paragraphs", opts.Paragraphs, "ask provider for paragraph segmentation")
	cmd.Flags().BoolVar(&opts.SmartFormat, "smart-format", opts.SmartFormat, "ask provider for smart formatting")
	cmd.Flags().BoolVar(&opts.Diarize, "diarize", opts.Diarize, "ask provider for speaker diarization")
	cmd.Flags().StringVar(&opts.DiarizeModel, "diarize-model", opts.DiarizeModel, "Deepgram diarization model version")
	cmd.Flags().BoolVar(&opts.Utterances, "utterances", opts.Utterances, "ask provider for utterance segmentation")
	cmd.Flags().StringVar(&opts.Endpoint, "deepgram-endpoint", opts.Endpoint, "Deepgram listen endpoint")
}

func addCueFlags(cmd *cobra.Command, opts *subtitle.CueOptions) {
	cmd.Flags().StringVar(&opts.Algorithm, "subtitle-algorithm", opts.Algorithm, "subtitle cue algorithm")
	cmd.Flags().IntVar(&opts.MaxCharsPerLine, "subtitle-max-chars", opts.MaxCharsPerLine, "max characters per subtitle line")
	cmd.Flags().IntVar(&opts.MaxLines, "subtitle-max-lines", opts.MaxLines, "max lines per subtitle cue")
	cmd.Flags().Float64Var(&opts.MinDuration, "subtitle-min-duration", opts.MinDuration, "minimum cue duration in seconds")
	cmd.Flags().Float64Var(&opts.MaxDuration, "subtitle-max-duration", opts.MaxDuration, "maximum cue duration in seconds")
	cmd.Flags().Float64Var(&opts.MaxGap, "subtitle-max-gap", opts.MaxGap, "pause length that forces a cue boundary")
	cmd.Flags().BoolVar(&opts.PreferSegments, "subtitle-prefer-segments", opts.PreferSegments, "prefer provider utterance segments when cueing")
}

func parseOutputSpecs(values []string) ([]outputSpec, error) {
	var specs []outputSpec
	for _, value := range splitValues(values) {
		kind, format, ok := strings.Cut(value, ":")
		if !ok {
			return nil, fmt.Errorf("output %q should look like kind:format", value)
		}
		kind = strings.ToLower(strings.TrimSpace(kind))
		format = strings.ToLower(strings.TrimSpace(format))
		if kind == "" || format == "" {
			return nil, fmt.Errorf("output %q should look like kind:format", value)
		}
		specs = append(specs, outputSpec{Kind: kind, Format: format})
	}
	return specs, nil
}

func splitValues(values []string) []string {
	var result []string
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				result = append(result, part)
			}
		}
	}
	return result
}

func runOutput(ctx context.Context, runner *pipeline.Runner, mediaPath string, outputDir string, spec outputSpec) (string, bool, error) {
	switch spec.Kind {
	case "subtitle", "subtitles":
		_, path, copied, err := runner.EnsureSubtitle(ctx, mediaPath, spec.Format, outputInDir(mediaPath, outputDir, spec.Format))
		return path, copied, err
	case "script", "text":
		_, path, copied, err := runner.EnsureScript(ctx, mediaPath, spec.Format, namedOutputInDir(mediaPath, outputDir, "script", spec.Format))
		return path, copied, err
	case "words":
		if spec.Format != "json" {
			return "", false, fmt.Errorf("words only supports json output for now")
		}
		_, path, copied, err := runner.EnsureWords(ctx, mediaPath, namedOutputInDir(mediaPath, outputDir, "words", "json"))
		return path, copied, err
	default:
		return "", false, fmt.Errorf("unsupported output kind %q", spec.Kind)
	}
}

func outputInDir(mediaPath string, outputDir string, ext string) string {
	if outputDir == "" {
		return ""
	}
	base := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))
	return filepath.Join(outputDir, base+"."+ext)
}

func namedOutputInDir(mediaPath string, outputDir string, suffix string, ext string) string {
	if outputDir == "" {
		return ""
	}
	base := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))
	return filepath.Join(outputDir, base+"."+suffix+"."+ext)
}

func printResult(cmd *cobra.Command, path string, copied bool) {
	if copied {
		fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)
		return
	}
	fmt.Fprintf(cmd.OutOrStdout(), "cached %s\n", path)
}

func humanBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}
