BINARY := site-manager
IMAGE  := ghcr.io/fwump38/website-manager

.PHONY: build run test clean docker-build docker-push version help

## build: compile the binary
build:
	CGO_ENABLED=1 go build -ldflags '-extldflags "-static"' -o $(BINARY) .

## run: build and run locally
run: build
	./$(BINARY)

## test: run all tests
test:
	go test ./...

## clean: remove compiled binary
clean:
	rm -f $(BINARY)

## docker-build: build the Docker image
docker-build:
	docker build -t $(IMAGE):latest .

## docker-push: push the Docker image
docker-push:
	docker push $(IMAGE):latest

## up: start services with docker compose
up:
	docker compose up -d

## down: stop services with docker compose
down:
	docker compose down

## logs: tail docker compose logs
logs:
	docker compose logs -f

## version: create and push a git tag  (usage: make version 1.2.3)
version:
	$(if $(word 2,$(MAKECMDGOALS)),,$(error usage: make version <version>))
	git tag v$(word 2,$(MAKECMDGOALS))
	git push origin v$(word 2,$(MAKECMDGOALS))

# Absorb the version number argument so make doesn't treat it as a target
$(filter-out version,$(MAKECMDGOALS)):
	@:

## help: list available targets
help:
	@grep -E '^## ' Makefile | sed 's/^## //'
