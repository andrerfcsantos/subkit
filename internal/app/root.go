package app

import (
	"fmt"
	"os"
	"strings"

	"github.com/andrerfcsantos/subkit-codex/internal/cache"
	"github.com/andrerfcsantos/subkit-codex/internal/flagutil"
	"github.com/andrerfcsantos/subkit-codex/internal/media"
	"github.com/andrerfcsantos/subkit-codex/internal/pipeline"
	"github.com/andrerfcsantos/subkit-codex/internal/providers/deepgram"
	"github.com/andrerfcsantos/subkit-codex/internal/subtitle"
	"github.com/andrerfcsantos/subkit-codex/internal/videocheck"
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
	var batch batchFlags

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
	root.PersistentFlags().BoolVar(&opts.Cache.CacheAudio, "cache-audio", false, "read and write persistent normalized audio artifacts")

	subtitleCmd := &cobra.Command{
		Use:   "subtitle <media-file> [media-file...]",
		Short: "Generate subtitle files for media files",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inputs, err := resolveInputs(args)
			if err != nil {
				return err
			}
			selectedFormats := flagutil.SplitCSV(formats)
			if len(selectedFormats) == 0 {
				selectedFormats = []string{"srt"}
			}
			jobs, err := planSubtitleJobs(inputs, selectedFormats, outputDir, batch.OutputTemplate)
			if err != nil {
				return err
			}
			return runBatch(cmd.Context(), cmd.OutOrStdout(), opts, batch, jobs)
		},
	}
	addPipelineFlags(subtitleCmd, &opts)
	addBatchFlags(subtitleCmd, &batch)
	subtitleCmd.Flags().StringArrayVarP(&formats, "format", "f", []string{"srt"}, "subtitle format; repeat or comma-separate values: srt,vtt")
	subtitleCmd.Flags().StringVar(&outputDir, "output-dir", "", "directory for generated subtitle files")
	root.AddCommand(subtitleCmd)

	generateCmd := &cobra.Command{
		Use:   "generate <media-file> [media-file...]",
		Short: "Generate one or more outputs from media files",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inputs, err := resolveInputs(args)
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
			jobs, err := planRenderJobs(inputs, specs, outputDir, batch.OutputTemplate)
			if err != nil {
				return err
			}
			return runBatch(cmd.Context(), cmd.OutOrStdout(), opts, batch, jobs)
		},
	}
	addPipelineFlags(generateCmd, &opts)
	addBatchFlags(generateCmd, &batch)
	generateCmd.Flags().StringArrayVarP(&outputs, "output", "o", nil, "output spec; repeat or comma-separate values like subtitle:srt,script:txt,words:json")
	generateCmd.Flags().StringVar(&outputDir, "output-dir", "", "directory for generated output files")
	root.AddCommand(generateCmd)

	extractCmd := &cobra.Command{
		Use:   "extract-audio <media-file> [media-file...]",
		Short: "Extract normalized audio artifacts",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if outPath == "" && outputDir == "" && batch.OutputTemplate == "" && !(opts.Cache.CacheAudio && !opts.Cache.NoCache) {
				return fmt.Errorf("extract-audio requires --out, --output-dir, or --output-template unless persistent audio caching is enabled with --cache-audio")
			}
			inputs, err := resolveInputs(args)
			if err != nil {
				return err
			}
			jobs, err := planArtifactJobs(inputs, "audio", media.AudioExtension(opts.Audio.Format), outPath, outputDir, batch.OutputTemplate)
			if err != nil {
				return err
			}
			return runBatch(cmd.Context(), cmd.OutOrStdout(), opts, batch, jobs)
		},
	}
	addAudioFlags(extractCmd, &opts.Audio)
	addBatchFlags(extractCmd, &batch)
	extractCmd.Flags().StringVar(&outPath, "out", "", "copy artifact to this path")
	extractCmd.Flags().StringVar(&outputDir, "output-dir", "", "directory for copied audio artifacts")
	root.AddCommand(extractCmd)

	transcribeCmd := &cobra.Command{
		Use:   "transcribe <media-file> [media-file...]",
		Short: "Extract audio if needed, transcribe it, and cache normalized transcript JSON",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inputs, err := resolveInputs(args)
			if err != nil {
				return err
			}
			jobs, err := planArtifactJobs(inputs, "transcript", "json", outPath, outputDir, batch.OutputTemplate)
			if err != nil {
				return err
			}
			return runBatch(cmd.Context(), cmd.OutOrStdout(), opts, batch, jobs)
		},
	}
	addPipelineFlags(transcribeCmd, &opts)
	addBatchFlags(transcribeCmd, &batch)
	transcribeCmd.Flags().StringVar(&outPath, "out", "", "copy normalized transcript JSON to this path")
	transcribeCmd.Flags().StringVar(&outputDir, "output-dir", "", "directory for copied transcript artifacts")
	root.AddCommand(transcribeCmd)

	cuesCmd := &cobra.Command{
		Use:   "cues <media-file> [media-file...]",
		Short: "Build and cache subtitle cues from transcripts",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inputs, err := resolveInputs(args)
			if err != nil {
				return err
			}
			jobs, err := planArtifactJobs(inputs, "cues", "json", outPath, outputDir, batch.OutputTemplate)
			if err != nil {
				return err
			}
			return runBatch(cmd.Context(), cmd.OutOrStdout(), opts, batch, jobs)
		},
	}
	addPipelineFlags(cuesCmd, &opts)
	addBatchFlags(cuesCmd, &batch)
	cuesCmd.Flags().StringVar(&outPath, "out", "", "copy cue JSON to this path")
	cuesCmd.Flags().StringVar(&outputDir, "output-dir", "", "directory for copied cue artifacts")
	root.AddCommand(cuesCmd)

	renderCmd := &cobra.Command{
		Use:   "render <media-file> [media-file...]",
		Short: "Render subtitle/script/words outputs from cached or rebuilt artifacts",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inputs, err := resolveInputs(args)
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
			jobs, err := planRenderJobs(inputs, specs, outputDir, batch.OutputTemplate)
			if err != nil {
				return err
			}
			return runBatch(cmd.Context(), cmd.OutOrStdout(), opts, batch, jobs)
		},
	}
	addPipelineFlags(renderCmd, &opts)
	addBatchFlags(renderCmd, &batch)
	renderCmd.Flags().StringArrayVarP(&outputs, "output", "o", []string{"subtitle:srt"}, "output spec; repeat or comma-separate values like subtitle:srt,script:txt,words:json")
	renderCmd.Flags().StringVar(&outputDir, "output-dir", "", "directory for generated output files")
	root.AddCommand(renderCmd)

	root.AddCommand(versionCommand())
	root.AddCommand(cacheCommand(&opts))
	root.AddCommand(videocheck.Command())
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
	for _, value := range flagutil.SplitCSV(values) {
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
