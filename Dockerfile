# Stage 1: build
FROM golang:1.25-alpine AS builder
RUN apk add --no-cache gcc musl-dev
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -ldflags '-extldflags "-static"' -o site-manager .

# Stage 2: runtime
FROM alpine:3.19
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /app/site-manager .
COPY --from=builder /app/templates ./templates
EXPOSE 8080
CMD ["./site-manager"]
