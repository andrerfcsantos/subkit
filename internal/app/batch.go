package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/andrerfcsantos/subkit-codex/internal/batch"
	"github.com/andrerfcsantos/subkit-codex/internal/cache"
	"github.com/andrerfcsantos/subkit-codex/internal/naming"
	"github.com/andrerfcsantos/subkit-codex/internal/pipeline"
	"github.com/spf13/cobra"
)

type batchFlags struct {
	Concurrency    int
	Progress       string
	OutputTemplate string
	FailFast       bool
}

type batchJob struct {
	Input   string
	Outputs []plannedOutput
}

type plannedOutput struct {
	Kind   string
	Format string
	Path   string
}

type outputResult struct {
	Path         string
	Copied       bool
	ArtifactOnly bool
	ForceWrote   bool
}

type fileResult struct {
	Input   string
	Outputs []outputResult
	Err     error
}

type fileFailure struct {
	Input string
	Err   error
}

type batchError struct {
	Failures []fileFailure
}

func (e batchError) Error() string {
	if len(e.Failures) == 1 {
		return "1 file failed"
	}
	return fmt.Sprintf("%d files failed", len(e.Failures))
}

type batchProcessor func(context.Context, batchJob, pipeline.Options, pipeline.Reporter) ([]outputResult, error)

// summarizeOutputs derives a short outcome description and whether the file was
// effectively skipped (all outputs already cached) from the results a worker
// produced. This is the structural signal for "processed vs cached", replacing
// any guessing from free-text stage messages.
func summarizeOutputs(outputs []outputResult) (detail string, cached bool) {
	if len(outputs) == 0 {
		return "done", false
	}

	written := 0
	artifactOnly := 0
	var writtenPath string
	for _, output := range outputs {
		switch {
		case output.Copied || output.ForceWrote:
			written++
			writtenPath = output.Path
		case output.ArtifactOnly:
			artifactOnly++
		}
	}

	switch {
	case written == 0 && artifactOnly == len(outputs):
		return "ready", true
	case written == 0:
		return "cached", true
	case written == 1 && len(outputs) == 1:
		return "wrote " + filepath.Base(writtenPath), false
	default:
		return fmt.Sprintf("wrote %d files", written), false
	}
}

func addBatchFlags(cmd *cobra.Command, flags *batchFlags) {
	cmd.Flags().IntVarP(&flags.Concurrency, "concurrency", "j", batch.DefaultConcurrency, "maximum number of files to process at once")
	cmd.Flags().StringVar(&flags.Progress, "progress", batch.ProgressAuto, "progress display: auto, tui, plain, or off")
	cmd.Flags().StringVar(&flags.OutputTemplate, "output-template", "", "output path template with tokens like {dir}, {base}, {kind}, and {format}")
	cmd.Flags().BoolVar(&flags.FailFast, "fail-fast", false, "cancel remaining batch work after the first file failure")
}

func resolveInputs(args []string) ([]string, error) {
	var inputs []string
	seen := map[string]bool{}
	for _, arg := range args {
		matches := []string{arg}
		if naming.HasGlobMeta(arg) {
			var err error
			matches, err = filepath.Glob(arg)
			if err != nil {
				return nil, fmt.Errorf("invalid glob %q: %w", arg, err)
			}
			if len(matches) == 0 {
				return nil, fmt.Errorf("glob %q did not match any files", arg)
			}
		}

		for _, match := range matches {
			info, err := os.Stat(match)
			if err != nil {
				return nil, fmt.Errorf("input %q: %w", match, err)
			}
			if info.IsDir() {
				return nil, fmt.Errorf("input %q is a directory", match)
			}

			abs, err := filepath.Abs(match)
			if err != nil {
				return nil, fmt.Errorf("resolving %q: %w", match, err)
			}
			key := naming.PathKey(abs)
			if seen[key] {
				continue
			}
			seen[key] = true
			inputs = append(inputs, match)
		}
	}
	return inputs, nil
}

func planSubtitleJobs(inputs []string, formats []string, outputDir string, outputTemplate string) ([]batchJob, error) {
	var jobs []batchJob
	for _, input := range inputs {
		job := batchJob{Input: input}
		for _, format := range formats {
			path, err := planOutputPath(input, "subtitle", format, outputDir, outputTemplate)
			if err != nil {
				return nil, err
			}
			job.Outputs = append(job.Outputs, plannedOutput{Kind: "subtitle", Format: format, Path: path})
		}
		jobs = append(jobs, job)
	}
	return jobs, preflightOutputCollisions(jobs)
}

func planRenderJobs(inputs []string, specs []outputSpec, outputDir string, outputTemplate string) ([]batchJob, error) {
	var jobs []batchJob
	for _, input := range inputs {
		job := batchJob{Input: input}
		for _, spec := range specs {
			kind := normalizeOutputKind(spec.Kind)
			path, err := planOutputPath(input, kind, spec.Format, outputDir, outputTemplate)
			if err != nil {
				return nil, err
			}
			job.Outputs = append(job.Outputs, plannedOutput{Kind: kind, Format: spec.Format, Path: path})
		}
		jobs = append(jobs, job)
	}
	return jobs, preflightOutputCollisions(jobs)
}

func planArtifactJobs(inputs []string, kind string, format string, outPath string, outputDir string, outputTemplate string) ([]batchJob, error) {
	if len(inputs) > 1 && outPath != "" {
		return nil, fmt.Errorf("--out cannot be used with multiple input files; use --output-dir or --output-template")
	}

	var jobs []batchJob
	for _, input := range inputs {
		path := outPath
		if path == "" {
			var err error
			path, err = planOutputPath(input, kind, format, outputDir, outputTemplate)
			if err != nil {
				return nil, err
			}
		}
		jobs = append(jobs, batchJob{
			Input:   input,
			Outputs: []plannedOutput{{Kind: kind, Format: format, Path: path}},
		})
	}
	return jobs, preflightOutputCollisions(jobs)
}

func runBatch(ctx context.Context, out io.Writer, opts pipeline.Options, flags batchFlags, jobs []batchJob) error {
	return runBatchWithProcessor(ctx, out, opts, flags, jobs, processBatchJob)
}

func runBatchWithProcessor(ctx context.Context, out io.Writer, opts pipeline.Options, flags batchFlags, jobs []batchJob, process batchProcessor) error {
	if out == nil {
		out = io.Discard
	}
	if flags.Concurrency == 0 {
		flags.Concurrency = batch.DefaultConcurrency
	}
	if flags.Concurrency < 1 {
		return fmt.Errorf("--concurrency must be at least 1")
	}
	flags.Progress = batch.NormalizeProgressMode(flags.Progress)
	if !batch.ValidProgressMode(flags.Progress) {
		return fmt.Errorf("--progress must be one of auto, tui, plain, or off")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	inputs := make([]string, len(jobs))
	for i, job := range jobs {
		inputs[i] = job.Input
	}
	reporter := batch.NewReporter(out, flags.Progress, inputs, flags.Concurrency, cancel)
	reporterClosed := false
	closeReporter := func() {
		if reporterClosed {
			return
		}
		reporter.Close()
		reporterClosed = true
	}
	defer closeReporter()
	for _, job := range jobs {
		reporter.Report(batch.Event{Input: job.Input, Stage: pipeline.StageQueued, Message: "queued"})
	}

	results, completed := batch.Run(ctx, jobs, flags.Concurrency, func(ctx context.Context, job batchJob) fileResult {
		jobReporter := pipeline.ReporterFunc(func(event pipeline.Event) {
			reporter.Report(batch.Event{Input: job.Input, Stage: event.Stage, Message: event.Message})
		})
		outputs, err := process(ctx, job, opts, jobReporter)
		if err != nil {
			reporter.Report(batch.Event{Input: job.Input, Stage: pipeline.StageFailed, Message: err.Error(), Err: err})
			if flags.FailFast {
				cancel()
			}
			return fileResult{Input: job.Input, Err: err}
		}
		detail, cached := summarizeOutputs(outputs)
		reporter.Report(batch.Event{Input: job.Input, Stage: pipeline.StageDone, Message: "done", Detail: detail, Cached: cached})
		return fileResult{Input: job.Input, Outputs: outputs}
	})

	closeReporter()

	var failures []fileFailure
	for i, result := range results {
		if !completed[i] {
			continue
		}
		if result.Err != nil {
			failures = append(failures, fileFailure{Input: result.Input, Err: result.Err})
			continue
		}
		for _, output := range result.Outputs {
			printOutputResult(out, result.Input, len(jobs) > 1, output)
		}
	}

	if len(failures) > 0 {
		printFailureSummary(out, failures)
		return batchError{Failures: failures}
	}
	return ctx.Err()
}

func processBatchJob(ctx context.Context, job batchJob, opts pipeline.Options, reporter pipeline.Reporter) ([]outputResult, error) {
	runner, err := pipeline.NewRunnerWithReporter(opts, reporter)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = runner.Close()
	}()

	var results []outputResult
	for _, output := range job.Outputs {
		result, err := processPlannedOutput(ctx, runner, job.Input, output, reporter)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

func processPlannedOutput(ctx context.Context, runner *pipeline.Runner, input string, output plannedOutput, reporter pipeline.Reporter) (outputResult, error) {
	switch output.Kind {
	case "subtitle":
		_, path, copied, err := runner.EnsureSubtitle(ctx, input, output.Format, output.Path)
		return outputResult{Path: path, Copied: copied}, err
	case "script":
		_, path, copied, err := runner.EnsureScript(ctx, input, output.Format, output.Path)
		return outputResult{Path: path, Copied: copied}, err
	case "words":
		if output.Format != "json" {
			return outputResult{}, fmt.Errorf("words only supports json output for now")
		}
		_, path, copied, err := runner.EnsureWords(ctx, input, output.Path)
		return outputResult{Path: path, Copied: copied}, err
	case "audio":
		artifact, err := runner.EnsureAudio(ctx, input)
		if err != nil {
			return outputResult{}, err
		}
		return copyArtifactOutput(artifact.Path, output.Path, reporter, output.Path != "")
	case "transcript":
		artifact, _, err := runner.EnsureTranscript(ctx, input)
		if err != nil {
			return outputResult{}, err
		}
		return copyArtifactOutput(artifact.Path, output.Path, reporter, output.Path != "")
	case "cues":
		artifact, _, err := runner.EnsureCues(ctx, input)
		if err != nil {
			return outputResult{}, err
		}
		return copyArtifactOutput(artifact.Path, output.Path, reporter, output.Path != "")
	default:
		return outputResult{}, fmt.Errorf("unsupported output kind %q", output.Kind)
	}
}

func copyArtifactOutput(cachePath string, outputPath string, reporter pipeline.Reporter, explicitOutput bool) (outputResult, error) {
	if outputPath == "" {
		return outputResult{Path: cachePath, ArtifactOnly: true}, nil
	}
	copied, err := cache.CopyFileIfDifferent(cachePath, outputPath)
	if err != nil {
		return outputResult{}, err
	}
	if reporter != nil {
		if copied || explicitOutput {
			reporter.Report(pipeline.Event{Stage: pipeline.StageWrite, Message: fmt.Sprintf("wrote %s", outputPath)})
		} else {
			reporter.Report(pipeline.Event{Stage: pipeline.StageWrite, Message: fmt.Sprintf("cached %s", outputPath)})
		}
	}
	return outputResult{Path: outputPath, Copied: copied, ForceWrote: explicitOutput}, nil
}

func printOutputResult(out io.Writer, input string, prefix bool, result outputResult) {
	line := result.Path
	if !result.ArtifactOnly {
		if result.Copied || result.ForceWrote {
			line = "wrote " + result.Path
		} else {
			line = "cached " + result.Path
		}
	}
	if prefix {
		fmt.Fprintf(out, "[%s] %s\n", filepath.Base(input), line)
		return
	}
	fmt.Fprintln(out, line)
}

func printFailureSummary(out io.Writer, failures []fileFailure) {
	fmt.Fprintln(out, "errors:")
	for _, failure := range failures {
		fmt.Fprintf(out, "- %s: %v\n", failure.Input, failure.Err)
	}
}

func planOutputPath(input string, kind string, format string, outputDir string, outputTemplate string) (string, error) {
	kind = normalizeOutputKind(kind)
	format = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(format), "."))
	if outputTemplate != "" {
		return renderOutputTemplate(outputTemplate, input, kind, format), nil
	}
	if outputDir != "" {
		if kind == "subtitle" {
			return naming.InDir(input, outputDir, "", format), nil
		}
		return naming.InDir(input, outputDir, kind, format), nil
	}

	switch kind {
	case "subtitle":
		return naming.ReplaceExt(input, format), nil
	case "script", "words":
		return naming.Sibling(input, kind, format), nil
	default:
		return "", nil
	}
}

func renderOutputTemplate(template string, input string, kind string, format string) string {
	ext := strings.TrimPrefix(filepath.Ext(input), ".")
	base := strings.TrimSuffix(filepath.Base(input), filepath.Ext(input))
	replacer := strings.NewReplacer(
		"{dir}", filepath.Dir(input),
		"{base}", base,
		"{input_ext}", ext,
		"{kind}", kind,
		"{format}", format,
	)
	return replacer.Replace(template)
}

func preflightOutputCollisions(jobs []batchJob) error {
	seen := map[string]string{}
	for _, job := range jobs {
		for _, output := range job.Outputs {
			if output.Path == "" {
				continue
			}
			abs, err := filepath.Abs(output.Path)
			if err != nil {
				return fmt.Errorf("resolving output %q: %w", output.Path, err)
			}
			key := naming.PathKey(abs)
			label := fmt.Sprintf("%s -> %s", job.Input, output.Path)
			if previous, ok := seen[key]; ok {
				return fmt.Errorf("output path collision: %s conflicts with %s", label, previous)
			}
			seen[key] = label
		}
	}
	return nil
}

func normalizeOutputKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "subtitles":
		return "subtitle"
	case "text":
		return "script"
	default:
		return strings.ToLower(strings.TrimSpace(kind))
	}
}
