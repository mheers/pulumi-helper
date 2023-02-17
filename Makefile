all: build

build: docker

docker: ##  Builds the application for amd64 and arm64
	docker buildx build --platform linux/amd64,linux/arm64 -t mheers/pulumi-helper:latest --push .
