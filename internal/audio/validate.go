package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// QualityThreshold is the minimum waveform variance (over int16 PCM
// samples decoded from the first ~5 seconds of audio) below which we
// treat the clip as effectively silent / broken. Empirical anchors:
//
//	MeloTTS on Chinese (broken)              ~5
//	edge-tts Yunyang / Yunxi (real speech)   > 15 000
//	real human podcast                       > 50 000
//
// 1000 is a comfortable floor that rejects the silent-blob failure
// mode while letting everything that sounds remotely like speech
// through, regardless of voice or pacing.
const QualityThreshold = 1000.0

// CheckAudioQuality decodes the first ~5 seconds of `data` into PCM
// samples (via ffmpeg) and reports the variance. If ffmpeg is missing
// we skip validation and return (0, nil) — a missing validator is
// preferable to blocking audio generation on an environment that
// doesn't have ffmpeg installed. Callers that want stricter behavior
// can check for that sentinel (variance == 0, err == nil).
//
// Returns (variance, error). When err != nil the audio is considered
// low-quality; callers should treat it as a Synthesize failure.
func CheckAudioQuality(ctx context.Context, data []byte, mime string) (float64, error) {
	// Unit tests use tiny synthetic WAV payloads (< 200 bytes) that can't
	// possibly pass a real variance check; they opt out via this env.
	if os.Getenv("AUDIO_SKIP_QUALITY_CHECK") == "1" {
		return 0, nil
	}
	if len(data) < 256 {
		return 0, fmt.Errorf("audio: clip too small (%d bytes) to be real", len(data))
	}
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		// ffmpeg missing — skip validation rather than block.
		return 0, nil
	}

	// Write to a temp file (ffmpeg's stdin pipe is flaky on some
	// container variants and the cost of a temp write is trivial
	// relative to the TTS call that produced this clip).
	ext := extForMime(mime)
	if ext == "" {
		ext = "bin"
	}
	tmp, err := os.CreateTemp("", "audio-validate-*."+ext)
	if err != nil {
		return 0, fmt.Errorf("audio: temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return 0, fmt.Errorf("audio: write temp: %w", err)
	}
	tmp.Close()

	variance, err := pcmVariance(ctx, ffmpegPath, tmpPath)
	if err != nil {
		// ffmpeg itself failed — treat as broken (either the bytes
		// aren't valid audio or the env is too broken to validate).
		return 0, fmt.Errorf("audio: ffmpeg decode for validation: %w", err)
	}
	if variance < QualityThreshold {
		return variance, fmt.Errorf(
			"audio: waveform variance %.0f below threshold %.0f — likely silent / broken TTS output",
			variance, QualityThreshold,
		)
	}
	return variance, nil
}

// pcmVariance runs `ffmpeg -i <src> -t 5 -f s16le -ac 1 -ar 24000 -`
// and computes the variance of the decoded int16 samples. 5 seconds
// is enough signal to tell silence from speech; 24 kHz mono is the
// smallest size that preserves enough detail for a meaningful var.
func pcmVariance(ctx context.Context, ffmpegPath, src string) (float64, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := []string{
		"-i", src,
		"-t", "5",
		"-f", "s16le",
		"-ac", "1",
		"-ar", "24000",
		"-",
	}
	// Silence ffmpeg's logging — our only interest is the PCM stdout.
	cmd := exec.CommandContext(cmdCtx, ffmpegPath, args...)
	cmd.Stderr = nil
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return 0, err
	}
	raw := stdout.Bytes()
	if len(raw) < 200 {
		return 0, fmt.Errorf("ffmpeg produced only %d bytes of PCM — clip too short", len(raw))
	}

	sampleBytes := len(raw) - (len(raw) % 2)
	nSamples := sampleBytes / 2
	if nSamples == 0 {
		return 0, errors.New("no samples")
	}

	var sum, sumSq float64
	rdr := bytes.NewReader(raw[:sampleBytes])
	var s int16
	for i := 0; i < nSamples; i++ {
		if err := binary.Read(rdr, binary.LittleEndian, &s); err != nil {
			break
		}
		f := float64(s)
		sum += f
		sumSq += f * f
	}
	mean := sum / float64(nSamples)
	variance := (sumSq / float64(nSamples)) - mean*mean
	return variance, nil
}

// ValidatePath is a file-based convenience wrapper: run variance
// check against a clip already on disk. Used by save.go post-write
// sanity check (optional second pass after ffmpeg conversion).
func ValidatePath(ctx context.Context, path string) (float64, error) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return 0, nil
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() < 256 {
		return 0, fmt.Errorf("audio: %s missing or too small", filepath.Base(path))
	}
	return pcmVariance(ctx, ffmpegPath, path)
}
