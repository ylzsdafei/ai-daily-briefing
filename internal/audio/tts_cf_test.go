package audio

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeWAVBytes returns a minimal-but-valid RIFF WAV header prefix so
// detectAudioMime recognizes the result as audio/wav. The body does
// not need to be a playable waveform for the unit tests — we only
// assert on the byte prefix + length.
func fakeWAVBytes() []byte {
	header := []byte("RIFF")
	padding := make([]byte, 64)
	return append(header, padding...)
}

// TestCFTTSClient_Success verifies the happy path: the mock server
// returns success=true with base64-encoded "RIFF..." audio, and the
// client hands back the decoded bytes plus "audio/wav" mime.
func TestCFTTSClient_Success(t *testing.T) {
	want := fakeWAVBytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer test-token")
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var req cfTTSRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if req.Lang != "zh" {
			t.Errorf("req.Lang = %q, want zh", req.Lang)
		}
		if !strings.Contains(req.Prompt, "你好") {
			t.Errorf("req.Prompt missing expected text, got %q", req.Prompt)
		}

		resp := cfTTSResponse{Success: true}
		resp.Result.Audio = base64.StdEncoding.EncodeToString(want)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewCFTTSClient("test-token", "acc-id").
		WithAPIURLOverride(srv.URL).
		WithTimeout(5 * time.Second)

	audio, mime, err := client.Synthesize(context.Background(), "你好，世界", SynthesizeOpts{})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if mime != "audio/wav" {
		t.Errorf("mime = %q, want audio/wav", mime)
	}
	if len(audio) != len(want) {
		t.Errorf("audio len = %d, want %d", len(audio), len(want))
	}
	if !strings.HasPrefix(string(audio), "RIFF") {
		t.Errorf("audio does not start with RIFF: %q", audio[:min(4, len(audio))])
	}
}

// TestCFTTSClient_APIError verifies the error branch when the server
// returns HTTP 200 with success=false — a common Workers AI failure
// mode (quota exhaustion, model timeout, etc.).
func TestCFTTSClient_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := cfTTSResponse{
			Success: false,
			Errors: []cfTTSErrorEntry{
				{Code: 7003, Message: "Neuron budget exceeded"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewCFTTSClient("test-token", "acc-id").
		WithAPIURLOverride(srv.URL).
		WithTimeout(5 * time.Second)

	_, _, err := client.Synthesize(context.Background(), "test", SynthesizeOpts{})
	if err == nil {
		t.Fatal("Synthesize should have returned an error but did not")
	}
	if !strings.Contains(err.Error(), "Neuron budget exceeded") {
		t.Errorf("error missing CF message: %v", err)
	}
	if !strings.Contains(err.Error(), "7003") {
		t.Errorf("error missing CF error code: %v", err)
	}
}

// TestCFTTSClient_HTTP500 verifies the 5xx branch — transport-level
// failure with a body we can show the operator in logs.
func TestCFTTSClient_HTTP500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":9999,"message":"backend timeout"}]}`))
	}))
	defer srv.Close()

	client := NewCFTTSClient("test-token", "acc-id").
		WithAPIURLOverride(srv.URL).
		WithTimeout(5 * time.Second)

	_, _, err := client.Synthesize(context.Background(), "test", SynthesizeOpts{})
	if err == nil {
		t.Fatal("Synthesize should have returned an error but did not")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error missing HTTP status: %v", err)
	}
}

// TestCFTTSClient_EmptyAudio guards against the case where
// success=true but the result.audio field is blank — an unexpected
// upstream response we must treat as failure.
func TestCFTTSClient_EmptyAudio(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := cfTTSResponse{Success: true}
		resp.Result.Audio = ""
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewCFTTSClient("test-token", "acc-id").
		WithAPIURLOverride(srv.URL).
		WithTimeout(5 * time.Second)

	_, _, err := client.Synthesize(context.Background(), "test", SynthesizeOpts{})
	if err == nil {
		t.Fatal("Synthesize should have returned an error for empty audio")
	}
	if !strings.Contains(err.Error(), "empty audio") {
		t.Errorf("error did not mention empty audio: %v", err)
	}
}

// TestCFTTSClient_MissingToken guards against silent auth
// misconfiguration. NewCFTTSClient("", ...) must surface an error
// before any network call.
func TestCFTTSClient_MissingToken(t *testing.T) {
	client := NewCFTTSClient("", "acc")
	_, _, err := client.Synthesize(context.Background(), "test", SynthesizeOpts{})
	if err == nil {
		t.Fatal("expected error for missing token")
	}
	if !strings.Contains(err.Error(), "CF_API_TOKEN") {
		t.Errorf("error did not mention CF_API_TOKEN: %v", err)
	}
}

// TestDetectAudioMime exercises every branch of the sniffer so we
// don't silently regress mime detection when swapping providers.
func TestDetectAudioMime(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"wav", []byte("RIFF\x00\x00\x00\x00WAVE"), "audio/wav"},
		{"mp3-id3", []byte("ID3\x04\x00\x00\x00\x00"), "audio/mpeg"},
		{"mp3-frame", []byte{0xFF, 0xFB, 0x90, 0x44}, "audio/mpeg"},
		{"ogg", []byte("OggS\x00\x02\x00"), "audio/ogg"},
		{"flac", []byte("fLaC\x00\x00\x00\x00"), "audio/flac"},
		{"unknown", []byte("XYZ\x00\x00"), "application/octet-stream"},
		{"too-short", []byte("RI"), "application/octet-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectAudioMime(tc.in)
			if got != tc.want {
				t.Errorf("detectAudioMime(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// min is a tiny local helper for the Success test. Go 1.21+ has
// builtin min but keeping it local avoids coupling tests to the
// toolchain version.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
