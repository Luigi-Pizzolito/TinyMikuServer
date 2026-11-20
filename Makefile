# Makefile for TinyMikuServer and mikv-converter

# Directories
BIN_DIR := ./bin
MIKV_CONVERTER_DIR := ./pkg/mikvformat/cmd/mikv-converter
HOLOCROPPER_DIR := ./pkg/holocropper/cmd/holocropper

# Go commands
GO := go
GO_BUILD := $(GO) build

# Targets
all: build-server build-mikv-converter build-holocropper

build-server:
	@echo "Building TinyMikuServer..."
	$(GO_BUILD) -o $(BIN_DIR)/TinyMikuServer .

build-mikv-converter:
	@echo "Building mikv-converter..."
	cd $(MIKV_CONVERTER_DIR) && $(GO_BUILD) -o ../../../../$(BIN_DIR)/mikv-converter

build-holocropper:
	@echo "Building holocropper..."
	cd $(HOLOCROPPER_DIR) && $(GO_BUILD) -o ../../../../$(BIN_DIR)/holocropper

# Mirrors .github/workflows/ci.yml: build+vet for the server and mikv modules,
# the full -race suite for holocropper (lavfi fixtures + golden CSV only, no
# corpus videos needed).
test: test-mikv test-holocropper

test-mikv:
	@echo "Building and vetting server + mikv packages..."
	$(GO_BUILD) ./... && $(GO) vet ./...
	cd pkg/mikvformat && $(GO_BUILD) ./... && $(GO) vet ./...
	cd pkg/mikvserver && $(GO_BUILD) ./... && $(GO) vet ./...

test-holocropper:
	@echo "Testing holocropper (-race, corpus-free)..."
	cd pkg/holocropper && $(GO) test -count=1 -race -timeout 600s ./...

clean:
	@echo "Cleaning up binaries..."
	find $(BIN_DIR) -type f ! -name '.gitkeep' -delete

.PHONY: all build-server build-mikv-converter build-holocropper test test-mikv test-holocropper clean

convert-assets:
	@echo "Converting assets..."
	@for file in ./assets/mp4/*; do \
		basefile=$$(basename "$$file" .mp4); \
		output_file=./assets/mikv/$$basefile.mikv; \
		output_dir=./assets/mikv; \
		if [ ! -f "$$output_file" ]; then \
			echo "Converting $$file to $$output_file..."; \
			./bin/mikv-converter -i "$$file" -o "$$output_dir"; \
		fi; \
	done
	@echo "All assets converted."

convert-full:
	@echo "Converting full videos..."
	@for file in ./assets/mp4-full/*; do \
		basefile=$$(basename "$$file" .mp4); \
		output_file=./assets/mp4/$$basefile.mp4; \
		output_dir=./assets/mp4; \
		if [ ! -f "$$output_file" ]; then \
			echo "Converting $$file to $$output_file..."; \
			./bin/holocropper -i "$$file" -o "$$output_dir"; \
		fi; \
		done
	@echo "All full videos converted."

.PHONY: convert-assets convert-full

full-convert-assets: convert-full convert-assets

