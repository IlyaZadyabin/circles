FROM golang:1.23-alpine

RUN apk add --no-cache ffmpeg

WORKDIR /app
EXPOSE 8080
ENV PORT 8080

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN go build -o main .

CMD ["./main"]