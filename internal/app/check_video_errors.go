package app

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andrerfcsantos/subkit-codex/internal/media"
	"github.com/andrerfcsantos/subkit-codex/internal/pipeline"
	"github.com/spf13/cobra"
)

const defaultVideoExtensions = ".3gp,.avi,.flv,.m2ts,.m4v,.mkv,.mov,.mp4,.mpeg,.mpg,.mts,.ts,.vob,.webm,.wmv"

type checkVideoFlags struct {
	Options        media.VideoCheckOptions
	Mode           string
	Extensions     string
	JSONReport     string
	CSVReport      string
	Concurrency    int
	Progress       string
	NoLocate       bool
	Timeout        float64
	FullTimeout    float64
	AnalyzeSeconds float64
}

type indexedVideoResult struct {
	index  int
	result media.VideoCheckResult
	err    error
}

type videoChecker func(context.Context, string, media.VideoCheckOptions, media.VideoCheckReporter) (media.VideoCheckResult, error)

type videoCheckBatchError struct {
	Problematic int
}

func (e videoCheckBatchError) Error() string {
	if e.Problematic == 1 {
		return "1 video has errors or could not be checked"
	}
	return fmt.Sprintf("%d videos have errors or could not be checked", e.Problematic)
}

func newCheckVideoErrorsCommand() *cobra.Command {
	defaults := media.DefaultVideoCheckOptions()
	flags := checkVideoFlags{
		Options:        defaults,
		Mode:           string(defaults.Mode),
		Extensions:     defaultVideoExtensions,
		Concurrency:    defaultConcurrency,
		Progress:       progressAuto,
		Timeout:        defaults.Timeout.Seconds(),
		FullTimeout:    defaults.FullTimeout.Seconds(),
		AnalyzeSeconds: defaults.AnalyzeDuration.Seconds(),
	}

	cmd := &cobra.Command{
		Use:    "check-video-errors <file-or-directory-or-glob> [more inputs...]",
		Short:  "Check video files for decoding errors",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := prepareCheckVideoFlags(&flags); err != nil {
				return err
			}
			extensions := parseVideoExtensions(flags.Extensions)
			inputs, err := resolveVideoCheckInputs(args, extensions)
			if err != nil {
				return err
			}
			return runVideoChecks(cmd.Context(), cmd.OutOrStdout(), flags, inputs)
		},
	}

	cmd.Flags().StringVar(&flags.Mode, "mode", flags.Mode, "scan mode: quick or full")
	cmd.Flags().IntVar(&flags.Options.Samples, "samples", flags.Options.Samples, "interior one-frame samples after a clean tail; 0 checks only the tail")
	cmd.Flags().Float64Var(&flags.Options.TailSeconds, "tail-seconds", flags.Options.TailSeconds, "seconds to decode at the end of each video")
	cmd.Flags().BoolVar(&flags.Options.Locate, "locate", flags.Options.Locate, "locate a persistent-to-EOF failure")
	cmd.Flags().BoolVar(&flags.NoLocate, "no-locate", false, "disable persistent failure localization")
	cmd.Flags().Float64Var(&flags.Options.Resolution, "resolution", flags.Options.Resolution, "failure localization resolution in seconds")
	cmd.Flags().IntVarP(&flags.Concurrency, "concurrency", "j", flags.Concurrency, "maximum number of videos to check concurrently")
	cmd.Flags().IntVar(&flags.Concurrency, "jobs", flags.Concurrency, "alias for --concurrency")
	_ = cmd.Flags().MarkHidden("jobs")
	cmd.Flags().StringVar(&flags.Progress, "progress", flags.Progress, "progress display: auto, tui, plain, or off")
	cmd.Flags().Float64Var(&flags.Timeout, "timeout", flags.Timeout, "seconds allowed for each quick probe")
	cmd.Flags().Float64Var(&flags.FullTimeout, "timeout-full", flags.FullTimeout, "seconds allowed per video in full mode")
	cmd.Flags().IntVar(&flags.Options.ProbeSizeMiB, "probe-size-mib", flags.Options.ProbeSizeMiB, "maximum FFprobe probe size in MiB")
	cmd.Flags().Float64Var(&flags.AnalyzeSeconds, "analyze-seconds", flags.AnalyzeSeconds, "FFprobe analysis duration in seconds")
	cmd.Flags().StringVar(&flags.Extensions, "extensions", flags.Extensions, "comma-separated video file extensions")
	cmd.Flags().StringVar(&flags.JSONReport, "json", "", "write a detailed JSON report")
	cmd.Flags().StringVar(&flags.CSVReport, "csv", "", "write a summary CSV report")
	cmd.Flags().StringVar(&flags.Options.FFmpegPath, "ffmpeg", flags.Options.FFmpegPath, "FFmpeg executable path")
	cmd.Flags().StringVar(&flags.Options.FFprobePath, "ffprobe", flags.Options.FFprobePath, "FFprobe executable path")
	return cmd
}

func prepareCheckVideoFlags(flags *checkVideoFlags) error {
	flags.Options.Mode = media.VideoCheckMode(strings.ToLower(strings.TrimSpace(flags.Mode)))
	if flags.Options.Mode != media.VideoCheckQuick && flags.Options.Mode != media.VideoCheckFull {
		return fmt.Errorf("--mode must be quick or full")
	}
	if flags.Options.Samples < 0 {
		return fmt.Errorf("--samples must be at least 0")
	}
	if flags.Options.TailSeconds <= 0 || flags.Options.Resolution <= 0 {
		return fmt.Errorf("--tail-seconds and --resolution must be greater than 0")
	}
	if flags.Concurrency < 1 {
		return fmt.Errorf("--concurrency must be at least 1")
	}
	flags.Progress = strings.ToLower(strings.TrimSpace(flags.Progress))
	if flags.Progress != progressAuto && flags.Progress != progressTUI && flags.Progress != progressPlain && flags.Progress != progressOff {
		return fmt.Errorf("--progress must be one of auto, tui, plain, or off")
	}
	if flags.Timeout <= 0 || flags.FullTimeout <= 0 || flags.AnalyzeSeconds <= 0 {
		return fmt.Errorf("timeouts and --analyze-seconds must be greater than 0")
	}
	if flags.Options.ProbeSizeMiB <= 0 {
		return fmt.Errorf("--probe-size-mib must be greater than 0")
	}
	flags.Options.Timeout = time.Duration(flags.Timeout * float64(time.Second))
	flags.Options.FullTimeout = time.Duration(flags.FullTimeout * float64(time.Second))
	flags.Options.AnalyzeDuration = time.Duration(flags.AnalyzeSeconds * float64(time.Second))
	if flags.NoLocate {
		flags.Options.Locate = false
	}
	if len(parseVideoExtensions(flags.Extensions)) == 0 {
		return fmt.Errorf("--extensions must contain at least one extension")
	}
	return nil
}

func parseVideoExtensions(raw string) map[string]bool {
	extensions := map[string]bool{}
	for _, value := range strings.Split(raw, ",") {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if !strings.HasPrefix(value, ".") {
			value = "." + value
		}
		extensions[value] = true
	}
	return extensions
}

func resolveVideoCheckInputs(args []string, extensions map[string]bool) ([]string, error) {
	seen := map[string]bool{}
	var inputs []string
	addFile := func(path string) error {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !extensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		key := pathKey(absolute)
		if !seen[key] {
			seen[key] = true
			inputs = append(inputs, absolute)
		}
		return nil
	}

	for _, arg := range args {
		info, err := os.Stat(arg)
		switch {
		case err == nil && info.IsDir():
			err = filepath.WalkDir(arg, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.Type().IsRegular() {
					return addFile(path)
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("walking input directory %q: %w", arg, err)
			}
		case err == nil:
			if err := addFile(arg); err != nil {
				return nil, fmt.Errorf("input %q: %w", arg, err)
			}
		case hasGlobMeta(arg):
			matches, err := expandVideoGlob(arg)
			if err != nil {
				return nil, fmt.Errorf("glob %q: %w", arg, err)
			}
			matched := 0
			for _, match := range matches {
				info, statErr := os.Stat(match)
				if statErr != nil {
					return nil, fmt.Errorf("glob match %q: %w", match, statErr)
				}
				if info.Mode().IsRegular() && extensions[strings.ToLower(filepath.Ext(match))] {
					matched++
				}
				if err := addFile(match); err != nil {
					return nil, fmt.Errorf("glob match %q: %w", match, err)
				}
			}
			if matched == 0 {
				return nil, fmt.Errorf("glob %q did not match any configured video files", arg)
			}
		default:
			return nil, fmt.Errorf("input %q: %w", arg, err)
		}
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("no matching video files found")
	}
	sort.Slice(inputs, func(i, j int) bool { return strings.ToLower(inputs[i]) < strings.ToLower(inputs[j]) })
	return inputs, nil
}

func expandVideoGlob(pattern string) ([]string, error) {
	if !strings.Contains(pattern, "**") {
		return filepath.Glob(pattern)
	}
	absolutePattern, err := filepath.Abs(pattern)
	if err != nil {
		return nil, err
	}
	root, err := globWalkRoot(absolutePattern)
	if err != nil {
		return nil, err
	}
	matcher, err := compileGlobstar(filepath.ToSlash(absolutePattern))
	if err != nil {
		return nil, err
	}
	var matches []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() {
			absolute, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			if matcher.MatchString(filepath.ToSlash(absolute)) {
				matches = append(matches, absolute)
			}
		}
		return nil
	})
	return matches, err
}

func globWalkRoot(absolutePattern string) (string, error) {
	slashed := filepath.ToSlash(absolutePattern)
	meta := strings.IndexAny(slashed, "*?[")
	if meta < 0 {
		return "", fmt.Errorf("pattern has no glob metacharacters")
	}
	prefix := slashed[:meta]
	separator := strings.LastIndex(prefix, "/")
	if separator < 0 {
		return ".", nil
	}
	root := prefix[:separator]
	if root == "" {
		root = "/"
	}
	if len(root) == 2 && root[1] == ':' {
		root += "/"
	}
	return filepath.FromSlash(root), nil
}

func compileGlobstar(pattern string) (*regexp.Regexp, error) {
	runes := []rune(pattern)
	var expression strings.Builder
	if runtime.GOOS == "windows" {
		expression.WriteString("(?i)")
	}
	expression.WriteByte('^')
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '*':
			if i+1 < len(runes) && runes[i+1] == '*' {
				for i+1 < len(runes) && runes[i+1] == '*' {
					i++
				}
				if i+1 < len(runes) && runes[i+1] == '/' {
					i++
					expression.WriteString("(?:.*/)?")
				} else {
					expression.WriteString(".*")
				}
			} else {
				expression.WriteString("[^/]*")
			}
		case '?':
			expression.WriteString("[^/]")
		case '[':
			end := i + 1
			for end < len(runes) && runes[end] != ']' {
				end++
			}
			if end == len(runes) {
				return nil, fmt.Errorf("unterminated character class")
			}
			class := string(runes[i+1 : end])
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			expression.WriteByte('[')
			expression.WriteString(class)
			expression.WriteByte(']')
			i = end
		default:
			expression.WriteString(regexp.QuoteMeta(string(runes[i])))
		}
	}
	expression.WriteByte('$')
	return regexp.Compile(expression.String())
}

func runVideoChecks(ctx context.Context, out io.Writer, flags checkVideoFlags, inputs []string) error {
	return runVideoChecksWithChecker(ctx, out, flags, inputs, media.CheckVideo)
}

func runVideoChecksWithChecker(ctx context.Context, out io.Writer, flags checkVideoFlags, inputs []string, check videoChecker) error {
	jobs := make([]batchJob, len(inputs))
	for i, input := range inputs {
		jobs[i] = batchJob{Input: input}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reporter := newBatchReporter(out, flags.Progress, jobs, flags.Concurrency, cancel)
	for _, job := range jobs {
		reporter.Report(batchEvent{Input: job.Input, Stage: pipeline.StageQueued, Message: "queued"})
	}

	workerCount := min(flags.Concurrency, len(inputs))
	type indexedInput struct {
		index int
		path  string
	}
	jobCh := make(chan indexedInput)
	resultCh := make(chan indexedVideoResult, len(inputs))
	var workers sync.WaitGroup
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobCh {
				if ctx.Err() != nil {
					return
				}
				result, err := check(ctx, job.path, flags.Options, func(event media.VideoCheckEvent) {
					reporter.Report(batchEvent{Input: job.path, Stage: pipeline.Stage(event.Stage), Message: event.Message})
				})
				if err != nil {
					reporter.Report(batchEvent{Input: job.path, Stage: pipeline.StageFailed, Message: err.Error(), Err: err})
				} else if result.Valid() {
					reporter.Report(batchEvent{Input: job.path, Stage: pipeline.StageDone, Message: "done", Detail: strings.ToLower(string(result.Status))})
				} else {
					detail := videoResultSummary(result)
					reporter.Report(batchEvent{Input: job.path, Stage: pipeline.StageFailed, Message: detail, Err: fmt.Errorf("%s", detail)})
				}
				resultCh <- indexedVideoResult{index: job.index, result: result, err: err}
			}
		}()
	}
	go func() {
		defer close(jobCh)
		for i, input := range inputs {
			select {
			case <-ctx.Done():
				return
			case jobCh <- indexedInput{index: i, path: input}:
			}
		}
	}()
	go func() {
		workers.Wait()
		close(resultCh)
	}()

	results := make([]media.VideoCheckResult, len(inputs))
	completed := make([]bool, len(inputs))
	for item := range resultCh {
		results[item.index] = item.result
		if item.err != nil && results[item.index].Path == "" {
			results[item.index] = media.VideoCheckResult{Path: inputs[item.index], Status: media.VideoStatusInconclusive, Reason: item.err.Error()}
		}
		completed[item.index] = true
	}
	reporter.Close()
	if err := ctx.Err(); err != nil {
		return err
	}

	var finished []media.VideoCheckResult
	for i, result := range results {
		if completed[i] {
			finished = append(finished, result)
		}
	}
	for _, result := range finished {
		printVideoResult(out, result)
	}
	if flags.JSONReport != "" {
		if err := writeVideoJSONReport(flags.JSONReport, flags.Options.Mode, finished); err != nil {
			return err
		}
		fmt.Fprintf(out, "JSON report: %s\n", flags.JSONReport)
	}
	if flags.CSVReport != "" {
		if err := writeVideoCSVReport(flags.CSVReport, finished); err != nil {
			return err
		}
		fmt.Fprintf(out, "CSV report: %s\n", flags.CSVReport)
	}

	counts := map[media.VideoCheckStatus]int{}
	problematic := 0
	for _, result := range finished {
		counts[result.Status]++
		if !result.Valid() {
			problematic++
		}
	}
	fmt.Fprintln(out, formatVideoCounts(counts))
	if problematic > 0 {
		return videoCheckBatchError{Problematic: problematic}
	}
	return nil
}

func videoResultSummary(result media.VideoCheckResult) string {
	label := string(result.Status)
	if result.FailureEstimateSeconds != nil {
		label += " near " + media.FormatVideoTimestamp(*result.FailureEstimateSeconds)
	}
	if result.Reason != "" {
		label += ": " + result.Reason
	}
	return label
}

func printVideoResult(out io.Writer, result media.VideoCheckResult) {
	label := fmt.Sprintf("[%-12s] %s", result.Status, result.Path)
	if result.FailureEstimateSeconds != nil {
		label += " near " + media.FormatVideoTimestamp(*result.FailureEstimateSeconds)
	}
	fmt.Fprintln(out, label)
	if !result.Valid() {
		fmt.Fprintf(out, "               %s\n", result.Reason)
		if result.EstimateNote != "" {
			fmt.Fprintf(out, "               %s\n", result.EstimateNote)
		}
	}
}

func formatVideoCounts(counts map[media.VideoCheckStatus]int) string {
	statuses := []media.VideoCheckStatus{media.VideoStatusOK, media.VideoStatusLikelyOK, media.VideoStatusCorrupt, media.VideoStatusUnreadable, media.VideoStatusInconclusive}
	var parts []string
	for _, status := range statuses {
		if count := counts[status]; count > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", status, count))
		}
	}
	return "Summary: " + strings.Join(parts, ", ")
}

func writeVideoJSONReport(path string, mode media.VideoCheckMode, results []media.VideoCheckResult) error {
	payload := struct {
		GeneratedAt              string                   `json:"generated_at"`
		Mode                     media.VideoCheckMode     `json:"mode"`
		QuickScanIsProbabilistic bool                     `json:"quick_scan_is_probabilistic"`
		Results                  []media.VideoCheckResult `json:"results"`
	}{
		GeneratedAt:              time.Now().UTC().Format(time.RFC3339Nano),
		Mode:                     mode,
		QuickScanIsProbabilistic: mode == media.VideoCheckQuick,
		Results:                  results,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func writeVideoCSVReport(path string, results []media.VideoCheckResult) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := csv.NewWriter(file)
	defer writer.Flush()
	if err := writer.Write([]string{"status", "path", "size_bytes", "duration_seconds", "codec", "reason", "failure_estimate_seconds", "failure_estimate_time", "estimate_note", "checks_run", "elapsed_seconds"}); err != nil {
		return err
	}
	for _, result := range results {
		estimateSeconds, estimateTime := "", ""
		if result.FailureEstimateSeconds != nil {
			estimateSeconds = strconv.FormatFloat(*result.FailureEstimateSeconds, 'f', 6, 64)
			estimateTime = media.FormatVideoTimestamp(*result.FailureEstimateSeconds)
		}
		row := []string{
			string(result.Status), result.Path, strconv.FormatInt(result.SizeBytes, 10),
			strconv.FormatFloat(result.DurationSeconds, 'f', 6, 64), result.Codec, result.Reason,
			estimateSeconds, estimateTime, result.EstimateNote, strconv.Itoa(len(result.Checks)),
			strconv.FormatFloat(result.ElapsedSeconds, 'f', 3, 64),
		}
		if err := writer.Write(row); err != nil {
			return err
		}
	}
	return writer.Error()
}
