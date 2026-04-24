package audio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Backend identifiers used in config/ai.yaml's audio.backend field.
// Exported so cmd/briefing/run.go (and future callers) compare against
// the same string constants instead of duplicating literals.
const (
	BackendEdge = "edge"
	BackendCF   = "cf"
)

// TTSClient is the minimal synthesis surface the rest of the briefing
// pipeline depends on. Implementing this interface lets operators
// swap MeloTTS (Cloudflare Workers AI) out for a different provider
// (Replicate IndexTTS-2, Azure TTS, ...) without touching the caller
// code in cmd/briefing/run.go — they only need to wire up a new
// constructor in pipeline-integrate.
type TTSClient interface {
	// Synthesize converts `text` into audio bytes.
	// Returns (audioBytes, mimeType, error). Common mime types:
	//   - "audio/wav"  — MeloTTS (Cloudflare) returns RIFF WAV despite
	//                    the upstream docs claiming MP3. Verified
	//                    empirically 2026-04-24.
	//   - "audio/mpeg" — Replicate etc.
	Synthesize(ctx context.Context, text string, opts SynthesizeOpts) ([]byte, string, error)
}

// SynthesizeOpts carries per-call knobs. Kept in its own struct so
// future providers can add voice / speaker / style options without
// an interface break.
type SynthesizeOpts struct {
	// Lang ISO code passed to the provider. MeloTTS uses "zh" for
	// Mandarin Chinese. Empty string => "zh" (sane default for our
	// Chinese-only pipeline). Ignored by providers that derive the
	// language from Voice (edge-tts).
	Lang string
	// Voice — edge-tts voice short-name (e.g. "zh-CN-YunjianNeural").
	// Ignored by MeloTTS which has a single voice per language.
	// Empty string => client's default voice.
	Voice string
	// Rate — edge-tts prosody adjustment (e.g. "+5%", "-10%"). Empty
	// string => client's default rate. Ignored by MeloTTS.
	Rate string
}

// CFTTSClient talks to Cloudflare Workers AI's MeloTTS model. All
// state is goroutine-safe; the intended usage is one shared client
// per pipeline run (pipeline-integrate holds the singleton).
type CFTTSClient struct {
	apiToken  string
	accountID string
	timeout   time.Duration
	// apiURLOverride lets tests redirect HTTP traffic to a
	// httptest.NewServer URL. Empty in production — in that case the
	// URL is built from accountID.
	apiURLOverride string
	// httpClient is stored so tests can substitute a custom
	// transport if they ever need to. Nil means "build on demand".
	httpClient *http.Client
}

// cfTTSDefaultTimeout is the per-request HTTP timeout. WAV
// generation for a ~1500 rune Chinese monologue takes ~25 seconds
// on a cold Worker, so 60s leaves headroom for retries + CF queue
// warmup without letting the pipeline hang forever.
const cfTTSDefaultTimeout = 60 * time.Second

// NewCFTTSClient constructs a production-ready client. Accepts the
// two values we already bake into config/secrets.env:
//
//	CF_API_TOKEN  — Workers AI user token (Bearer auth)
//	CF_ACCOUNT_ID — the account under which MeloTTS runs
func NewCFTTSClient(apiToken, accountID string) *CFTTSClient {
	return &CFTTSClient{
		apiToken:   strings.TrimSpace(apiToken),
		accountID:  strings.TrimSpace(accountID),
		timeout:    cfTTSDefaultTimeout,
		httpClient: &http.Client{},
	}
}

// WithTimeout returns a copy with a custom per-request timeout.
// Useful for tests that want sub-second failure modes.
func (c *CFTTSClient) WithTimeout(d time.Duration) *CFTTSClient {
	cp := *c
	cp.timeout = d
	return &cp
}

// WithAPIURLOverride returns a copy whose HTTP traffic goes to
// `url` instead of the real Cloudflare endpoint. Tests call this
// with httptest.Server.URL so they never touch the network.
// Production code NEVER calls this.
func (c *CFTTSClient) WithAPIURLOverride(url string) *CFTTSClient {
	cp := *c
	cp.apiURLOverride = strings.TrimRight(url, "/")
	return &cp
}

// cfTTSRequest matches the Workers AI run endpoint schema.
type cfTTSRequest struct {
	Prompt string `json:"prompt"`
	Lang   string `json:"lang"`
}

// cfTTSResponse matches the Workers AI standard envelope. Note that
// `result.audio` is the base64-encoded audio, and the *actual*
// encoding has been observed to be WAV (RIFF header) regardless of
// what the upstream docs claim about MP3.
type cfTTSResponse struct {
	Result struct {
		Audio string `json:"audio"`
	} `json:"result"`
	Success  bool              `json:"success"`
	Errors   []cfTTSErrorEntry `json:"errors"`
	Messages []cfTTSErrorEntry `json:"messages"`
}

type cfTTSErrorEntry struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Synthesize implements TTSClient against Cloudflare Workers AI.
func (c *CFTTSClient) Synthesize(ctx context.Context, text string, opts SynthesizeOpts) ([]byte, string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "", errors.New("audio: Synthesize requires non-empty text")
	}
	if c.apiToken == "" {
		return nil, "", errors.New("audio: CF_API_TOKEN missing")
	}
	if c.accountID == "" && c.apiURLOverride == "" {
		return nil, "", errors.New("audio: CF_ACCOUNT_ID missing")
	}

	lang := opts.Lang
	if lang == "" {
		lang = "zh"
	}

	reqBody := cfTTSRequest{Prompt: text, Lang: lang}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return nil, "", fmt.Errorf("marshal: %w", err)
	}

	apiURL := c.resolveURL()

	timeout := c.timeout
	if timeout <= 0 {
		timeout = cfTTSDefaultTimeout
	}
	httpCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(httpCtx, http.MethodPost, apiURL, bytes.NewReader(buf))
	if err != nil {
		return nil, "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Accept", "application/json")

	hc := c.httpClient
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(body)
		if len(snippet) > 500 {
			snippet = snippet[:500]
		}
		return nil, "", fmt.Errorf("cf tts http %d: %s", resp.StatusCode, snippet)
	}

	var parsed cfTTSResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, "", fmt.Errorf("parse cf response: %w", err)
	}
	if !parsed.Success {
		msgs := extractMessages(parsed.Errors)
		if msgs == "" {
			msgs = extractMessages(parsed.Messages)
		}
		if msgs == "" {
			msgs = "unknown error (success=false but no errors[])"
		}
		return nil, "", fmt.Errorf("cf tts failed: %s", msgs)
	}
	if parsed.Result.Audio == "" {
		return nil, "", errors.New("cf tts returned empty audio field")
	}

	audio, err := base64.StdEncoding.DecodeString(parsed.Result.Audio)
	if err != nil {
		return nil, "", fmt.Errorf("decode base64 audio: %w", err)
	}
	if len(audio) < 32 {
		return nil, "", fmt.Errorf("cf tts returned implausibly short audio (%d bytes)", len(audio))
	}

	mime := detectAudioMime(audio)
	return audio, mime, nil
}

// resolveURL returns the actual HTTP endpoint. Tests override via
// apiURLOverride; production derives it from accountID.
func (c *CFTTSClient) resolveURL() string {
	if c.apiURLOverride != "" {
		return c.apiURLOverride
	}
	return fmt.Sprintf(
		"https://api.cloudflare.com/client/v4/accounts/%s/ai/run/@cf/myshell-ai/melotts",
		c.accountID,
	)
}

// extractMessages joins CF error entries into a human-readable string.
func extractMessages(entries []cfTTSErrorEntry) string {
	if len(entries) == 0 {
		return ""
	}
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Message == "" {
			continue
		}
		if e.Code != 0 {
			parts = append(parts, fmt.Sprintf("[%d] %s", e.Code, e.Message))
			continue
		}
		parts = append(parts, e.Message)
	}
	return strings.Join(parts, "; ")
}

// detectAudioMime sniffs the first few bytes to return the correct
// mime. MeloTTS empirically returns WAV, but keeping the detector
// general means we can switch models without changing callers.
//
//   - "RIFF" → WAV
//   - "ID3" or 0xFF 0xFB/0xF3/0xF2 → MP3
//   - "OggS" → Ogg
//   - "fLaC" → FLAC
//   - default → application/octet-stream (let the caller decide)
func detectAudioMime(b []byte) string {
	if len(b) < 4 {
		return "application/octet-stream"
	}
	switch {
	case bytes.HasPrefix(b, []byte("RIFF")):
		return "audio/wav"
	case bytes.HasPrefix(b, []byte("ID3")):
		return "audio/mpeg"
	case len(b) >= 2 && b[0] == 0xFF && (b[1] == 0xFB || b[1] == 0xF3 || b[1] == 0xF2):
		return "audio/mpeg"
	case bytes.HasPrefix(b, []byte("OggS")):
		return "audio/ogg"
	case bytes.HasPrefix(b, []byte("fLaC")):
		return "audio/flac"
	default:
		return "application/octet-stream"
	}
}

// compile-time check: CFTTSClient satisfies TTSClient.
var _ TTSClient = (*CFTTSClient)(nil)
