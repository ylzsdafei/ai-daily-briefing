package audio

import (
	"context"
	"fmt"
	"time"
)

// SynthesizeWithRetry wraps TTSClient.Synthesize + CheckAudioQuality in
// a retry loop. A single TTS call occasionally returns a corrupted or
// silent blob (e.g. edge-tts transient 'NoAudioReceived', or Azure
// backends emitting low-variance noise on certain inputs) — re-running
// the same request against the same service often succeeds because
// upstream generation has inherent nondeterminism.
//
// Each attempt:
//  1. client.Synthesize
//  2. CheckAudioQuality (variance pre-check) — catches silent output
//     BEFORE it reaches SaveAudio's on-disk overwrite.
// If either step fails, backoff and retry. Backoffs default to
// [5s, 15s] (giving 3 total attempts) if nil is passed.
//
// Returns the first attempt's (bytes, mime, nil). On terminal failure
// returns the most recent attempt's error wrapped with attempt count.
func SynthesizeWithRetry(
	ctx context.Context,
	client TTSClient,
	text string,
	opts SynthesizeOpts,
	backoffs []time.Duration,
) ([]byte, string, error) {
	if len(backoffs) == 0 {
		backoffs = []time.Duration{5 * time.Second, 15 * time.Second}
	}
	maxAttempts := len(backoffs) + 1
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		data, mime, err := client.Synthesize(ctx, text, opts)
		if err != nil {
			lastErr = fmt.Errorf("tts attempt %d: %w", attempt, err)
		} else if _, qErr := CheckAudioQuality(ctx, data, mime); qErr != nil {
			lastErr = fmt.Errorf("tts attempt %d (quality): %w", attempt, qErr)
		} else {
			return data, mime, nil
		}
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(backoffs[attempt-1]):
			}
		}
	}
	return nil, "", lastErr
}
