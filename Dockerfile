# SOURCE: [Docker minimal image](https://oneuptime.com/blog/post/2026-02-20-go-docker-minimal-image/view)
# Stage 1: Build
FROM golang:1.26-alpine3.23 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o server .

# Stage 2: Distroless static image
FROM gcr.io/distroless/static-debian13:nonroot

# Copy the binary from the builder stage
COPY --from=builder /app/server /server

# Distroless images include a nonroot user by default
USER nonroot:nonroot

EXPOSE 8080
ENTRYPOINT ["/server"]
