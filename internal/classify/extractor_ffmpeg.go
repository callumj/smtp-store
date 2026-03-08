package classify

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

// FFmpegExtractor samples jpeg frames from a video file.
type FFmpegExtractor struct{}

func (FFmpegExtractor) ExtractFrames(ctx context.Context, videoPath string, frameCount int) ([][]byte, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, ErrFFmpegUnavailable
	}

	tmpDir, err := os.MkdirTemp("", "smtp-store-frames-*")
	if err != nil {
		return nil, fmt.Errorf("create frame temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	pattern := filepath.Join(tmpDir, "frame_%03d.jpg")
	cmd := exec.CommandContext(
		ctx,
		"ffmpeg",
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-i", videoPath,
		"-vf", "fps=1",
		"-frames:v", fmt.Sprintf("%d", frameCount),
		pattern,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg extract frames: %w (%s)", err, string(out))
	}

	files, err := filepath.Glob(filepath.Join(tmpDir, "frame_*.jpg"))
	if err != nil {
		return nil, fmt.Errorf("list frame files: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("ffmpeg produced no frames")
	}
	sort.Strings(files)

	frames := make([][]byte, 0, len(files))
	for _, file := range files {
		payload, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read frame file: %w", err)
		}
		frames = append(frames, payload)
	}
	return frames, nil
}
