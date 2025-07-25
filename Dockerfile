# Build stage
FROM golang:1.24.5-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -o server .

# Final stage
FROM alpine:latest

WORKDIR /app

COPY --from=builder /app/server .

ENV PORT=5070

EXPOSE 5070

CMD ["sh", "-c", "./server -port ${PORT}"]
