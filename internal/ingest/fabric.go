package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rarebit-one/jumpdrive-index/internal/secret"
)

// Transcriber turns a staged audio file into transcript text. ExecCapability
// stages the audio with yt-dlp, then hands the path to a Transcriber for the
// speech-to-text step: either the local Whisper subprocess (the default) or the
// fabric transcriber (off-host, ADR-0004). The contract is best-effort — a
// Transcriber error degrades ingestion to a metadata-only node, never a hard
// failure.
type Transcriber interface {
	Transcribe(ctx context.Context, audioPath string) (string, error)
}

// httpDoer is the minimal HTTP surface, for testability (the ingest analogue of
// the embed package's seam).
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// FabricTranscriber routes speech-to-text through the TechnoCore switchboard
// (Farcaster) — an OpenAI-compatible POST /v1/audio/transcriptions endpoint that
// serves the infer.transcribe capability from whichever accelerator (or cloud
// fallback) can run it. It keeps the main binary a thin client: no Whisper runs
// in-process, so the static / CGO-off binary is preserved and the ML weight lives
// on the fabric, not the host — the same ADR-0004 rationale as the local Whisper
// subprocess it re-pins. It is the transcription analogue of the embed.Fabric
// embedder.
type FabricTranscriber struct {
	baseURL string
	model   string
	token   secret.Value
	http    httpDoer
}

// FabricTranscriberOption configures NewFabricTranscriber.
type FabricTranscriberOption func(*FabricTranscriber)

// WithHTTPClient overrides the default HTTP client (used in tests).
func WithHTTPClient(d httpDoer) FabricTranscriberOption {
	return func(f *FabricTranscriber) { f.http = d }
}

// NewFabricTranscriber builds a FabricTranscriber. baseURL is the Farcaster
// switchboard root (e.g. https://farcaster.br.thesim.family); model is the
// transcription model to request (e.g. "whisper-1"); token is the optional bearer
// credential. Construction does no I/O, so the endpoint need not be reachable at
// boot.
func NewFabricTranscriber(baseURL, model string, token secret.Value, opts ...FabricTranscriberOption) (*FabricTranscriber, error) {
	if baseURL == "" || model == "" {
		return nil, fmt.Errorf("ingest: fabric transcriber requires baseURL and model")
	}
	f := &FabricTranscriber{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		token:   token,
		// Transcription is slower than embedding — a full-video audio file may
		// take minutes on the fabric — so this ceiling is generous.
		http: &http.Client{Timeout: 10 * time.Minute},
	}
	for _, opt := range opts {
		opt(f)
	}
	return f, nil
}

// transcriptionResponse is the subset of the OpenAI-compatible
// /v1/audio/transcriptions reply ingest reads; unknown fields are ignored.
type transcriptionResponse struct {
	Text string `json:"text"`
}

// Transcribe POSTs the staged audio file to the fabric as multipart/form-data
// (a "file" part carrying the audio plus a "model" field) and returns the decoded
// transcript. An error is returned on any failure (open, transport, non-200,
// decode) so the caller can decide; ExecCapability treats that as best-effort and
// degrades to a metadata-only node.
func (f *FabricTranscriber) Transcribe(ctx context.Context, audioPath string) (string, error) {
	//nolint:gosec // G304: reading the audio file ExecCapability just staged in its own temp dir, not attacker-controlled input.
	file, err := os.Open(audioPath)
	if err != nil {
		return "", fmt.Errorf("ingest: open staged audio: %w", err)
	}
	defer func() { _ = file.Close() }()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return "", fmt.Errorf("ingest: multipart file part: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return "", fmt.Errorf("ingest: copy audio into request: %w", err)
	}
	if err := mw.WriteField("model", f.model); err != nil {
		return "", fmt.Errorf("ingest: multipart model field: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("ingest: finalise multipart body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.baseURL+"/v1/audio/transcriptions", &body)
	if err != nil {
		return "", fmt.Errorf("ingest: build transcription request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	if !f.token.IsZero() {
		req.Header.Set("Authorization", "Bearer "+f.token.Reveal())
	}

	resp, err := f.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("ingest: fabric transcription request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ingest: fabric transcription status %d", resp.StatusCode)
	}

	var out transcriptionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("ingest: decode fabric transcription: %w", err)
	}
	return strings.TrimSpace(out.Text), nil
}

// bestEffortTranscribe runs a Transcriber over the staged audio, swallowing any
// error into an empty transcript (a metadata-only node). It is the single place
// the best-effort contract lives, so both the local-Whisper and fabric paths — and
// the tests — share one degradation rule.
func bestEffortTranscribe(ctx context.Context, t Transcriber, audioPath string) string {
	text, err := t.Transcribe(ctx, audioPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(text)
}
