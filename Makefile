.PHONY: dev-hub dev-agent dev-web build test fmt proto

dev-hub:
	go run ./apps/hub

dev-agent:
	go run ./apps/agent

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
	protoc --descriptor_set_out=/tmp/sinthmux-protocol.pb proto/sinthmux/v1/agent.proto
