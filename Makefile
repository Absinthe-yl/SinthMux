.PHONY: quickstart docker-up docker-down dev-hub dev-connector dev-web build test fmt proto

-include .env
export SINTHMUX_HUB_ADDR SINTHMUX_DEV_TOKEN SINTHMUX_DATABASE_URL SINTHMUX_TEST_DATABASE_URL
export SINTHMUX_PUBLIC_URL SINTHMUX_GITHUB_CLIENT_ID SINTHMUX_GITHUB_CLIENT_SECRET
export SINTHMUX_CONNECTOR_HUB_URL SINTHMUX_CONNECTOR_DEVICE_ID SINTHMUX_CONNECTOR_DEVICE_TOKEN SINTHMUX_CONNECTOR_NAME

quickstart:
	./scripts/quickstart.sh

docker-up:
	./scripts/docker-up.sh

docker-down:
	docker compose --env-file deploy/.env.local -f deploy/docker-compose.yml down

dev-hub:
	go run ./apps/hub

dev-connector:
	go run ./apps/connector

dev-web:
	npm --prefix apps/web run dev

build:
	go build ./...
	npm --prefix apps/web run build

test:
	go test ./...
	npm --prefix apps/web run typecheck

fmt:
	gofmt -w apps internal pkg

proto:
	protoc --descriptor_set_out=/tmp/sinthmux-protocol.pb proto/sinthmux/v1/connector.proto
