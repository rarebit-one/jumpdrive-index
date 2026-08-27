package ingest

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ExecCapability fetches metadata with yt-dlp and a best-effort transcript with
// Whisper, OUT OF PROCESS (heyarr ADR-0025 shape) so the main binary stays
// pure-Go / CGO-off / distroless-safe. It is built but NOT exercised in CI (the
// binaries are absent there); its two degradation modes are deliberate:
//
//   - yt-dlp missing  → Fetch returns ErrCapabilityUnavailable (no metadata, no
//     node — the caller cannot invent a video),
//   - Whisper missing → Fetch returns metadata with an empty Transcript (a
//     metadata-only node — a valid, degraded ingestion).
type ExecCapability struct {
	// YtDlpPath and WhisperPath are the executables to invoke; defaults are the
	// bare command names (resolved via PATH). WorkDir is where audio/transcripts
	// are staged; empty means a per-call temp dir.
	YtDlpPath   string
	WhisperPath string
	WorkDir     string
}

// NewExecCapability returns an ExecCapability with default command names.
func NewExecCapability() *ExecCapability {
	return &ExecCapability{YtDlpPath: "yt-dlp", WhisperPath: "whisper"}
}

// ytDlpInfo is the subset of yt-dlp -J output ingest reads. yt-dlp emits far
// more; unknown fields are ignored so it can grow without breaking this parse.
type ytDlpInfo struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	UploadDate  string `json:"upload_date"` // "YYYYMMDD"
	WebpageURL  string `json:"webpage_url"`
	ChannelID   string `json:"channel_id"`
	Channel     string `json:"channel"`
	ChannelURL  string `json:"channel_url"`
}

// Fetch runs yt-dlp for metadata and, when Whisper is present, a transcript.
func (c *ExecCapability) Fetch(ctx context.Context, ref string) (Metadata, error) {
	ytDlp := c.YtDlpPath
	if ytDlp == "" {
		ytDlp = "yt-dlp"
	}
	if _, err := exec.LookPath(ytDlp); err != nil {
		return Metadata{}, ErrCapabilityUnavailable
	}

	//nolint:gosec // G204: invoking the operator-configured yt-dlp on the caller's video ref IS this capability's purpose; the binary is not attacker-controlled and ref is a single positional arg.
	out, err := exec.CommandContext(ctx, ytDlp, "-J", "--no-warnings", ref).Output()
	if err != nil {
		return Metadata{}, ErrCapabilityUnavailable
	}
	var info ytDlpInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return Metadata{}, err
	}

	md := Metadata{
		VideoID:     info.ID,
		Title:       info.Title,
		URL:         info.WebpageURL,
		Description: info.Description,
		UploadDate:  isoDate(info.UploadDate),
		ChannelID:   info.ChannelID,
		ChannelName: info.Channel,
		ChannelURL:  info.ChannelURL,
	}
	md.Transcript = c.transcribe(ctx, ref) // best-effort; empty on any failure
	return md, nil
}

// transcribe stages the audio with yt-dlp and runs Whisper on it, returning the
// transcript text. It is entirely best-effort: a missing Whisper or any failure
// yields an empty string (a metadata-only node), never an error.
func (c *ExecCapability) transcribe(ctx context.Context, ref string) string {
	whisper := c.WhisperPath
	if whisper == "" {
		whisper = "whisper"
	}
	if _, err := exec.LookPath(whisper); err != nil {
		return ""
	}
	dir := c.WorkDir
	if dir == "" {
		tmp, err := os.MkdirTemp("", "jdx-ingest-*")
		if err != nil {
			return ""
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		dir = tmp
	}

	audio := filepath.Join(dir, "audio.wav")
	//nolint:gosec // G204: operator-configured yt-dlp on the caller's ref, staging audio into our own temp dir — the capability's purpose, not tainted input.
	dl := exec.CommandContext(ctx, c.YtDlpPath, "-x", "--audio-format", "wav", "-o", audio, ref)
	if err := dl.Run(); err != nil {
		return ""
	}
	//nolint:gosec // G204: operator-configured Whisper on audio we just staged in our own temp dir.
	w := exec.CommandContext(ctx, whisper, audio, "--output_format", "txt", "--output_dir", dir)
	if err := w.Run(); err != nil {
		return ""
	}
	//nolint:gosec // G304: reading Whisper's output from a fixed filename inside the temp dir we created.
	txt, err := os.ReadFile(filepath.Join(dir, "audio.txt"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(txt))
}

// isoDate converts yt-dlp's "YYYYMMDD" to a schema.org "YYYY-MM-DD" date, leaving
// any other shape untouched.
func isoDate(s string) string {
	if len(s) != 8 {
		return s
	}
	return s[0:4] + "-" + s[4:6] + "-" + s[6:8]
}

// StaticCapability returns a fixed Metadata regardless of ref (the ref fills in
// VideoID when unset). It backs the tests and a toolchain-free path where the
// caller supplies metadata/transcript directly, so the ingestion graph logic is
// exercised without yt-dlp/Whisper.
type StaticCapability struct {
	Meta Metadata
}

// Fetch returns the preset metadata, defaulting VideoID to ref.
func (c StaticCapability) Fetch(_ context.Context, ref string) (Metadata, error) {
	m := c.Meta
	if m.VideoID == "" {
		m.VideoID = ref
	}
	return m, nil
}
