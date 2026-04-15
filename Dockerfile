# Stage 1: build
FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod .
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o site-manager .

# Stage 2: runtime
FROM alpine:3.19
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /app/site-manager .
COPY --from=builder /app/templates ./templates
EXPOSE 8080
CMD ["./site-manager"]
