.PHONY: build-workspace build up down logs test bootstrap

build-workspace:
	docker build -t vibedock/workspace:local ./workspace

build:
	docker compose build

up:
	docker compose up -d

down:
	docker compose down

logs:
	docker compose logs -f

test:
	go test ./...
	go vet ./...

bootstrap:
	./scripts/bootstrap.sh
