package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	botpkg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const (
	defaultVideoSize = 640
	// Telegram sends videos longer than this as a regular video, not a circle.
	maxVideoNoteSeconds = 60
	// Bot API getFile refuses files above 20 MB.
	maxDownloadBytes       = 20 << 20
	voiceMsgRestrictionErr = "VOICE_MESSAGES_FORBIDDEN"
	shutdownTimeout        = 25 * time.Second
)

type app struct {
	bot     *botpkg.Bot
	secret  string
	handle  func(ctx context.Context, message *models.Message)
	jobs    sync.WaitGroup
	workers chan struct{}
	// jobCtx outlives the shutdown signal so running jobs can finish.
	jobCtx context.Context
}

func main() {
	botToken := os.Getenv("BOT_TOKEN")
	if botToken == "" {
		log.Fatal("BOT_TOKEN environment variable is not set")
	}

	webhookURL := os.Getenv("WEBHOOK_URL")
	if webhookURL == "" {
		log.Fatal("WEBHOOK_URL environment variable is not set")
	}

	webhookSecret := os.Getenv("WEBHOOK_SECRET_TOKEN")
	if webhookSecret == "" {
		log.Fatal("WEBHOOK_SECRET_TOKEN environment variable is not set")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	b, err := botpkg.New(botToken)
	if err != nil {
		log.Fatal(err)
	}

	a := &app{
		bot:     b,
		secret:  webhookSecret,
		workers: make(chan struct{}, runtime.NumCPU()),
		jobCtx:  context.WithoutCancel(ctx),
	}
	a.handle = a.handleMessage

	_, err = b.SetWebhook(ctx, &botpkg.SetWebhookParams{
		URL:         webhookURL,
		SecretToken: webhookSecret,
	})
	if err != nil {
		log.Fatalf("Failed to set webhook: %v", err)
	}
	log.Printf("Webhook set to %s", webhookURL)

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", a.webhook)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	server := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		log.Printf("Listening for webhook on :%s", port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Failed to start HTTP server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Received shutdown signal. Waiting for running jobs...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)

	done := make(chan struct{})
	go func() {
		a.jobs.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Println("All jobs finished. Bye.")
	case <-shutdownCtx.Done():
		log.Println("Shutdown timeout. Exiting with jobs still running.")
	}
}

// webhook registers the job before answering 200, so server.Shutdown
// cannot return while an acknowledged update is still untracked.
func (a *app) webhook(w http.ResponseWriter, r *http.Request) {
	got := r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(a.secret)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var update models.Update
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&update); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if update.Message == nil {
		return
	}

	a.jobs.Add(1)
	go func() {
		defer a.jobs.Done()
		a.handle(a.jobCtx, update.Message)
	}()
}

func (a *app) handleMessage(ctx context.Context, message *models.Message) {
	if message.Video == nil && message.Document == nil {
		sendText(ctx, a.bot, message.Chat.ID, "Please send a video file to make it circular.")
		return
	}
	a.handleVideo(ctx, a.bot, message)
}

type videoInput struct {
	fileID   string
	mimeType string
	size     int64
}

func videoFromMessage(message *models.Message) (videoInput, string) {
	var in videoInput
	switch {
	case message.Video != nil:
		in = videoInput{message.Video.FileID, message.Video.MimeType, message.Video.FileSize}
		if in.mimeType == "" {
			in.mimeType = "video/mp4"
		}
	case message.Document != nil:
		in = videoInput{message.Document.FileID, message.Document.MimeType, message.Document.FileSize}
	default:
		return in, "Please send a valid video file."
	}

	if in.mimeType != "" && !strings.HasPrefix(in.mimeType, "video/") {
		return in, "This file is not a video. Please send a video file."
	}
	if in.size > maxDownloadBytes {
		return in, "This video is larger than 20 MB, which is the most Telegram lets bots download. Please send a shorter or smaller video."
	}
	return in, ""
}

func (a *app) handleVideo(ctx context.Context, b *botpkg.Bot, message *models.Message) {
	chatID := message.Chat.ID

	in, reply := videoFromMessage(message)
	if reply != "" {
		sendText(ctx, b, chatID, reply)
		return
	}

	a.workers <- struct{}{}
	defer func() { <-a.workers }()

	file, err := b.GetFile(ctx, &botpkg.GetFileParams{FileID: in.fileID})
	if err != nil {
		log.Println("Error getting file:", err)
		sendText(ctx, b, chatID, "Failed to process the video. Please try again.")
		return
	}

	inputPath, err := tempPath("input_*")
	if err != nil {
		log.Println("Error creating temp file:", err)
		sendText(ctx, b, chatID, "Failed to process the video. Please try again.")
		return
	}
	defer os.Remove(inputPath)

	log.Printf("Downloading video for chat %d", chatID)
	err = downloadFile(ctx, b.FileDownloadLink(file), inputPath)
	if err != nil {
		log.Println("Error downloading file:", err)
		sendText(ctx, b, chatID, "Failed to download the video. Please try again.")
		return
	}

	sendText(ctx, b, chatID, "Video downloaded. Processing...")

	outputPath, err := tempPath("output_*.mp4")
	if err != nil {
		log.Println("Error creating temp file:", err)
		sendText(ctx, b, chatID, "Failed to process the video. Please try again.")
		return
	}
	defer os.Remove(outputPath)

	err = makeCircularVideo(ctx, inputPath, outputPath)
	if err != nil {
		log.Println("Error processing video:", err)
		sendText(ctx, b, chatID, "Failed to process the video. Please try again.")
		return
	}

	_, _ = b.SendChatAction(ctx, &botpkg.SendChatActionParams{
		ChatID: chatID,
		Action: models.ChatActionUploadVideoNote,
	})

	f, err := os.Open(outputPath)
	if err != nil {
		log.Println("Error opening output file:", err)
		sendText(ctx, b, chatID, "Failed to open the processed video. Please try again.")
		return
	}
	defer f.Close()

	sent, err := b.SendVideoNote(ctx, &botpkg.SendVideoNoteParams{
		ChatID: chatID,
		VideoNote: &models.InputFileUpload{
			Filename: "circle.mp4",
			Data:     f,
		},
		Length: defaultVideoSize,
	})
	if err != nil {
		log.Println("Error sending video note:", err)
		if isVoiceForbidden(err) {
			sendText(ctx, b, chatID, "It seems that I don't have permission to send video notes. Please check if you allow sending voice messages in the settings.")
		} else {
			sendText(ctx, b, chatID, "Failed to send the processed video. Please try again.")
		}
		return
	}
	if sent.VideoNote == nil {
		log.Printf("Telegram did not accept chat %d's video as a video note", chatID)
		return
	}
	log.Printf("Sent video note to chat %d (%ds)", chatID, sent.VideoNote.Duration)
}

func isVoiceForbidden(err error) bool {
	return err != nil && strings.Contains(err.Error(), voiceMsgRestrictionErr)
}

func tempPath(pattern string) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	return f.Name(), f.Close()
}

func downloadFile(ctx context.Context, link, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		// The URL holds the bot token; drop it from the error.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return urlErr.Err
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: %s", resp.Status)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(resp.Body, maxDownloadBytes+1)); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func ffmpegArgs(inputPath, outputPath string) []string {
	return []string{
		"-loglevel", "error",
		"-i", inputPath,
		"-t", fmt.Sprint(maxVideoNoteSeconds),
		"-vf", fmt.Sprintf("crop=min(iw\\,ih):min(iw\\,ih),scale=%d:%d,format=yuv420p", defaultVideoSize, defaultVideoSize),
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "26",
		"-c:a", "aac", "-b:a", "96k",
		"-movflags", "+faststart",
		"-y",
		outputPath,
	}
}

func makeCircularVideo(ctx context.Context, inputPath, outputPath string) error {
	out, err := exec.CommandContext(ctx, "ffmpeg", ffmpegArgs(inputPath, outputPath)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func sendText(ctx context.Context, b *botpkg.Bot, chatID int64, text string) {
	_, err := b.SendMessage(ctx, &botpkg.SendMessageParams{
		ChatID: chatID,
		Text:   text,
	})
	if err != nil {
		log.Println("Error sending message:", err)
	}
}
