# Multi-stage build for JobForge binary.
FROM golang:1.26-alpine AS builder

WORKDIR /src

# Cache dependencies.
COPY go.mod go.sum ./
RUN go mod download

# Build binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/jobforge ./cmd/jobforge

# Minimal runtime image.
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /bin/jobforge /usr/local/bin/jobforge
COPY migrations /app/migrations

EXPOSE 8080

ENTRYPOINT ["jobforge"]
CMD ["api"]
