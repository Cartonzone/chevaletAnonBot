# Multi-stage build for ChevaletAnonBot (Go).
#
# Build context is the repository root. The same image is used for production
# (docker-compose.yml) and staging (deploy/go/docker-compose.yml).

FROM golang:1.26.11-alpine AS build
WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

# Build a static binary (no cgo) so it runs on a bare alpine image.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/bot ./cmd/bot

FROM alpine:3.20
# ca-certificates: HTTPS to the Telegram & AI endpoints.
# tzdata: Asia/Tehran for the GM/GN greetings (also embedded in the binary).
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app

# The bot fails closed at startup when its backups directory is missing (the
# nightly delivery and /admin backup read it). Staging/sharedtest compose stacks
# do not bind-mount it, so create the empty directory in the image; a mounted
# volume simply overlays it. Running the binary outside Docker needs the same
# (`mkdir -p backups` in the working directory).
RUN mkdir -p /app/backups

COPY --from=build /out/bot /app/bot
# Static message templates are read from ./Texts at runtime.
COPY Texts /app/Texts

ENTRYPOINT ["/app/bot"]
