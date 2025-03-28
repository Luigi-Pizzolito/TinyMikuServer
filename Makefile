# Makefile for TinyMikuServer and mikv-converter

# Directories
BIN_DIR := ./bin
MIKV_CONVERTER_DIR := ./pkg/mikvformat/cmd/mikv-converter

# Go commands
GO := go
GO_BUILD := $(GO) build

# Targets
all: build-server build-mikv-converter

build-server:
	@echo "Building TinyMikuServer..."
	$(GO_BUILD) -o $(BIN_DIR)/TinyMikuServer .

build-mikv-converter:
	@echo "Building mikv-converter..."
	cd $(MIKV_CONVERTER_DIR) && $(GO_BUILD) -o ../../../../$(BIN_DIR)/mikv-converter

clean:
	@echo "Cleaning up binaries..."
	find $(BIN_DIR) -type f ! -name '.gitkeep' -delete

.PHONY: all build-server build-mikv-converter clean

convert-assets:
	@echo "Converting assets..."
	./bin/mikv-converter -i ./assets/mp4 -o ./assets/mikv

.PHONY: convert-assets