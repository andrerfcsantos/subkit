package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/andrerfcsantos/subkit-codex/internal/cache"
	"github.com/andrerfcsantos/subkit-codex/internal/pipeline"
	"github.com/spf13/cobra"
)

const (
	defaultConcurrency = 4
	progressAuto       = "auto"
	progressTUI        = "tui"
	progressPlain      = "plain"
	progressOff        = "off"
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

type batchEvent struct {
	Input   string
	Stage   pipeline.Stage
	Message string
	Err     error
}

type batchReporter interface {
	Report(batchEvent)
	Close()
}

func addBatchFlags(cmd *cobra.Command, flags *batchFlags) {
	cmd.Flags().IntVarP(&flags.Concurrency, "concurrency", "j", defaultConcurrency, "maximum number of files to process at once")
	cmd.Flags().StringVar(&flags.Progress, "progress", progressAuto, "progress display: auto, tui, plain, or off")
	cmd.Flags().StringVar(&flags.OutputTemplate, "output-template", "", "output path template with tokens like {dir}, {base}, {kind}, and {format}")
	cmd.Flags().BoolVar(&flags.FailFast, "fail-fast", false, "cancel remaining batch work after the first file failure")
}

func resolveInputs(args []string) ([]string, error) {
	var inputs []string
	seen := map[string]bool{}
	for _, arg := range args {
		matches := []string{arg}
		if hasGlobMeta(arg) {
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
			key := pathKey(abs)
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
		flags.Concurrency = defaultConcurrency
	}
	if flags.Concurrency < 1 {
		return fmt.Errorf("--concurrency must be at least 1")
	}
	flags.Progress = strings.ToLower(strings.TrimSpace(flags.Progress))
	if flags.Progress == "" {
		flags.Progress = progressAuto
	}
	if flags.Progress != progressAuto && flags.Progress != progressTUI && flags.Progress != progressPlain && flags.Progress != progressOff {
		return fmt.Errorf("--progress must be one of auto, tui, plain, or off")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	reporter := newBatchReporter(out, flags.Progress, jobs, cancel)
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
		reporter.Report(batchEvent{Input: job.Input, Stage: pipeline.StageQueued, Message: "queued"})
	}

	workerCount := flags.Concurrency
	if workerCount > len(jobs) {
		workerCount = len(jobs)
	}
	if workerCount == 0 {
		return nil
	}

	jobCh := make(chan batchJob)
	resultCh := make(chan fileResult, len(jobs))
	var wg sync.WaitGroup

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				if ctx.Err() != nil {
					continue
				}
				jobReporter := pipeline.ReporterFunc(func(event pipeline.Event) {
					reporter.Report(batchEvent{Input: job.Input, Stage: event.Stage, Message: event.Message})
				})
				outputs, err := process(ctx, job, opts, jobReporter)
				if err != nil {
					reporter.Report(batchEvent{Input: job.Input, Stage: pipeline.StageFailed, Message: err.Error(), Err: err})
					resultCh <- fileResult{Input: job.Input, Err: err}
					if flags.FailFast {
						cancel()
					}
					continue
				}
				reporter.Report(batchEvent{Input: job.Input, Stage: pipeline.StageDone, Message: "done"})
				resultCh <- fileResult{Input: job.Input, Outputs: outputs}
			}
		}()
	}

	go func() {
		defer close(jobCh)
		for _, job := range jobs {
			select {
			case <-ctx.Done():
				return
			case jobCh <- job:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	var failures []fileFailure
	var successes []fileResult
	for result := range resultCh {
		if result.Err != nil {
			failures = append(failures, fileFailure{Input: result.Input, Err: result.Err})
			continue
		}
		successes = append(successes, result)
	}

	closeReporter()

	for _, result := range successes {
		for _, output := range result.Outputs {
			printOutputResult(out, result.Input, len(jobs) > 1, output)
		}
	}

	if len(failures) > 0 {
		printFailureSummary(out, failures)
		return batchError{Failures: failures}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func processBatchJob(ctx context.Context, job batchJob, opts pipeline.Options, reporter pipeline.Reporter) ([]outputResult, error) {
	runner, err := pipeline.NewRunnerWithReporter(opts, reporter)
	if err != nil {
		return nil, err
	}

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

func newBatchReporter(out io.Writer, mode string, jobs []batchJob, cancel context.CancelFunc) batchReporter {
	useTUI := false
	switch mode {
	case progressTUI:
		useTUI = true
	case progressAuto:
		useTUI = len(jobs) > 1 && isTerminal(out)
	}
	if useTUI {
		return newTUIBatchReporter(out, jobs, cancel)
	}
	if mode == progressOff {
		return noopBatchReporter{}
	}
	return &plainBatchReporter{out: out, prefix: len(jobs) > 1}
}

type noopBatchReporter struct{}

func (noopBatchReporter) Report(batchEvent) {}
func (noopBatchReporter) Close()            {}

type plainBatchReporter struct {
	out    io.Writer
	prefix bool
	mu     sync.Mutex
}

func (r *plainBatchReporter) Report(event batchEvent) {
	if event.Message == "" || event.Stage == pipeline.StageWrite {
		return
	}
	if !r.prefix && event.Stage == pipeline.StageQueued {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.prefix {
		fmt.Fprintf(r.out, "[%s] %s: %s\n", filepath.Base(event.Input), event.Stage, event.Message)
		return
	}
	fmt.Fprintf(r.out, "%s: %s\n", event.Stage, event.Message)
}

func (r *plainBatchReporter) Close() {}

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
			return outputInDir(input, outputDir, format), nil
		}
		return namedOutputInDir(input, outputDir, kind, format), nil
	}

	switch kind {
	case "subtitle":
		return replaceExt(input, format), nil
	case "script":
		return namedOutputPath(input, "script", format), nil
	case "words":
		return namedOutputPath(input, "words", format), nil
	case "audio", "transcript", "cues":
		return "", nil
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
			key := pathKey(abs)
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

func namedOutputPath(mediaPath string, suffix string, ext string) string {
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

func hasGlobMeta(value string) bool {
	return strings.ContainsAny(value, "*?[")
}

func pathKey(path string) string {
	return strings.ToLower(filepath.Clean(path))
}

func isTerminal(out io.Writer) bool {
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
