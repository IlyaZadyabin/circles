FROM golang:1.23-alpine

RUN apk add --no-cache ffmpeg

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN go build -o main .

EXPOSE 8090

CMD ["./main"]