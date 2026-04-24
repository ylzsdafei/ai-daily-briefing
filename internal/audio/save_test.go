package audio

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSaveAudio_WAVNoFFmpeg covers the path where the raw WAV is
// written unchanged (simulated by disguising the mime as a format
// ffmpeg conversion does not touch — "audio/mpeg" — so the code
// skips ffmpeg even if it's installed). Asserts the file exists,
// contents match, and the returned URL has the expected shape.
func TestSaveAudio_WAVNoFFmpeg(t *testing.T) {
	t.Setenv("AUDIO_SKIP_QUALITY_CHECK", "1")
	tmp := t.TempDir()
	date := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	// Use audio/mpeg mime to force the "raw write" branch regardless
	// of ffmpeg availability — that way the test is deterministic on
	// any CI machine (ffmpeg present or not).
	payload := []byte("ID3\x04\x00\x00\x00\x00\x00testaudiopayload")
	rel, err := SaveAudio(context.Background(), payload, "audio/mpeg", date, tmp)
	if err != nil {
		t.Fatalf("SaveAudio: %v", err)
	}
	if rel != "/audio/2026-04-24.mp3" {
		t.Errorf("relative URL = %q, want /audio/2026-04-24.mp3", rel)
	}

	onDisk := filepath.Join(tmp, "static", "audio", "2026-04-24.mp3")
	got, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("file bytes mismatch (len got=%d want=%d)", len(got), len(payload))
	}
}

// TestSaveAudio_WAVRawFallback verifies the fallback path where
// ffmpeg is NOT on PATH (we fake that by temporarily scrubbing PATH)
// and the WAV bytes are written as-is with a .wav extension.
func TestSaveAudio_WAVRawFallback(t *testing.T) {
	t.Setenv("AUDIO_SKIP_QUALITY_CHECK", "1")
	tmp := t.TempDir()

	// Scrub PATH so exec.LookPath("ffmpeg") fails inside SaveAudio.
	originalPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", originalPath) })
	_ = os.Setenv("PATH", "")

	// Minimal but valid RIFF WAV prefix — detectAudioMime would call
	// this "audio/wav", and the SaveAudio code sees mime="audio/wav"
	// and would try ffmpeg, but our PATH scrub makes LookPath fail.
	payload := append([]byte("RIFF"), make([]byte, 100)...)
	date := time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC)

	rel, err := SaveAudio(context.Background(), payload, "audio/wav", date, tmp)
	if err != nil {
		t.Fatalf("SaveAudio: %v", err)
	}
	if rel != "/audio/2026-04-24.wav" {
		t.Errorf("relative URL = %q, want /audio/2026-04-24.wav (ffmpeg-less fallback)", rel)
	}

	onDisk := filepath.Join(tmp, "static", "audio", "2026-04-24.wav")
	got, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) != len(payload) {
		t.Errorf("file len = %d, want %d", len(got), len(payload))
	}
}

// TestSaveAudio_EmptyInput guards against accidentally writing a
// zero-byte file (e.g. if the TTS client ever regresses and returns
// empty bytes).
func TestSaveAudio_EmptyInput(t *testing.T) {
	tmp := t.TempDir()
	date := time.Now()
	_, err := SaveAudio(context.Background(), nil, "audio/wav", date, tmp)
	if err == nil {
		t.Error("expected error for empty audioBytes")
	}
}

// TestSaveAudio_EmptySiteDir guards against silent writes to cwd
// when HEXTRA_SITE_DIR env var is unset.
func TestSaveAudio_EmptySiteDir(t *testing.T) {
	date := time.Now()
	_, err := SaveAudio(context.Background(), []byte("ID3\x04\x00\x00\x00\x00"), "audio/mpeg", date, "")
	if err == nil {
		t.Error("expected error for empty hextraSiteDir")
	}
	if !strings.Contains(err.Error(), "HEXTRA_SITE_DIR") {
		t.Errorf("error did not reference HEXTRA_SITE_DIR: %v", err)
	}
}

// TestSaveAudio_WithMP3Conversion runs only when ffmpeg is on PATH.
// It feeds a tiny synthetic WAV to SaveAudio and verifies:
//  1. The returned URL ends in .mp3 (conversion happened).
//  2. The file on disk starts with a valid MP3 magic (ID3 tag or
//     MPEG frame sync byte).
//
// A proper synthetic WAV is built via ffmpeg's lavfi sine generator
// first, then passed to SaveAudio. That keeps the test hermetic —
// we do not depend on any pre-recorded fixture file.
func TestSaveAudio_WithMP3Conversion(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed, skipping MP3 conversion test")
	}
	t.Setenv("AUDIO_SKIP_QUALITY_CHECK", "1")

	// Generate 2 seconds of silence as a real WAV so ffmpeg's decoder
	// inside SaveAudio has something legitimate to chew on.
	wavBytes := generateSilentWAV(t)

	tmp := t.TempDir()
	date := time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC)
	rel, err := SaveAudio(context.Background(), wavBytes, "audio/wav", date, tmp)
	if err != nil {
		t.Fatalf("SaveAudio: %v", err)
	}
	if !strings.HasSuffix(rel, ".mp3") {
		t.Errorf("with ffmpeg available, expected .mp3 URL, got %q", rel)
	}

	onDisk := filepath.Join(tmp, "static", "audio", "2026-04-24.mp3")
	got, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) < 1024 {
		t.Errorf("mp3 file suspiciously small: %d bytes", len(got))
	}
	// MP3 files start with either an ID3v2 tag ("ID3") or a raw
	// MPEG frame sync (0xFF 0xFB / 0xF3 / 0xF2). Accept both.
	switch {
	case strings.HasPrefix(string(got), "ID3"):
		// ok
	case len(got) >= 2 && got[0] == 0xFF && (got[1] == 0xFB || got[1] == 0xF3 || got[1] == 0xF2):
		// ok
	default:
		t.Errorf("mp3 magic missing: first bytes = % x", got[:min(8, len(got))])
	}
}

// generateSilentWAV uses ffmpeg (already on PATH — the caller
// guarantees this) to synthesize 2 seconds of silence as a WAV file
// in memory. We avoid the roundtrip of "write tmp wav → read back"
// by asking ffmpeg to write to pipe:1.
func generateSilentWAV(t *testing.T) []byte {
	t.Helper()
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "anullsrc=r=16000:cl=mono",
		"-t", "2",
		"-f", "wav",
		"pipe:1",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("generate silent wav: %v", err)
	}
	if len(out) < 100 {
		t.Fatalf("silent wav too small: %d bytes", len(out))
	}
	return out
}

// TestExtForMime exercises the mime → extension mapper.
func TestExtForMime(t *testing.T) {
	cases := map[string]string{
		"audio/wav":   "wav",
		"audio/wave":  "wav",
		"audio/x-wav": "wav",
		"audio/mpeg":  "mp3",
		"audio/mp3":   "mp3",
		"audio/ogg":   "ogg",
		"audio/flac":  "flac",
		"":            "bin",
		"nonsense":    "bin",
	}
	for mime, want := range cases {
		if got := extForMime(mime); got != want {
			t.Errorf("extForMime(%q) = %q, want %q", mime, got, want)
		}
	}
}
