package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
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

	for update := range updates {
		if update.Message == nil {
			continue
		}

		if update.Message.Video != nil {
			handleVideo(bot, update.Message)
		}
	}
}

func handleVideo(bot *tgbotapi.BotAPI, message *tgbotapi.Message) {
	// Download the video file
	fileID := message.Video.FileID
	file, err := bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		log.Println("Error getting file:", err)
		return
	}

	inputPath := filepath.Join(os.TempDir(), "input.mp4")
	err = downloadFile(bot, file.FilePath, inputPath)
	if err != nil {
		log.Println("Error downloading file:", err)
		return
	}
	defer os.Remove(inputPath)

	// Process the video to make it circular
	outputPath := filepath.Join(os.TempDir(), "output.mp4")
	err = makeCircularVideo(inputPath, outputPath)
	if err != nil {
		log.Println("Error processing video:", err)
		return
	}
	defer os.Remove(outputPath)

	// Send the processed video as a video note
	videoNote := tgbotapi.NewVideoNote(message.Chat.ID, 640, tgbotapi.FilePath(outputPath))
	_, err = bot.Send(videoNote)
	if err != nil {
		log.Println("Error sending video note:", err)
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

func makeCircularVideo(inputPath, outputPath string) error {
	cmd := exec.Command(
		"ffmpeg",
		"-i", inputPath,
		"-vf", "crop=min(iw\\,ih):min(iw\\,ih),scale=640:640,format=yuv420p",
		"-c:a", "copy",
		"-y",
		outputPath,
	)
	return cmd.Run()
}
