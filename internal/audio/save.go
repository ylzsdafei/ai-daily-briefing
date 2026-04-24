package audio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SaveAudio writes `audioBytes` into the Hextra site's static/audio/
// directory and returns the relative URL path that the frontend
// audio-player shortcode will consume ("/audio/YYYY-MM-DD.mp3" or
// ".wav"). It auto-detects the file extension from `mime` so callers
// don't need to care whether the upstream TTS returned WAV or MP3.
//
// If the system has `ffmpeg` on PATH AND the input is WAV, the
// function down-samples the WAV to a 64 kbit/s mono MP3 before
// writing the final file. Rationale:
//
//   - A 3-5 minute WAV at the default MeloTTS sample rate clocks in
//     around 25 MB, which is painful to ship through GitHub Pages
//     and awkward on mobile data.
//   - The same audio at 64 kbit/s mono MP3 is ~3 MB — perfectly
//     acceptable for spoken-word content and well within GitHub's
//     100 MB file limit.
//
// If ffmpeg is missing OR conversion fails, the function falls back
// to writing the raw bytes (preserving the original mime extension)
// so the audio feature still works — just larger.
//
// `hextraSiteDir` is the root of the Hugo site (HEXTRA_SITE_DIR env
// var). The resulting layout is:
//
//	{hextraSiteDir}/static/audio/{YYYY-MM-DD}.{mp3|wav}
//
// which Hugo serves at /audio/{YYYY-MM-DD}.{mp3|wav}.
func SaveAudio(
	ctx context.Context,
	audioBytes []byte,
	mime string,
	date time.Time,
	hextraSiteDir string,
) (string, error) {
	if len(audioBytes) == 0 {
		return "", errors.New("audio: SaveAudio requires non-empty audioBytes")
	}
	hextraSiteDir = strings.TrimSpace(hextraSiteDir)
	if hextraSiteDir == "" {
		return "", errors.New("audio: SaveAudio requires non-empty hextraSiteDir (HEXTRA_SITE_DIR)")
	}

	// Quality gate: decode first ~5s as PCM, reject if variance below
	// floor. Prevents silent / broken TTS output (MeloTTS "康" bug)
	// from overwriting a previous day's good MP3. fail-soft if
	// ffmpeg is missing — CheckAudioQuality returns (0, nil) in that
	// case, and we let the write proceed.
	if variance, err := CheckAudioQuality(ctx, audioBytes, mime); err != nil {
		return "", fmt.Errorf("audio: quality check failed (variance=%.0f): %w", variance, err)
	}

	dateStr := date.Format("2006-01-02")
	audioDir := filepath.Join(hextraSiteDir, "static", "audio")
	if err := os.MkdirAll(audioDir, 0o755); err != nil {
		return "", fmt.Errorf("audio: mkdir %s: %w", audioDir, err)
	}

	srcExt := extForMime(mime)

	// Try ffmpeg conversion only when (a) we have an input WAV and
	// (b) ffmpeg is on PATH. Everything else goes straight to disk.
	if srcExt == "wav" {
		if ffmpegPath, err := exec.LookPath("ffmpeg"); err == nil {
			outPath := filepath.Join(audioDir, dateStr+".mp3")
			if convErr := convertWAVToMP3(ctx, ffmpegPath, audioBytes, outPath); convErr == nil {
				return "/audio/" + dateStr + ".mp3", nil
			}
			// Conversion failed — cleanup partial output (if any) and
			// fall through to raw write below.
			_ = os.Remove(outPath)
		}
	}

	// Raw write fallback: whatever mime we detected becomes the
	// extension. For MeloTTS this means WAV today; if we ever swap
	// to an MP3-returning provider, this path writes MP3 directly.
	outPath := filepath.Join(audioDir, dateStr+"."+srcExt)
	if err := os.WriteFile(outPath, audioBytes, 0o644); err != nil {
		return "", fmt.Errorf("audio: write %s: %w", outPath, err)
	}
	return "/audio/" + dateStr + "." + srcExt, nil
}

// extForMime maps a mime type to a file extension without the dot.
// Unknown mimes fall back to "bin" so the caller sees an obviously
// wrong filename instead of a silent failure.
func extForMime(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "audio/wav", "audio/wave", "audio/x-wav":
		return "wav"
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/ogg":
		return "ogg"
	case "audio/flac":
		return "flac"
	default:
		return "bin"
	}
}

// convertWAVToMP3 pipes `wavBytes` to ffmpeg over stdin and asks for
// a 64 kbit/s mono MP3 written to `outPath`. Using stdin avoids an
// intermediate .wav file on disk.
//
// ffmpeg flags explained:
//
//	-hide_banner, -loglevel error  — keep stderr quiet unless something actually fails
//	-y                              — overwrite outPath if it exists
//	-f wav -i pipe:0                — force WAV demux on stdin
//	-vn                             — no video stream
//	-ac 1                           — downmix to mono (spoken word is mono)
//	-ar 24000                       — 24 kHz sample rate (plenty for speech)
//	-b:a 64k                        — 64 kbit/s MP3 (about 480 KB/min, 3 MB for 5 min)
//	-acodec libmp3lame              — explicit MP3 codec (some ffmpeg builds need it)
func convertWAVToMP3(ctx context.Context, ffmpegPath string, wavBytes []byte, outPath string) error {
	// Guard against pathological huge inputs blocking stdin.
	if len(wavBytes) == 0 {
		return errors.New("empty wav")
	}

	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-y",
		"-f", "wav", "-i", "pipe:0",
		"-vn",
		"-ac", "1",
		"-ar", "24000",
		"-b:a", "64k",
		"-acodec", "libmp3lame",
		outPath,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg stdin pipe: %w", err)
	}

	// Capture combined stderr for diagnostics on failure.
	var errBuf strings.Builder
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}

	// Feed bytes then close stdin so ffmpeg sees EOF.
	writeErr := writeAllAndClose(stdin, wavBytes)
	waitErr := cmd.Wait()

	if writeErr != nil {
		return fmt.Errorf("ffmpeg stdin write: %w (stderr: %s)", writeErr, errBuf.String())
	}
	if waitErr != nil {
		return fmt.Errorf("ffmpeg wait: %w (stderr: %s)", waitErr, errBuf.String())
	}

	// Sanity-check the resulting file exists and is non-trivially sized.
	fi, err := os.Stat(outPath)
	if err != nil {
		return fmt.Errorf("ffmpeg produced no output: %w", err)
	}
	if fi.Size() < 1024 {
		return fmt.Errorf("ffmpeg output too small (%d bytes)", fi.Size())
	}
	return nil
}

// writeAllAndClose is a small helper around (io.WriteCloser).Write
// that guarantees Close happens even if Write fails.
func writeAllAndClose(w interface {
	Write(p []byte) (int, error)
	Close() error
}, b []byte) error {
	_, writeErr := w.Write(b)
	closeErr := w.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
