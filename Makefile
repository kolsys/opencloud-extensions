EXTENSIONS := video-thumbnails file-activity
MODULES := common $(EXTENSIONS)
BIN_DIR := $(CURDIR)/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
COMPOSE := docker compose -f dev/docker-compose.yml --env-file dev/.env

.PHONY: all build web test test-stand lint lint-api fmt tidy images up down logs clean

all: build

build:
	@mkdir -p $(BIN_DIR)
	@for e in $(EXTENSIONS); do \
		echo "build $$e"; \
		(cd $$e && go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$$e ./cmd/$$e) || exit 1; \
	done

# Builds the web app of video-thumbnails into the directory the binary embeds.
# Needs node; the image builds it on its own. Without it the binary serves no
# web app and says so at start.
web:
	cd video-thumbnails/web-app && npm ci && npm run check && npm run build
	touch video-thumbnails/internal/web/dist/.gitkeep

test:
	@for m in $(MODULES); do \
		echo "test $$m"; \
		(cd $$m && go test ./...) || exit 1; \
	done

# Runs the tests that need a running stand, inside the docker network where
# the internal ports of the platform are reachable. The tests skip themselves
# when the variables they need are unset.
test-stand:
	@set -a; . ./dev/.env; set +a; \
	docker run --rm --network opencloud-extensions_opencloud-net \
		-v "$(CURDIR)":/src -w /src/common \
		-v opencloud-extensions-gocache:/go/pkg/mod \
		-e GOWORK=off -e GOFLAGS=-mod=mod \
		-e OC_EVENTS_ENDPOINT=nats://opencloud:9233 \
		-e OC_GATEWAY_GRPC_ADDR=opencloud:9142 \
		-e OC_SERVICE_ACCOUNT_ID="$$SERVICE_ACCOUNT_ID" \
		-e OC_SERVICE_ACCOUNT_SECRET="$$SERVICE_ACCOUNT_SECRET" \
		-e PLATFORM_INTERNAL_URL=https://opencloud:9200 \
		-e STAND_USER=alan \
		-e STAND_TOKEN="$$(./dev/scripts/token.sh alan 1h 2>/dev/null)" \
		-e VIDEO_THUMBNAILS_S3_ENDPOINT=http://minio:9000 \
		-e VIDEO_THUMBNAILS_S3_REGION="$$S3_REGION" \
		-e VIDEO_THUMBNAILS_S3_BUCKET="$$S3_THUMBS_BUCKET" \
		-e VIDEO_THUMBNAILS_S3_ACCESS_KEY="$$S3_ACCESS_KEY" \
		-e VIDEO_THUMBNAILS_S3_SECRET_KEY="$$S3_SECRET_KEY" \
		golang:1.26-alpine go test ./...

lint:
	@for m in $(MODULES); do \
		echo "lint $$m"; \
		(cd $$m && golangci-lint run ./...) || exit 1; \
	done

# Validates the descriptions of the APIs. The drift between a description
# and its handlers is caught by go test, this catches the description itself.
lint-api:
	@for s in $(wildcard */api/openapi.yaml); do \
		echo "lint-api $$s"; \
		npx --yes @redocly/cli@latest lint "$$s" || exit 1; \
	done

fmt:
	@for m in $(MODULES); do \
		echo "fmt $$m"; \
		(cd $$m && golangci-lint fmt ./...) || exit 1; \
	done

tidy:
	@for m in $(MODULES); do \
		echo "tidy $$m"; \
		(cd $$m && GOWORK=off go mod tidy) || exit 1; \
	done
	go work sync

images:
	@for e in $(EXTENSIONS); do \
		echo "image $$e"; \
		docker build -f $$e/Dockerfile -t opencloud-extensions/$$e:$(VERSION) --build-arg VERSION=$(VERSION) . || exit 1; \
	done

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f


clean:
	rm -rf $(BIN_DIR)
