# The updater drives a real Chrome, since Letterboxd's Cloudflare scores every
# request and lets through only the browser's own: Google Chrome, and Xvfb for
# the display it runs on, as Turnstile refuses a headless Chrome.

FROM golang:1.27-alpine AS builder

RUN apk update \
  && apk upgrade --no-cache \
  && apk add --no-cache git ca-certificates \
  && update-ca-certificates

WORKDIR /usr/src/app

COPY . .
RUN go mod download && go mod verify

RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags="-s -w" -installsuffix cgo -o /usr/src/bin/app

FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      wget gnupg ca-certificates fonts-liberation xvfb xauth \
 && wget -qO- https://dl.google.com/linux/linux_signing_key.pub \
      | gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg \
 && echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] https://dl.google.com/linux/chrome/deb/ stable main" \
      > /etc/apt/sources.list.d/google-chrome.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends google-chrome-stable \
 && rm -rf /var/lib/apt/lists/*

# Xvfb, running as the unprivileged user, makes its socket here; the directory
# has to exist already, as it does on a desktop.
RUN install -d -o 1000 -g 0 -m 0755 /home/app \
 && install -d -m 1777 /tmp/.X11-unix

ENV LETTERBOXD_CHROME_EXEC_PATH=/usr/bin/google-chrome
ENV HOME=/home/app

COPY --from=builder /usr/src/bin/app /app
COPY --chmod=0755 entrypoint.sh /entrypoint.sh

USER 1000
EXPOSE 8080

ENTRYPOINT ["/entrypoint.sh"]
