package main

import (
	"bufio"
	"context"
	"fmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
)

const (
	defaultVideoSize = 640
)

func main() {
	botToken := os.Getenv("BOT_TOKEN")
	if botToken == "" {
		log.Fatal("BOT_TOKEN environment variable is not set")
	}

	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Panic(err)
	}

	bot.Debug = true
	log.Printf("Authorized on account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

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

	for {
		select {
		case update := <-updates:
			if update.Message == nil {
				continue
			}

			if update.Message.Video != nil || update.Message.Document != nil {
				go handleVideo(ctx, bot, update.Message)
			} else {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Please send a video file to make it circular.")
				bot.Send(msg)
			}
		case <-ctx.Done():
			log.Println("Bot is shutting down...")
			return
		}
	}
}

func handleVideo(ctx context.Context, bot *tgbotapi.BotAPI, message *tgbotapi.Message) {
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
		sendErrorMessage(bot, chatID, "Please send a valid video file.")
		return
	}

	// Ensure fileName is not empty and has a valid extension
	if fileName == "" {
		fileName = "video.mp4"
	} else if filepath.Ext(fileName) == "" {
		fileName += ".mp4"
	}

	file, err := bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		log.Println("Error getting file:", err)
		sendErrorMessage(bot, chatID, "Failed to process the video. Please try again.")
		return
	}

	inputPath := filepath.Join(os.TempDir(), fmt.Sprintf("input_%d_%s", chatID, fileName))
	log.Println("Downloading video to", inputPath)
	err = downloadFile(bot, file.FilePath, inputPath)
	if err != nil {
		log.Println("Error downloading file:", err)
		sendErrorMessage(bot, chatID, "Failed to download the video. Please try again.")
		return
	}
	defer os.Remove(inputPath)

	sendProgressMessage(bot, chatID, "Video downloaded. Processing...")

	outputPath := filepath.Join(os.TempDir(), "output_"+fileName)
	err = makeCircularVideo(ctx, inputPath, outputPath)
	if err != nil {
		log.Println("Error processing video:", err)
		sendErrorMessage(bot, chatID, "Failed to process the video. Please try again.")
		return
	}
	defer os.Remove(outputPath)

	sendProgressMessage(bot, chatID, "Video processed. Sending...")

	videoNote := tgbotapi.NewVideoNote(chatID, defaultVideoSize, tgbotapi.FilePath(outputPath))
	_, err = bot.Send(videoNote)
	if err != nil {
		log.Println("Error sending video note:", err)
		sendErrorMessage(bot, chatID, "Failed to send the processed video. Please try again.")
	}
}

func downloadFile(bot *tgbotapi.BotAPI, filePath, destPath string) error {
	url := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", bot.Token, filePath)

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

func sendErrorMessage(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	bot.Send(msg)
}

func sendProgressMessage(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	bot.Send(msg)
}
