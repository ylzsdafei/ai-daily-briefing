package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// EdgeTTSClient synthesizes Chinese audio via Microsoft Edge's free
// TTS service (Azure Neural TTS endpoint that ships with the Edge
// browser). It shells out to the Python `edge-tts` MIT-licensed CLI
// (https://github.com/rany2/edge-tts). The underlying service is
// provided by Microsoft for Edge readers — no API key, no quota,
// no cost — so it meets the "零成本 + 全开源" constraint even though
// the service itself is closed-source.
//
// Why we replaced MeloTTS: Cloudflare's @cf/myshell-ai/melotts model
// returns broken Chinese output (waveform variance ~5, i.e. silence
// or a single repeated phoneme — verified 2026-04-24 by the operator
// actually listening to the produced MP3). edge-tts with YunjianNeural
// yields variance in the 27x10^4 range, indistinguishable from a human
// speaker in subjective tests.
//
// Output is native MP3 (24 kHz mono), so downstream code does NOT
// need ffmpeg transcoding — save.go's fast-path writes the bytes
// straight to disk.
type EdgeTTSClient struct {
	// binary is the resolved path to the edge-tts CLI. Defaults to
	// "edge-tts" (looked up via $PATH) but tests can override.
	binary string
	// defaultVoice used when SynthesizeOpts.Voice is empty.
	// zh-CN-YunjianNeural is a sports-commentator-style male voice
	// that best approximates Luo Yonghao's sharp / humorous register.
	defaultVoice string
	// defaultRate is the prosody adjustment sent via --rate. "+5%"
	// gives a snappier delivery without hurting intelligibility.
	defaultRate string
	// timeout caps the subprocess runtime. A 3000-rune script takes
	// ~5s on cold start; 120s is very generous headroom.
	timeout time.Duration
}

// edgeTTSDefaultTimeout is intentionally long because a 3000-rune
// script generation takes ~5 seconds, but we want to absorb the
// occasional Azure endpoint hiccup without the whole pipeline
// bailing. The outer audio.ScriptGenerator already has retry.
const edgeTTSDefaultTimeout = 120 * time.Second

// NewEdgeTTSClient constructs a production-ready edge-tts client
// with sane defaults for Chinese Luo-Yonghao-style monologue.
//
// Pass "" to either arg to accept the default (YunjianNeural / +5%).
func NewEdgeTTSClient(voice, rate string) *EdgeTTSClient {
	if strings.TrimSpace(voice) == "" {
		voice = "zh-CN-YunjianNeural"
	}
	if strings.TrimSpace(rate) == "" {
		rate = "+5%"
	}
	return &EdgeTTSClient{
		binary:       "edge-tts",
		defaultVoice: voice,
		defaultRate:  rate,
		timeout:      edgeTTSDefaultTimeout,
	}
}

// WithBinary overrides the CLI path (for tests using a stub script
// on $PATH).
func (c *EdgeTTSClient) WithBinary(path string) *EdgeTTSClient {
	cp := *c
	cp.binary = path
	return &cp
}

// WithTimeout returns a copy with a custom subprocess timeout.
func (c *EdgeTTSClient) WithTimeout(d time.Duration) *EdgeTTSClient {
	cp := *c
	cp.timeout = d
	return &cp
}

// Synthesize implements TTSClient against the edge-tts CLI.
//
// Call flow:
//  1. write `text` to a temp input file (edge-tts reads the full
//     script from stdin-as-file; --text arg can't carry newlines
//     reliably in shell contexts)
//  2. invoke: edge-tts --voice V --rate R --file IN --write-media OUT
//  3. read OUT, return bytes + "audio/mpeg"
//  4. delete both temp files (best-effort; defer)
//
// The function is safe to call from goroutines as long as each
// call gets its own (unique) temp files, which it does.
func (c *EdgeTTSClient) Synthesize(ctx context.Context, text string, opts SynthesizeOpts) ([]byte, string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "", errors.New("audio: Synthesize requires non-empty text")
	}

	voice := opts.Voice
	if voice == "" {
		voice = c.defaultVoice
	}
	rate := opts.Rate
	if rate == "" {
		rate = c.defaultRate
	}

	// Temp files. The operating system cleans /tmp/ on reboot so a
	// hard crash between create + defer still won't leak long-term.
	inFile, err := os.CreateTemp("", "edge-tts-in-*.txt")
	if err != nil {
		return nil, "", fmt.Errorf("create temp input: %w", err)
	}
	inPath := inFile.Name()
	defer os.Remove(inPath)

	if _, err := inFile.WriteString(text); err != nil {
		inFile.Close()
		return nil, "", fmt.Errorf("write temp input: %w", err)
	}
	if err := inFile.Close(); err != nil {
		return nil, "", fmt.Errorf("close temp input: %w", err)
	}

	outPath := filepath.Join(os.TempDir(), fmt.Sprintf("edge-tts-out-%d.mp3", time.Now().UnixNano()))
	defer os.Remove(outPath)

	timeout := c.timeout
	if timeout <= 0 {
		timeout = edgeTTSDefaultTimeout
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, c.binary,
		"--voice", voice,
		"--rate", rate,
		"--file", inPath,
		"--write-media", outPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		snippet := stderr.String()
		if len(snippet) > 500 {
			snippet = snippet[:500]
		}
		return nil, "", fmt.Errorf("edge-tts run: %w (stderr: %s)", err, snippet)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, "", fmt.Errorf("read edge-tts output: %w", err)
	}
	if len(data) < 256 {
		// 256 is a conservative floor; even silent MP3 headers are
		// ~200 bytes, so anything smaller is certainly broken.
		return nil, "", fmt.Errorf("edge-tts produced implausibly short audio (%d bytes)", len(data))
	}

	return data, detectAudioMime(data), nil
}

// compile-time check: EdgeTTSClient satisfies TTSClient.
var _ TTSClient = (*EdgeTTSClient)(nil)
