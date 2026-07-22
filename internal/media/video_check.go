package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type VideoCheckMode string

const (
	VideoCheckQuick VideoCheckMode = "quick"
	VideoCheckFull  VideoCheckMode = "full"
)

type VideoCheckStatus string

const (
	VideoStatusLikelyOK      VideoCheckStatus = "LIKELY_OK"
	VideoStatusOK            VideoCheckStatus = "OK"
	VideoStatusNonconformant VideoCheckStatus = "NONCONFORMANT"
	VideoStatusCorrupt       VideoCheckStatus = "CORRUPT"
	VideoStatusTruncated     VideoCheckStatus = "TRUNCATED"
	VideoStatusUnreadable    VideoCheckStatus = "UNREADABLE"
	VideoStatusInconclusive  VideoCheckStatus = "INCONCLUSIVE"
)

type VideoCheckOptions struct {
	Mode            VideoCheckMode
	Samples         int
	SampleSeconds   float64
	TailSeconds     float64
	Locate          bool
	StrictBitstream bool
	Resolution      float64
	Timeout         time.Duration
	FullTimeout     time.Duration
	ProbeSizeMiB    int
	AnalyzeDuration time.Duration
	FFmpegPath      string
	FFprobePath     string
}

func DefaultVideoCheckOptions() VideoCheckOptions {
	return VideoCheckOptions{
		Mode:            VideoCheckQuick,
		Samples:         3,
		SampleSeconds:   1,
		TailSeconds:     2,
		Locate:          true,
		Resolution:      0.25,
		Timeout:         120 * time.Second,
		FullTimeout:     24 * time.Hour,
		ProbeSizeMiB:    10,
		AnalyzeDuration: 10 * time.Second,
		FFmpegPath:      "ffmpeg",
		FFprobePath:     "ffprobe",
	}
}

type VideoCheckEvent struct {
	Stage   string
	Message string
}

type VideoCheckReporter func(VideoCheckEvent)

type VideoDecodeCheck struct {
	Kind              string   `json:"kind"`
	StartSeconds      float64  `json:"start_seconds"`
	RequestedSeconds  *float64 `json:"requested_seconds,omitempty"`
	LastOutputSeconds *float64 `json:"last_output_seconds,omitempty"`
	StrictBitstream   bool     `json:"strict_bitstream,omitempty"`
	State             string   `json:"state"`
	Frames            int      `json:"frames"`
	ElapsedSeconds    float64  `json:"elapsed_seconds"`
	Detail            string   `json:"detail,omitempty"`
}

type VideoCheckResult struct {
	Path                   string             `json:"path"`
	Status                 VideoCheckStatus   `json:"status"`
	Reason                 string             `json:"reason"`
	SizeBytes              int64              `json:"size_bytes,omitempty"`
	DurationSeconds        float64            `json:"duration_seconds,omitempty"`
	Codec                  string             `json:"codec,omitempty"`
	VideoStreamIndex       int                `json:"video_stream_index"`
	FailureEstimateSeconds *float64           `json:"failure_estimate_seconds,omitempty"`
	EstimateNote           string             `json:"estimate_note,omitempty"`
	Checks                 []VideoDecodeCheck `json:"checks,omitempty"`
	ElapsedSeconds         float64            `json:"elapsed_seconds"`
}

func (r VideoCheckResult) Valid() bool {
	return r.Status == VideoStatusOK || r.Status == VideoStatusLikelyOK || r.Status == VideoStatusNonconformant
}

func (r VideoCheckResult) TimedOut() bool {
	return slices.ContainsFunc(r.Checks, func(check VideoDecodeCheck) bool { return check.State == checkTimeout })
}

type videoMetadata struct {
	sizeBytes        int64
	durationSeconds  float64
	videoStreamIndex int
	codec            string
}

type toolResult struct {
	stdout   string
	stderr   string
	elapsed  time.Duration
	err      error
	timedOut bool
}

const (
	checkPassed        = "passed"
	checkFailed        = "failed"
	checkTimeout       = "timeout"
	checkInconclusive  = "inconclusive"
	checkNonconformant = "nonconformant"
	checkCancelled     = "cancelled"
)

var framePattern = regexp.MustCompile(`(?m)^frame=(\d+)\r?$`)
var outputTimePattern = regexp.MustCompile(`(?m)^out_time_us=(\d+)\r?$`)

func CheckVideo(ctx context.Context, path string, opts VideoCheckOptions, reporter VideoCheckReporter) (VideoCheckResult, error) {
	started := time.Now()
	result := VideoCheckResult{Path: path, Status: VideoStatusInconclusive, Reason: "scan did not complete"}
	finish := func() VideoCheckResult {
		result.ElapsedSeconds = time.Since(started).Seconds()
		return result
	}

	if err := validateVideoCheckOptions(&opts); err != nil {
		return finish(), err
	}
	reportVideoCheck(reporter, "metadata", "reading container metadata")
	metadata, reason, err := readVideoMetadata(ctx, path, opts)
	if err != nil {
		return finish(), err
	}
	if metadata == nil {
		result.Status = VideoStatusUnreadable
		result.Reason = reason
		return finish(), nil
	}
	result.SizeBytes = metadata.sizeBytes
	result.DurationSeconds = metadata.durationSeconds
	result.Codec = metadata.codec
	result.VideoStreamIndex = metadata.videoStreamIndex

	if opts.Mode == VideoCheckFull {
		reportVideoCheck(reporter, "full", "decoding the full primary video stream")
		check, err := runDecodeCheck(ctx, path, *metadata, opts, "full-decode", 0, nil, false, opts.FullTimeout)
		if err != nil {
			return finish(), err
		}
		result.Checks = append(result.Checks, check)
		switch check.State {
		case checkPassed:
			result.Status = VideoStatusOK
			result.Reason = "entire primary video stream decoded without a reported error"
		case checkNonconformant:
			result.Status = VideoStatusNonconformant
			result.Reason = check.Detail
		case checkFailed:
			result.Status = VideoStatusCorrupt
			result.Reason = check.Detail
			setProgressFailureEstimate(&result, check)
		default:
			result.Status = VideoStatusInconclusive
			result.Reason = check.Detail
		}
		return finish(), nil
	}

	tailSeconds := math.Min(opts.TailSeconds, metadata.durationSeconds)
	tailStart := math.Max(0, metadata.durationSeconds-tailSeconds)
	reportVideoCheck(reporter, "tail", fmt.Sprintf("decoding final %.2fs", tailSeconds))
	tail, err := runDecodeCheck(ctx, path, *metadata, opts, "tail", tailStart, &tailSeconds, false, opts.Timeout)
	if err != nil {
		return finish(), err
	}
	result.Checks = append(result.Checks, tail)
	if tail.State == checkFailed {
		result.Status = VideoStatusCorrupt
		result.Reason = tail.Detail
		if setProgressFailureEstimate(&result, tail) {
			return finish(), nil
		}
		if opts.Locate && metadata.durationSeconds > opts.Resolution {
			reportVideoCheck(reporter, "locate", "locating last decodable window")
			estimate, note, conclusive, err := locateLastDecodableBoundary(ctx, path, *metadata, opts, tailStart, &result.Checks, reporter)
			if err != nil {
				return finish(), err
			}
			result.FailureEstimateSeconds = estimate
			result.EstimateNote = note
			if !conclusive {
				result.Status = VideoStatusInconclusive
			}
		}
		return finish(), nil
	}
	if tail.State == checkInconclusive && tail.Frames == 0 {
		result.Reason = fmt.Sprintf("no video frame was produced in the declared final %.2fs", tailSeconds)
		if opts.Locate && metadata.durationSeconds > opts.Resolution {
			reportVideoCheck(reporter, "locate", "locating last decodable window after empty tail")
			estimate, note, conclusive, err := locateLastDecodableBoundary(ctx, path, *metadata, opts, tailStart, &result.Checks, reporter)
			if err != nil {
				return finish(), err
			}
			result.FailureEstimateSeconds = estimate
			result.EstimateNote = note
			if conclusive {
				result.Status = VideoStatusTruncated
				result.Reason += "; decodable video appears to end before the advertised duration"
				return finish(), nil
			}
		}
		result.Status = VideoStatusInconclusive
		return finish(), nil
	}
	if !decodeCheckPassed(tail) {
		result.Status = VideoStatusInconclusive
		result.Reason = tail.Detail
		return finish(), nil
	}
	hadStrictWarning := tail.State == checkNonconformant
	var strictWarnings []string
	if hadStrictWarning {
		strictWarnings = append(strictWarnings, tail.Detail)
	}

	points := videoSparsePoints(metadata.durationSeconds, tailStart, opts.Samples)
	for i, point := range points {
		reportVideoCheck(reporter, "sample", fmt.Sprintf("checking interior sample %d/%d", i+1, len(points)))
		sampleSeconds := math.Min(opts.SampleSeconds, math.Max(0, tailStart-point))
		check, err := runDecodeCheck(ctx, path, *metadata, opts, "interior-sample", point, &sampleSeconds, false, opts.Timeout)
		if err != nil {
			return finish(), err
		}
		result.Checks = append(result.Checks, check)
		if check.State == checkFailed {
			result.Status = VideoStatusCorrupt
			result.Reason = check.Detail
			if !setProgressFailureEstimate(&result, check) {
				estimate := point
				result.FailureEstimateSeconds = &estimate
				result.EstimateNote = "failure was detected in this sampled decode window; this is not necessarily the first damaged frame"
			}
			return finish(), nil
		}
		if !decodeCheckPassed(check) {
			result.Status = VideoStatusInconclusive
			result.Reason = check.Detail
			return finish(), nil
		}
		if check.State == checkNonconformant {
			hadStrictWarning = true
			strictWarnings = append(strictWarnings, check.Detail)
		}
	}

	if hadStrictWarning {
		result.Status = VideoStatusNonconformant
		result.Reason = strings.Join(strictWarnings, " | ")
		return finish(), nil
	}
	result.Status = VideoStatusLikelyOK
	result.Reason = fmt.Sprintf("tail and %d interior %.2fs sample(s) decoded cleanly; unsampled regions were not read", len(points), opts.SampleSeconds)
	return finish(), nil
}

func decodeCheckPassed(check VideoDecodeCheck) bool {
	return check.State == checkPassed || check.State == checkNonconformant
}

func setProgressFailureEstimate(result *VideoCheckResult, check VideoDecodeCheck) bool {
	if check.LastOutputSeconds == nil {
		return false
	}
	estimate := *check.LastOutputSeconds
	result.FailureEstimateSeconds = &estimate
	result.EstimateNote = "last successfully emitted video timestamp before FFmpeg stopped; the failing packet is at or shortly after this point"
	return true
}

func validateVideoCheckOptions(opts *VideoCheckOptions) error {
	if opts.Mode == "" {
		opts.Mode = VideoCheckQuick
	}
	if opts.Mode != VideoCheckQuick && opts.Mode != VideoCheckFull {
		return fmt.Errorf("video check mode must be quick or full")
	}
	if opts.Samples < 0 {
		return fmt.Errorf("video check samples must be at least 0")
	}
	if opts.SampleSeconds <= 0 || opts.TailSeconds <= 0 {
		return fmt.Errorf("video check sample and tail seconds must be greater than 0")
	}
	if opts.Resolution <= 0 {
		return fmt.Errorf("video check resolution must be greater than 0")
	}
	if opts.Timeout <= 0 || opts.FullTimeout <= 0 {
		return fmt.Errorf("video check timeouts must be greater than 0")
	}
	if opts.ProbeSizeMiB <= 0 || opts.AnalyzeDuration <= 0 {
		return fmt.Errorf("video metadata probe limits must be greater than 0")
	}
	if opts.FFmpegPath == "" {
		opts.FFmpegPath = "ffmpeg"
	}
	if opts.FFprobePath == "" {
		opts.FFprobePath = "ffprobe"
	}
	return nil
}

func reportVideoCheck(reporter VideoCheckReporter, stage string, message string) {
	if reporter != nil {
		reporter(VideoCheckEvent{Stage: stage, Message: message})
	}
}

func readVideoMetadata(ctx context.Context, path string, opts VideoCheckOptions) (*videoMetadata, string, error) {
	args := []string{
		"-v", "error",
		"-probesize", strconv.FormatInt(int64(opts.ProbeSizeMiB)*1024*1024, 10),
		"-analyzeduration", strconv.FormatInt(opts.AnalyzeDuration.Microseconds(), 10),
		"-show_entries", "format=duration,size:stream=index,codec_type,codec_name,duration:stream_disposition=attached_pic",
		"-of", "json",
		path,
	}
	command := runVideoTool(ctx, opts.Timeout, opts.FFprobePath, args...)
	if ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	if command.timedOut {
		return nil, fmt.Sprintf("ffprobe timed out after %s", opts.Timeout), nil
	}
	if command.err != nil {
		return nil, conciseVideoError(command.stderr), nil
	}

	var payload struct {
		Streams []struct {
			Index       int    `json:"index"`
			CodecType   string `json:"codec_type"`
			CodecName   string `json:"codec_name"`
			Duration    string `json:"duration"`
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
			Size     string `json:"size"`
		} `json:"format"`
	}
	if err := json.Unmarshal([]byte(command.stdout), &payload); err != nil {
		return nil, fmt.Sprintf("invalid ffprobe JSON: %v", err), nil
	}

	streamIndex := -1
	codec := "unknown"
	streamDuration := ""
	for _, stream := range payload.Streams {
		if stream.CodecType == "video" && stream.Disposition.AttachedPic == 0 {
			streamIndex = stream.Index
			codec = stream.CodecName
			streamDuration = stream.Duration
			break
		}
	}
	if streamIndex < 0 {
		return nil, "no non-thumbnail video stream found", nil
	}

	duration, ok := positiveVideoFloat(streamDuration)
	if !ok {
		duration, ok = positiveVideoFloat(payload.Format.Duration)
	}
	if !ok {
		return nil, "container does not expose a usable duration", nil
	}

	size := int64(0)
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	} else if parsed, err := strconv.ParseInt(payload.Format.Size, 10, 64); err == nil {
		size = parsed
	}
	return &videoMetadata{
		sizeBytes:        size,
		durationSeconds:  duration,
		videoStreamIndex: streamIndex,
		codec:            codec,
	}, "", nil
}

func positiveVideoFloat(value string) (float64, bool) {
	parsed, err := strconv.ParseFloat(value, 64)
	return parsed, err == nil && parsed > 0 && !math.IsInf(parsed, 0) && !math.IsNaN(parsed)
}

func runDecodeCheck(ctx context.Context, path string, metadata videoMetadata, opts VideoCheckOptions, kind string, start float64, duration *float64, oneFrame bool, timeout time.Duration) (VideoDecodeCheck, error) {
	check, err := runDecodeAttempt(ctx, path, metadata, opts, kind, start, duration, oneFrame, timeout, opts.StrictBitstream)
	if err != nil || !opts.StrictBitstream || check.State != checkFailed {
		return check, err
	}

	practical, err := runDecodeAttempt(ctx, path, metadata, opts, kind+"-practical-retry", start, duration, oneFrame, timeout, false)
	if err != nil {
		return practical, err
	}
	check.ElapsedSeconds += practical.ElapsedSeconds
	if decodeCheckPassed(practical) {
		check.State = checkNonconformant
		check.Frames = practical.Frames
		check.LastOutputSeconds = practical.LastOutputSeconds
		check.Detail = "strict bitstream validation failed but practical decoding succeeded: " + check.Detail
		return check, nil
	}
	if practical.State != checkFailed {
		practical.Detail = "strict bitstream validation failed and the practical retry was inconclusive: " + practical.Detail
		return practical, nil
	}
	return check, nil
}

func runDecodeAttempt(ctx context.Context, path string, metadata videoMetadata, opts VideoCheckOptions, kind string, start float64, duration *float64, oneFrame bool, timeout time.Duration, strict bool) (VideoDecodeCheck, error) {
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-xerror",
	}
	if strict {
		args = append(args, "-err_detect", "explode")
	}
	args = append(args,
		"-ss", fmt.Sprintf("%.6f", math.Max(0, start)),
		"-i", path,
		"-map", fmt.Sprintf("0:%d", metadata.videoStreamIndex),
		"-an", "-sn", "-dn",
	)
	if duration != nil {
		args = append(args, "-t", fmt.Sprintf("%.6f", *duration))
	}
	if oneFrame {
		args = append(args, "-frames:v", "1")
	}
	args = append(args, "-progress", "pipe:1", "-nostats", "-f", "null", os.DevNull)

	command := runVideoTool(ctx, timeout, opts.FFmpegPath, args...)
	frames, relativeOutput := decodedProgress(command.stdout)
	check := VideoDecodeCheck{
		Kind:             kind,
		StartSeconds:     math.Max(0, start),
		RequestedSeconds: duration,
		StrictBitstream:  strict,
		Frames:           frames,
		ElapsedSeconds:   command.elapsed.Seconds(),
	}
	if relativeOutput != nil {
		absolute := check.StartSeconds + *relativeOutput
		check.LastOutputSeconds = &absolute
	}
	switch {
	case ctx.Err() != nil:
		check.State = checkCancelled
		check.Detail = ctx.Err().Error()
		return check, ctx.Err()
	case command.timedOut:
		check.State = checkTimeout
		check.Detail = fmt.Sprintf("%s decode exceeded %s after producing %d frame(s)", kind, timeout, check.Frames)
	case command.err != nil:
		check.State = checkFailed
		check.Detail = conciseVideoError(command.stderr)
	case check.Frames == 0:
		check.State = checkInconclusive
		check.Detail = fmt.Sprintf("%s decode completed but produced no video frame", kind)
	default:
		check.State = checkPassed
	}
	return check, nil
}

func runVideoTool(ctx context.Context, timeout time.Duration, executable string, args ...string) toolResult {
	started := time.Now()
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := exec.CommandContext(commandCtx, executable, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return toolResult{
		stdout:   stdout.String(),
		stderr:   stderr.String(),
		elapsed:  time.Since(started),
		err:      err,
		timedOut: errors.Is(commandCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil,
	}
}

func decodedProgress(output string) (int, *float64) {
	maxFrames := 0
	for _, match := range framePattern.FindAllStringSubmatch(output, -1) {
		value, err := strconv.Atoi(match[1])
		if err == nil && value > maxFrames {
			maxFrames = value
		}
	}
	var maxOutput float64
	foundOutput := false
	for _, match := range outputTimePattern.FindAllStringSubmatch(output, -1) {
		value, err := strconv.ParseInt(match[1], 10, 64)
		if err == nil && (!foundOutput || float64(value)/1_000_000 > maxOutput) {
			maxOutput = float64(value) / 1_000_000
			foundOutput = true
		}
	}
	if !foundOutput {
		return maxFrames, nil
	}
	return maxFrames, &maxOutput
}

func conciseVideoError(stderr string) string {
	var lines []string
	for _, line := range strings.Split(stderr, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return "FFmpeg returned an error without a diagnostic message"
	}
	terms := []string{"could not find ref", "constructing the frame", "invalid", "corrupt", "error splitting", "error while decoding", "partial file", "missing picture", "failed to"}
	var selected []string
	for _, line := range lines {
		lower := strings.ToLower(line)
		if slices.ContainsFunc(terms, func(term string) bool { return strings.Contains(lower, term) }) {
			selected = append(selected, line)
			if len(selected) == 2 {
				break
			}
		}
	}
	if len(selected) == 0 {
		selected = append(selected, lines[0])
		if len(lines) > 1 {
			selected = append(selected, lines[1])
		}
	}
	last := lines[len(lines)-1]
	if len(selected) < 2 && !slices.Contains(selected, last) && !strings.Contains(strings.ToLower(last), "nothing was written") {
		selected = append(selected, last)
	}
	message := strings.Join(selected, " | ")
	const limit = 500
	if len(message) > limit {
		message = message[:limit-3] + "..."
	}
	return message
}

func videoSparsePoints(duration float64, tailStart float64, count int) []float64 {
	if count <= 0 || tailStart <= 1 {
		return nil
	}
	seen := map[float64]bool{}
	var points []float64
	for i := 0; i < count; i++ {
		point := duration * float64(i+1) / float64(count+1)
		if point < tailStart-0.25 && !seen[point] {
			seen[point] = true
			points = append(points, math.Max(0, point))
		}
	}
	slices.Sort(points)
	return points
}

func locateLastDecodableBoundary(ctx context.Context, path string, metadata videoMetadata, opts VideoCheckOptions, badPoint float64, checks *[]VideoDecodeCheck, reporter VideoCheckReporter) (*float64, string, bool, error) {
	windowAt := func(kind string, point float64) (VideoDecodeCheck, error) {
		duration := math.Min(opts.SampleSeconds, math.Max(0.001, metadata.durationSeconds-point))
		check, err := runDecodeCheck(ctx, path, metadata, opts, kind, point, &duration, false, opts.Timeout)
		*checks = append(*checks, check)
		return check, err
	}

	startCheck, err := windowAt("locator-start-window", 0)
	if err != nil {
		return nil, "", false, err
	}
	if !decodeCheckPassed(startCheck) {
		return nil, fmt.Sprintf("localization unavailable because the startup window was %s: %s", startCheck.State, startCheck.Detail), false, nil
	}

	good, bad := 0.0, math.Max(opts.Resolution, badPoint)
	for bad-good > opts.Resolution {
		midpoint := (good + bad) / 2
		reportVideoCheck(reporter, "locate", fmt.Sprintf("probing %.2fs window near %s", opts.SampleSeconds, FormatVideoTimestamp(midpoint)))
		check, err := windowAt("locator-window", midpoint)
		if err != nil {
			return nil, "", false, err
		}
		switch {
		case decodeCheckPassed(check):
			good = midpoint
		case check.State == checkFailed || (check.State == checkInconclusive && check.Frames == 0):
			bad = midpoint
		default:
			estimate := bad
			return &estimate, fmt.Sprintf("localization stopped when the window at %s became %s; last known-good window starts at %s", FormatVideoTimestamp(midpoint), check.State, FormatVideoTimestamp(good)), false, nil
		}
	}
	estimate := bad
	return &estimate, fmt.Sprintf("approximate first failing decode window; last known-good %.2fs window starts at %s (+/-%.3gs). This estimate assumes failures persist to the declared end", opts.SampleSeconds, FormatVideoTimestamp(good), opts.Resolution), true, nil
}

func FormatVideoTimestamp(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	milliseconds := int64(math.Round(seconds * 1000))
	hours := milliseconds / 3_600_000
	milliseconds %= 3_600_000
	minutes := milliseconds / 60_000
	milliseconds %= 60_000
	wholeSeconds := milliseconds / 1000
	milliseconds %= 1000
	if hours > 0 {
		return fmt.Sprintf("%d:%02d:%02d.%03d", hours, minutes, wholeSeconds, milliseconds)
	}
	return fmt.Sprintf("%d:%02d.%03d", minutes, wholeSeconds, milliseconds)
}
