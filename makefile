# Define variables
BINARY_NAME=hfget
CMD_PATH=./cmd
# Go paths
GOPATH=$(shell go env GOPATH)
GOBIN=$(shell go env GOBIN)

# Default GOBIN to GOPATH/bin if it's not set
ifeq ($(GOBIN),)
    GOBIN=$(GOPATH)/bin
endif

## install: builds and installs the application in your GOBIN
install:
	@echo "Building $(BINARY_NAME) for your system..."
	@go build -o $(BINARY_NAME) $(CMD_PATH)
	@echo "Installing $(BINARY_NAME) to $(GOBIN)/..."
	@mv $(BINARY_NAME) $(GOBIN)/

## help: prints this help message
help:
	@echo "Usage: make <target>"
	@echo ""
	@echo "Targets:"
	@fgrep "##" Makefile | fgrep -v fgrep

.PHONY: install help
