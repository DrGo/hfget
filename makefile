# Makefile for hfget

# === Variables ===
# The single source of truth for the version number, read from the VERSION file.
VERSION := $(shell sed 's/ / /g' VERSION)

# Variables for local builds and installation.
BINARY_NAME=hfget
CMD_PATH=./cmd
GOPATH=$(shell go env GOPATH)
GOBIN=$(shell go env GOBIN)

# Default GOBIN to GOPATH/bin if it's not set
ifeq ($(GOBIN),)
    GOBIN=$(GOPATH)/bin
endif

# === Targets ===

## help: prints this help message
help:
	@echo "Usage: make <target>"
	@echo ""
	@echo "Targets:"
	@fgrep -h "##" Makefile | fgrep -v fgrep | sed 's/## //'

## build: builds the application binary into the ./bin directory
build:
	@echo "--- Building $(BINARY_NAME) version $(VERSION)..."
	@go build -ldflags="-s -w -X main.VERSION=$(VERSION)" -o dist/$(BINARY_NAME) $(CMD_PATH)

## install: builds and installs the application into your Go bin path
install:
	@echo "--- Building $(BINARY_NAME) version $(VERSION) for installation..."
	@go build -ldflags="-s -w -X main.VERSION=$(VERSION)" -o $(BINARY_NAME) $(CMD_PATH)
	@echo "--- Installing $(BINARY_NAME) to $(GOBIN)/..."
	@mv $(BINARY_NAME) $(GOBIN)/
	@echo "--- $(BINARY_NAME) installed successfully."

## tag: creates and pushes a git tag to trigger a new release
tag:
	@echo "--- Tagging release for version $(VERSION)..."
	@git add VERSION
	@git commit -m "chore: bump version to $(VERSION)"
	@git tag -a $(VERSION) -m "Release $(VERSION)"
	@echo "--- Pushing commit and tag to origin..."
	@git push origin main
	@git push origin $(VERSION)
	@echo "--- Tag $(VERSION) pushed successfully. GitHub Actions will now create the release."

## clean: removes build artifacts
clean:
	@echo "--- Cleaning up..."
	@rm -rf ./dist

.PHONY: help build install tag clean

