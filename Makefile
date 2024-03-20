all: help

build: docker

docker: ##  Builds the application for amd64 and arm64
	$(eval VERSION := $(shell cat .VERSION | grep VERSION | cut -d'=' -f2))
	docker buildx build --platform linux/amd64,linux/arm64 -t mheers/pulumi-helper:$(VERSION) --push .
	docker buildx build --platform linux/amd64,linux/arm64 -t mheers/pulumi-helper:latest --push .

##@ Testing
test-staticcheck: ## Starts staticcheck tests
	go install honnef.co/go/tools/cmd/staticcheck@latest
	staticcheck ./...

help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[0-9a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
