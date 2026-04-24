//go:build integration

// Package audio integration test — talks to the real Cloudflare
// Workers AI endpoint. This file is gated by the "integration" build
// tag so `go test ./internal/audio/...` never accidentally burns the
// free-tier neuron budget. Run explicitly with:
//
//	go test -tags integration -run TestCFIntegration ./internal/audio/
//
// Required env vars (match config/secrets.env):
//
//	CF_API_TOKEN   — Workers AI user token
//	CF_ACCOUNT_ID  — the account under which MeloTTS runs
package audio

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCFIntegration sends a short 测试 phrase to the real CF MeloTTS
// endpoint and asserts the result is a non-trivial WAV file. Files
// land in /tmp/audio-integration-test.{wav|mp3} for manual listening.
func TestCFIntegration(t *testing.T) {
	token := os.Getenv("CF_API_TOKEN")
	accID := os.Getenv("CF_ACCOUNT_ID")
	if token == "" || accID == "" {
		t.Skip("CF_API_TOKEN or CF_ACCOUNT_ID not set, skipping integration test")
	}

	client := NewCFTTSClient(token, accID).WithTimeout(90 * time.Second)

	text := "你好，这是 v 一点一 集成测试。"
	start := time.Now()
	audio, mime, err := client.Synthesize(context.Background(), text, SynthesizeOpts{Lang: "zh"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Synthesize failed after %v: %v", elapsed, err)
	}
	t.Logf("CF MeloTTS returned %d bytes, mime=%s in %v", len(audio), mime, elapsed)

	if len(audio) < 10_000 {
		t.Errorf("audio suspiciously small: %d bytes (expected >10KB)", len(audio))
	}
	if mime != "audio/wav" && mime != "audio/mpeg" {
		t.Errorf("unexpected mime %q — upstream schema may have changed", mime)
	}
	// Magic byte check: WAV should start with RIFF, MP3 with ID3 or sync frame.
	switch mime {
	case "audio/wav":
		if !strings.HasPrefix(string(audio), "RIFF") {
			t.Errorf("audio/wav mime but missing RIFF header: % x", audio[:min(8, len(audio))])
		}
	case "audio/mpeg":
		switch {
		case strings.HasPrefix(string(audio), "ID3"):
		case len(audio) >= 2 && audio[0] == 0xFF && (audio[1] == 0xFB || audio[1] == 0xF3 || audio[1] == 0xF2):
		default:
			t.Errorf("audio/mpeg mime but missing MP3 magic: % x", audio[:min(8, len(audio))])
		}
	}

	// Exercise SaveAudio end-to-end as well — this proves the whole
	// chain (CF → base64 decode → file write → optional ffmpeg) works.
	tmpDir := "/tmp/audio-integration-test-site"
	_ = os.RemoveAll(tmpDir)
	_ = os.MkdirAll(tmpDir, 0o755)

	date := time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC)
	relURL, err := SaveAudio(context.Background(), audio, mime, date, tmpDir)
	if err != nil {
		t.Fatalf("SaveAudio: %v", err)
	}
	t.Logf("SaveAudio wrote %s", relURL)

	// Also drop a copy at the well-known /tmp path for manual listening.
	ext := extForMime(mime)
	if strings.HasSuffix(relURL, ".mp3") {
		ext = "mp3"
	}
	diagPath := "/tmp/audio-integration-test." + ext
	if err := copyFileFromSite(tmpDir, relURL, diagPath); err != nil {
		t.Logf("could not copy to diag path: %v", err)
	} else {
		t.Logf("playback copy: %s", diagPath)
	}

	fi, err := os.Stat(filepath.Join(tmpDir, "static", relURL))
	if err != nil {
		// relURL already starts with /, the static/ prefix needs
		// trimming — stat via the known shape instead.
		siteFile := filepath.Join(tmpDir, "static"+relURL)
		fi, err = os.Stat(siteFile)
		if err != nil {
			t.Fatalf("cannot stat site file: %v", err)
		}
	}
	if fi.Size() < 10_000 {
		t.Errorf("saved file too small: %d bytes", fi.Size())
	}
}

// copyFileFromSite copies the file at {siteDir}/static{relURL} to
// diagPath. Purely a diagnostic convenience so operators can listen
// to the output after running the test.
func copyFileFromSite(siteDir, relURL, diagPath string) error {
	src := filepath.Join(siteDir, "static"+relURL)
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(diagPath, data, 0o644)
}
