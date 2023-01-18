all: build

build:
	docker build -t mheers/pulumi-helper:latest .

push:
	docker push mheers/pulumi-helper:latest
