.PHONY: build

DGRAPH_VERSION ?= latest
build:
	docker build -t dgraph/standalone:$(DGRAPH_VERSION) --build-arg DGRAPH_VERSION=$(DGRAPH_VERSION) .
