FROM golang:1.23-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /circles .

FROM alpine:3.20

RUN apk add --no-cache ffmpeg ca-certificates && adduser -D -H bot
COPY --from=build /circles /usr/local/bin/circles
USER bot
ENV PORT=8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -q -O /dev/null http://127.0.0.1:${PORT}/healthz || exit 1
CMD ["circles"]
