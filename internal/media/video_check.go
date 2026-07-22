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
	VideoStatusLikelyOK     VideoCheckStatus = "LIKELY_OK"
	VideoStatusOK           VideoCheckStatus = "OK"
	VideoStatusCorrupt      VideoCheckStatus = "CORRUPT"
	VideoStatusUnreadable   VideoCheckStatus = "UNREADABLE"
	VideoStatusInconclusive VideoCheckStatus = "INCONCLUSIVE"
)

type VideoCheckOptions struct {
	Mode            VideoCheckMode
	Samples         int
	TailSeconds     float64
	Locate          bool
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
	Kind             string   `json:"kind"`
	StartSeconds     float64  `json:"start_seconds"`
	RequestedSeconds *float64 `json:"requested_seconds,omitempty"`
	State            string   `json:"state"`
	Frames           int      `json:"frames"`
	ElapsedSeconds   float64  `json:"elapsed_seconds"`
	Detail           string   `json:"detail,omitempty"`
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
	return r.Status == VideoStatusOK || r.Status == VideoStatusLikelyOK
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
	checkPassed       = "passed"
	checkFailed       = "failed"
	checkTimeout      = "timeout"
	checkInconclusive = "inconclusive"
	checkCancelled    = "cancelled"
)

var framePattern = regexp.MustCompile(`(?m)^frame=(\d+)\r?$`)

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
		case checkFailed:
			result.Status = VideoStatusCorrupt
			result.Reason = check.Detail
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
		if opts.Locate && metadata.durationSeconds > opts.Resolution {
			reportVideoCheck(reporter, "locate", "locating persistent corruption boundary")
			estimate, note, err := locateVideoSuffixFailure(ctx, path, *metadata, opts, tailSeconds, &result.Checks, reporter)
			if err != nil {
				return finish(), err
			}
			result.FailureEstimateSeconds = estimate
			result.EstimateNote = note
		}
		return finish(), nil
	}
	if tail.State != checkPassed {
		result.Status = VideoStatusInconclusive
		result.Reason = tail.Detail
		return finish(), nil
	}

	points := videoSparsePoints(metadata.durationSeconds, tailStart, opts.Samples)
	for i, point := range points {
		reportVideoCheck(reporter, "sample", fmt.Sprintf("checking interior sample %d/%d", i+1, len(points)))
		check, err := runDecodeCheck(ctx, path, *metadata, opts, "interior-sample", point, nil, true, opts.Timeout)
		if err != nil {
			return finish(), err
		}
		result.Checks = append(result.Checks, check)
		if check.State == checkFailed {
			result.Status = VideoStatusCorrupt
			result.Reason = check.Detail
			estimate := point
			result.FailureEstimateSeconds = &estimate
			result.EstimateNote = "corruption was detected near this sampled seek/GOP; this is not necessarily the first damaged frame"
			return finish(), nil
		}
		if check.State != checkPassed {
			result.Status = VideoStatusInconclusive
			result.Reason = check.Detail
			return finish(), nil
		}
	}

	result.Status = VideoStatusLikelyOK
	result.Reason = fmt.Sprintf("tail and %d interior sample(s) decoded cleanly; unsampled regions were not read", len(points))
	return finish(), nil
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
	if opts.TailSeconds <= 0 {
		return fmt.Errorf("video check tail seconds must be greater than 0")
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

	duration, ok := positiveVideoFloat(payload.Format.Duration)
	if !ok {
		duration, ok = positiveVideoFloat(streamDuration)
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
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-xerror",
		"-err_detect", "explode",
		"-ss", fmt.Sprintf("%.6f", math.Max(0, start)),
		"-i", path,
		"-map", fmt.Sprintf("0:%d", metadata.videoStreamIndex),
		"-an", "-sn", "-dn",
	}
	if duration != nil {
		args = append(args, "-t", fmt.Sprintf("%.6f", *duration))
	}
	if oneFrame {
		args = append(args, "-frames:v", "1")
	}
	args = append(args, "-progress", "pipe:1", "-nostats", "-f", "null", os.DevNull)

	command := runVideoTool(ctx, timeout, opts.FFmpegPath, args...)
	check := VideoDecodeCheck{
		Kind:             kind,
		StartSeconds:     math.Max(0, start),
		RequestedSeconds: duration,
		Frames:           decodedFrameCount(command.stdout),
		ElapsedSeconds:   command.elapsed.Seconds(),
	}
	switch {
	case ctx.Err() != nil:
		check.State = checkCancelled
		check.Detail = ctx.Err().Error()
		return check, ctx.Err()
	case command.timedOut:
		check.State = checkTimeout
		check.Detail = fmt.Sprintf("decode exceeded %s", timeout)
	case command.err != nil:
		check.State = checkFailed
		check.Detail = conciseVideoError(command.stderr)
	case check.Frames == 0:
		check.State = checkInconclusive
		check.Detail = "decode completed but produced no video frame"
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

func decodedFrameCount(output string) int {
	maxFrames := 0
	for _, match := range framePattern.FindAllStringSubmatch(output, -1) {
		value, err := strconv.Atoi(match[1])
		if err == nil && value > maxFrames {
			maxFrames = value
		}
	}
	return maxFrames
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
	terms := []string{"invalid", "corrupt", "error splitting", "error while decoding", "partial file", "missing picture", "failed to"}
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
	if !slices.Contains(selected, last) && !strings.Contains(strings.ToLower(last), "nothing was written") {
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

func locateVideoSuffixFailure(ctx context.Context, path string, metadata videoMetadata, opts VideoCheckOptions, tailSeconds float64, checks *[]VideoDecodeCheck, reporter VideoCheckReporter) (*float64, string, error) {
	distances := []float64{0.10, 0.25, 0.50, 1.0, tailSeconds / 2, tailSeconds}
	var candidates []float64
	for _, distance := range distances {
		point := math.Max(0, metadata.durationSeconds-distance)
		if !slices.ContainsFunc(candidates, func(existing float64) bool { return math.Abs(point-existing) <= 0.01 }) {
			candidates = append(candidates, point)
		}
	}

	badPoint := -1.0
	for _, point := range candidates {
		check, err := runDecodeCheck(ctx, path, metadata, opts, "locator-endpoint", point, nil, true, opts.Timeout)
		if err != nil {
			return nil, "", err
		}
		*checks = append(*checks, check)
		if check.State == checkFailed {
			badPoint = point
			break
		}
	}
	if badPoint < 0 {
		return nil, "tail decode failed, but single-frame probes near the end did not; the error may be isolated or seek-dependent", nil
	}

	startCheck, err := runDecodeCheck(ctx, path, metadata, opts, "locator-start", 0, nil, true, opts.Timeout)
	if err != nil {
		return nil, "", err
	}
	*checks = append(*checks, startCheck)
	if startCheck.State != checkPassed {
		zero := 0.0
		return &zero, "the first independently decoded frame also fails", nil
	}

	good, bad := 0.0, badPoint
	for bad-good > opts.Resolution {
		midpoint := (good + bad) / 2
		reportVideoCheck(reporter, "locate", fmt.Sprintf("probing near %s", FormatVideoTimestamp(midpoint)))
		check, err := runDecodeCheck(ctx, path, metadata, opts, "locator-bisection", midpoint, nil, true, opts.Timeout)
		if err != nil {
			return nil, "", err
		}
		*checks = append(*checks, check)
		switch check.State {
		case checkFailed:
			bad = midpoint
		case checkPassed:
			good = midpoint
		default:
			estimate := bad
			return &estimate, fmt.Sprintf("localization became %s at %s; last known-good seek is %s", check.State, FormatVideoTimestamp(midpoint), FormatVideoTimestamp(good)), nil
		}
	}
	estimate := bad
	return &estimate, fmt.Sprintf("approximate first failing seek/GOP; last known-good seek is %s (+/-%.3gs). This assumes corruption persists from the boundary to the end", FormatVideoTimestamp(good), opts.Resolution), nil
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
