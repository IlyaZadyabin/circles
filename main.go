package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	botpkg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const (
	defaultVideoSize       = 640
	voiceMsgRestrictionErr = "Bad Request: VOICE_MESSAGES_FORBIDDEN"
)

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

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		log.Println("Received shutdown signal. Closing bot...")
		cancel()
	}()

	botOptions := []botpkg.Option{botpkg.WithDefaultHandler(defaultHandler)}
	if webhookSecret != "" {
		botOptions = append(botOptions, botpkg.WithWebhookSecretToken(webhookSecret))
	}

	b, err := botpkg.New(botToken, botOptions...)
	if err != nil {
		log.Panic(err)
	}

	// Set webhook
	setWebhookParams := &botpkg.SetWebhookParams{
		URL: webhookURL,
	}
	if webhookSecret != "" {
		setWebhookParams.SecretToken = webhookSecret
	}
	_, err = b.SetWebhook(ctx, setWebhookParams)
	if err != nil {
		log.Panicf("Failed to set webhook: %v", err)
	}

	// Start webhook server
	go b.StartWebhook(ctx)

	log.Printf("Listening for webhook on :%s", port)
	http.Handle("/webhook", b.WebhookHandler())
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Failed to start HTTP server: %v", err)
	}
}

func defaultHandler(ctx context.Context, b *botpkg.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	if update.Message.Video != nil || update.Message.Document != nil {
		go handleVideo(ctx, b, update.Message)
	} else {
		msg := &botpkg.SendMessageParams{
			ChatID: update.Message.Chat.ID,
			Text:   "Please send a video file to make it circular.",
		}
		_, _ = b.SendMessage(ctx, msg)
	}
}

func handleVideo(ctx context.Context, b *botpkg.Bot, message *models.Message) {
	chatID := message.Chat.ID
	var fileID string
	var fileName string

	if message.Video != nil {
		fileID = message.Video.FileID
		fileName = message.Video.FileName
	} else if message.Document != nil {
		fileID = message.Document.FileID
		fileName = message.Document.FileName
	} else {
		sendErrorMessage(ctx, b, chatID, "Please send a valid video file.")
		return
	}

	if fileName == "" {
		fileName = "video.mp4"
	} else if filepath.Ext(fileName) == "" {
		fileName += ".mp4"
	}

	file, err := b.GetFile(ctx, &botpkg.GetFileParams{FileID: fileID})
	if err != nil {
		log.Println("Error getting file:", err)
		sendErrorMessage(ctx, b, chatID, "Failed to process the video. Please try again.")
		return
	}

	inputPath := filepath.Join(os.TempDir(), fmt.Sprintf("input_%d_%s", chatID, fileName))
	log.Println("Downloading video to", inputPath)
	err = downloadFile(b, file, inputPath)
	if err != nil {
		log.Println("Error downloading file:", err)
		sendErrorMessage(ctx, b, chatID, "Failed to download the video. Please try again.")
		return
	}
	defer os.Remove(inputPath)

	sendProgressMessage(ctx, b, chatID, "Video downloaded. Processing...")

	outputPath := filepath.Join(os.TempDir(), "output_"+fileName)
	err = makeCircularVideo(ctx, inputPath, outputPath)
	if err != nil {
		log.Println("Error processing video:", err)
		sendErrorMessage(ctx, b, chatID, "Failed to process the video. Please try again.")
		return
	}
	defer os.Remove(outputPath)

	sendProgressMessage(ctx, b, chatID, "Video processed. Sending...")

	f, err := fileReader(outputPath)
	if err != nil {
		log.Println("Error opening output file:", err)
		sendErrorMessage(ctx, b, chatID, "Failed to open the processed video. Please try again.")
		return
	}
	defer f.Close()

	videoNoteParams := &botpkg.SendVideoNoteParams{
		ChatID: chatID,
		VideoNote: &models.InputFileUpload{
			Filename: fileName,
			Data:     f,
		},
		Length: defaultVideoSize,
	}
	_, err = b.SendVideoNote(ctx, videoNoteParams)
	if err != nil {
		log.Println("Error sending video note:", err)
		if err.Error() == voiceMsgRestrictionErr {
			log.Println("Permission to send video notes is forbidden.")
			sendErrorMessage(ctx, b, chatID, "It seems that I don't have permission to send video notes. Please check if you allow sending voice messages in the settings.")
		} else {
			sendErrorMessage(ctx, b, chatID, "Failed to send the processed video. Please try again.")
		}
	}
}

func downloadFile(b *botpkg.Bot, file *models.File, destPath string) error {
	url := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", b.Token(), file.FilePath)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func makeCircularVideo(ctx context.Context, inputPath, outputPath string) error {
	cmd := exec.CommandContext(ctx,
		"ffmpeg",
		"-i", inputPath,
		"-vf", fmt.Sprintf("crop=min(iw\\,ih):min(iw\\,ih),scale=%d:%d,format=yuv420p", defaultVideoSize, defaultVideoSize),
		"-c:a", "copy",
		"-y",
		outputPath,
	)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	go logFFmpegProgress(stderr)

	return cmd.Wait()
}

func logFFmpegProgress(stderr io.ReadCloser) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		log.Println("FFmpeg:", scanner.Text())
	}
}

func sendErrorMessage(ctx context.Context, b *botpkg.Bot, chatID int64, text string) {
	msg := &botpkg.SendMessageParams{
		ChatID: chatID,
		Text:   text,
	}
	_, _ = b.SendMessage(ctx, msg)
}

func sendProgressMessage(ctx context.Context, b *botpkg.Bot, chatID int64, text string) {
	msg := &botpkg.SendMessageParams{
		ChatID: chatID,
		Text:   text,
	}
	_, _ = b.SendMessage(ctx, msg)
}

func fileReader(path string) (io.ReadCloser, error) {
	return os.Open(path)
}
