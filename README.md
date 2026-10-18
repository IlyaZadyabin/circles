# Circles

Circles is a Go project that uses the Telegram Bot API to create a bot which converts videos sent to it into circles.

## Prerequisites

- A Telegram bot token

## Configuration

Create a `.env` file in the root directory:

    ```
    BOT_TOKEN=your-telegram-bot-token
    WEBHOOK_URL=https://your-domain/webhook
    WEBHOOK_SECRET_TOKEN=random-string
    ```
   
## Usage

### Run the bot locally
```sh
docker-compose up --build

Health check: `GET /healthz` returns 200.
