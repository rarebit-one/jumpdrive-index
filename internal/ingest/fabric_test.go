package ingest

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rarebit-one/jumpdrive-index/internal/secret"
)

// stageAudio writes a throwaway audio file (a stand-in for yt-dlp's staged wav)
// and returns its path.
func stageAudio(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "audio.wav")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("stage audio: %v", err)
	}
	return p
}

// doerFunc adapts a function to the httpDoer seam, so a test can mock transport
// without a live server (the ingest analogue of embed's WithHTTPClient tests).
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// TestFabricTranscriber_PostsMultipart asserts the transcriber sends the staged
// audio as a multipart "file" part alongside the "model" field and a bearer
// header, then decodes {"text":...} (trimmed).
func TestFabricTranscriber_PostsMultipart(t *testing.T) {
	const audio = "RIFF....fake-wav-bytes"
	var gotModel, gotAuth, gotFile string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "multipart/form-data" {
			t.Fatalf("content-type = %q (%v)", r.Header.Get("Content-Type"), err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("next part: %v", err)
			}
			b, _ := io.ReadAll(part)
			switch part.FormName() {
			case "file":
				gotFile = string(b)
			case "model":
				gotModel = string(b)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"  in space no one can hear you scream  "}`))
	}))
	defer srv.Close()

	ft, err := NewFabricTranscriber(srv.URL, "whisper-1", secret.Value("s3cr3t"))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	text, err := ft.Transcribe(context.Background(), stageAudio(t, audio))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "in space no one can hear you scream" {
		t.Errorf("transcript = %q", text)
	}
	if gotModel != "whisper-1" {
		t.Errorf("model field = %q, want whisper-1", gotModel)
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Errorf("authorization = %q, want Bearer s3cr3t", gotAuth)
	}
	if gotFile != audio {
		t.Errorf("uploaded file = %q, want %q", gotFile, audio)
	}
}

// TestFabricTranscriber_NoTokenNoAuthHeader proves an unset bearer sends no
// Authorization header at all (not an empty one).
func TestFabricTranscriber_NoTokenNoAuthHeader(t *testing.T) {
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		_, _ = w.Write([]byte(`{"text":"hi"}`))
	}))
	defer srv.Close()

	ft, err := NewFabricTranscriber(srv.URL, "whisper-1", "")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := ft.Transcribe(context.Background(), stageAudio(t, "x")); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if hadAuth {
		t.Error("no token → no Authorization header")
	}
}

// TestFabricTranscriber_ServerErrorReturnsError proves a 5xx surfaces as an error
// from the transcriber itself (the ExecCapability then degrades it to empty).
func TestFabricTranscriber_ServerErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	ft, err := NewFabricTranscriber(srv.URL, "whisper-1", "")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := ft.Transcribe(context.Background(), stageAudio(t, "x")); err == nil {
		t.Error("expected an error on a 5xx response")
	}
}

// TestFabricTranscriber_RequiresURLAndModel guards the constructor.
func TestFabricTranscriber_RequiresURLAndModel(t *testing.T) {
	if _, err := NewFabricTranscriber("", "whisper-1", ""); err == nil {
		t.Error("expected an error for an empty base URL")
	}
	if _, err := NewFabricTranscriber("https://farcaster.example", "", ""); err == nil {
		t.Error("expected an error for an empty model")
	}
}

// TestBestEffort_FabricSuccess proves the shared best-effort seam returns the
// trimmed transcript on success.
func TestBestEffort_FabricSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"text":" hello "}`))
	}))
	defer srv.Close()

	ft, err := NewFabricTranscriber(srv.URL, "whisper-1", "")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if got := bestEffortTranscribe(context.Background(), ft, stageAudio(t, "x")); got != "hello" {
		t.Errorf("transcript = %q, want hello", got)
	}
}

// TestBestEffort_Fabric5xxDegradesToEmpty proves a 5xx fabric degrades to an empty
// transcript (a metadata-only node), not an error — the ExecCapability contract.
func TestBestEffort_Fabric5xxDegradesToEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ft, err := NewFabricTranscriber(srv.URL, "whisper-1", "")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if got := bestEffortTranscribe(context.Background(), ft, stageAudio(t, "x")); got != "" {
		t.Errorf("expected empty transcript on a 5xx fabric, got %q", got)
	}
}

// TestBestEffort_FabricUnreachableDegradesToEmpty proves an unreachable fabric
// (transport error, via the WithHTTPClient seam) degrades to empty, not an error.
func TestBestEffort_FabricUnreachableDegradesToEmpty(t *testing.T) {
	ft, err := NewFabricTranscriber("https://farcaster.example", "whisper-1", "",
		WithHTTPClient(doerFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp: connection refused")
		})))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if got := bestEffortTranscribe(context.Background(), ft, stageAudio(t, "x")); got != "" {
		t.Errorf("expected empty transcript on an unreachable fabric, got %q", got)
	}
}
