VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test lint run docker bench tf-check k8s-check

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/switchyard ./cmd/switchyard

test:
	go test -race ./...

lint:
	gofmt -l . | (! grep .)
	go vet ./...

run: build
	./bin/switchyard serve -config switchyard.yaml

docker:
	docker build --build-arg VERSION=$(VERSION) -t switchyard:$(VERSION) .

bench:
	go test -run '^$$' -bench . -benchmem ./internal/proxy

tf-check:
	terraform -chdir=deploy/terraform/gcp fmt -check -recursive
	terraform -chdir=deploy/terraform/gcp init -backend=false -input=false >/dev/null
	terraform -chdir=deploy/terraform/gcp validate

k8s-check:
	kubectl kustomize deploy/kubernetes | kubeconform -strict -summary -kubernetes-version 1.33.0 -
