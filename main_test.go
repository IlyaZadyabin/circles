package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	botpkg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func probe(t *testing.T, path, entry string) string {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", entry, "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

func TestMakeCircularVideoCapsDurationAndOutputsMP4(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "long.webm")
	gen := exec.Command("ffmpeg", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=320x180:rate=10",
		"-f", "lavfi", "-i", "sine", "-t", "75", "-c:v", "libvpx", "-c:a", "libopus", input)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generate input: %v %s", err, out)
	}

	output := filepath.Join(dir, "out.mp4")
	if err := makeCircularVideo(context.Background(), input, output); err != nil {
		t.Fatal(err)
	}

	duration, _ := strconv.ParseFloat(probe(t, output, "format=duration"), 64)
	if duration > maxVideoNoteSeconds+0.5 {
		t.Errorf("duration = %.1fs, want <= %ds", duration, maxVideoNoteSeconds)
	}
	if got := probe(t, output, "stream=codec_name"); got != "h264\naac" {
		t.Errorf("codecs = %q, want h264 and aac", got)
	}
	if got := probe(t, output, "stream=width,height"); !strings.HasPrefix(got, "640,640") {
		t.Errorf("size = %q, want 640,640", got)
	}
}

func TestIsVoiceForbiddenMatchesLibraryError(t *testing.T) {
	// Same format go-telegram/bot builds for a 400 reply in raw_request.go.
	err := fmt.Errorf("%w, %s", botpkg.ErrorBadRequest, "Bad Request: VOICE_MESSAGES_FORBIDDEN")
	if !isVoiceForbidden(err) {
		t.Errorf("isVoiceForbidden(%q) = false", err)
	}
	if isVoiceForbidden(fmt.Errorf("%w, %s", botpkg.ErrorBadRequest, "Bad Request: chat not found")) {
		t.Error("matched an unrelated error")
	}
}

func TestVideoFromMessage(t *testing.T) {
	tests := []struct {
		name      string
		msg       *models.Message
		wantReply bool
	}{
		{"video", &models.Message{Video: &models.Video{FileID: "a", FileSize: 1 << 20}}, false},
		{"video document", &models.Message{Document: &models.Document{FileID: "a", MimeType: "video/webm", FileSize: 1 << 20}}, false},
		{"document without mime type", &models.Message{Document: &models.Document{FileID: "a", FileName: "clip.mp4", FileSize: 1 << 20}}, false},
		{"pdf document", &models.Message{Document: &models.Document{FileID: "a", MimeType: "application/pdf", FileSize: 1000}}, true},
		{"over 20 MB", &models.Message{Video: &models.Video{FileID: "a", FileSize: 27 << 20}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, reply := videoFromMessage(tt.msg)
			if (reply != "") != tt.wantReply {
				t.Errorf("reply = %q, want reply: %v", reply, tt.wantReply)
			}
		})
	}
}

func TestAcknowledgedUpdateIsTrackedBeforeResponse(t *testing.T) {
	a := &app{workers: make(chan struct{}, 1), jobCtx: context.Background(), secret: "s"}
	block := make(chan struct{})
	a.handle = func(context.Context, *models.Message) { <-block }

	req := httptest.NewRequest(http.MethodPost, "/webhook",
		strings.NewReader(`{"update_id":1,"message":{"message_id":1,"date":0,"chat":{"id":1,"type":"private"},"video":{"file_id":"f","file_unique_id":"u","width":1,"height":1,"duration":1}}}`))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "s")
	rec := httptest.NewRecorder()
	a.webhook(rec, req)

	done := make(chan struct{})
	go func() { a.jobs.Wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("job was not tracked when the webhook returned 200")
	case <-time.After(100 * time.Millisecond):
	}
	close(block)
	<-done
}

func TestWebhookRejectsWrongSecret(t *testing.T) {
	a := &app{secret: "s"}
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{}`))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "wrong")
	rec := httptest.NewRecorder()
	a.webhook(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAcquireGivesUpWhenJobDeadlinePasses(t *testing.T) {
	a := &app{workers: make(chan struct{}, 1)}
	a.workers <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	got := make(chan bool, 1)
	go func() { got <- a.acquire(ctx) }()
	select {
	case ok := <-got:
		if ok {
			t.Fatal("acquired a slot that was taken")
		}
	case <-time.After(time.Second):
		t.Fatal("acquire ignored the job deadline")
	}
}

func TestTempPathIsUniquePerJob(t *testing.T) {
	a, err := tempPath("output_*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	b, err := tempPath("output_*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two jobs got the same path %s", a)
	}
}

func TestFFmpegArgsUseMediumQuality(t *testing.T) {
	args := strings.Join(ffmpegArgs("in", "out.mp4"), " ")
	for _, want := range []string{"-preset medium", "-crf 23", "-b:a 128k"} {
		if !strings.Contains(args, want) {
			t.Errorf("ffmpeg args %q missing %q", args, want)
		}
	}
}
