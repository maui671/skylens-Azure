# Skylens Makefile
# Build, test, and manage the UAV airspace monitor

BINARY_NAME=skylens-node
SIM_NAME=flight-sim
INTEL_NAME=intel-updater
BUILD_DIR=bin
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME=$(shell date -u '+%Y-%m-%d_%H:%M:%S')
LDFLAGS=-ldflags "-X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME)"

.PHONY: all build build-sim build-intel build-all clean run test fmt vet proto intel-update intel-dry-run

all: build

# --- Build ---

build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/skylens-node
	@echo "Built: $(BUILD_DIR)/$(BINARY_NAME)"

build-sim:
	@echo "Building $(SIM_NAME)..."
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(SIM_NAME) ./cmd/flight-sim
	@echo "Built: $(BUILD_DIR)/$(SIM_NAME)"

build-intel:
	@echo "Building $(INTEL_NAME)..."
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(INTEL_NAME) ./cmd/intel-updater
	@echo "Built: $(BUILD_DIR)/$(INTEL_NAME)"

build-all: build build-sim build-intel

build-release:
	@echo "Building release $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 go build $(LDFLAGS) -trimpath -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/skylens-node
	@echo "Built: $(BUILD_DIR)/$(BINARY_NAME)"

clean:
	@echo "Cleaning..."
	@rm -rf $(BUILD_DIR)

# --- Run ---

run: build
	./$(BUILD_DIR)/$(BINARY_NAME) -config configs/config.yaml

run-json: build
	./$(BUILD_DIR)/$(BINARY_NAME) -config configs/config.yaml -log-format json

run-debug: build
	./$(BUILD_DIR)/$(BINARY_NAME) -config configs/config.yaml -log-level debug

# --- Test ---

test:
	go test -v ./internal/...

test-coverage:
	go test -coverprofile=coverage.out ./internal/...
	go tool cover -html=coverage.out -o coverage.html

# --- Code Quality ---

fmt:
	go fmt ./...

vet:
	go vet ./...

lint: fmt vet

# --- Protobuf ---

proto:
	protoc --go_out=. --go_opt=paths=source_relative proto/skylens.proto
	@echo "Regenerated proto/skylens.pb.go"

# --- Intel Update ---

intel-update: build-intel
	./$(BUILD_DIR)/$(INTEL_NAME) -verbose

intel-dry-run: build-intel
	./$(BUILD_DIR)/$(INTEL_NAME) -dry-run -verbose

# --- Service Management ---

install-service: build
	@echo "Installing systemd service..."
	sudo cp $(BUILD_DIR)/$(BINARY_NAME) /usr/local/bin/
	sudo cp skylens-node.service /etc/systemd/system/
	sudo systemctl daemon-reload
	sudo systemctl enable skylens-node
	@echo "Service installed. Start with: sudo systemctl start skylens-node"

uninstall-service:
	@echo "Uninstalling systemd service..."
	sudo systemctl stop skylens-node || true
	sudo systemctl disable skylens-node || true
	sudo rm -f /etc/systemd/system/skylens-node.service
	sudo rm -f /usr/local/bin/$(BINARY_NAME)
	sudo systemctl daemon-reload
	@echo "Service uninstalled."

start:
	sudo systemctl start skylens-node

stop:
	sudo systemctl stop skylens-node

restart:
	sudo systemctl restart skylens-node

status:
	sudo systemctl status skylens-node

logs:
	sudo journalctl -u skylens-node -f

logs-json:
	sudo journalctl -u skylens-node -o json -f

# --- Help ---

help:
	@echo "Skylens Makefile"
	@echo ""
	@echo "Build:"
	@echo "  build          - Build skylens-node"
	@echo "  build-sim      - Build flight simulator"
	@echo "  build-intel    - Build intel updater"
	@echo "  build-all      - Build everything"
	@echo "  build-release  - Build optimized release binary"
	@echo "  clean          - Remove build artifacts"
	@echo ""
	@echo "Run:"
	@echo "  run            - Build and run with text logging"
	@echo "  run-json       - Build and run with JSON logging"
	@echo "  run-debug      - Build and run with debug logging"
	@echo ""
	@echo "Test:"
	@echo "  test           - Run tests"
	@echo "  test-coverage  - Run tests with coverage report"
	@echo ""
	@echo "Code Quality:"
	@echo "  fmt            - Format code"
	@echo "  vet            - Run go vet"
	@echo "  lint           - Run fmt and vet"
	@echo "  proto          - Regenerate protobuf Go code"
	@echo ""
	@echo "Intel:"
	@echo "  intel-update   - Fetch IEEE OUIs and update drone_models.json"
	@echo "  intel-dry-run  - Show what would change without writing"
	@echo ""
	@echo "Service:"
	@echo "  install-service   - Install systemd service"
	@echo "  uninstall-service - Remove systemd service"
	@echo "  start/stop/restart/status/logs"
