# Package related
PACKAGE := sriovnet
BIN_DIR := $(CURDIR)/bin
GOFILES := $(shell find . -name "*.go" | grep -vE "(\/vendor\/)|(_test.go)")
PKGS := $(or $(PKG),$(shell go list ./... | grep -v "^$(PACKAGE)/vendor/"))
TESTPKGS := $(shell go list -f '{{ if or .TestGoFiles .XTestGoFiles }}{{ .ImportPath }}{{ end }}' $(PKGS))

# Go tools
GOLANGCI_LINT := $(BIN_DIR)/golangci-lint
GCOV2LCOV := $(BIN_DIR)/gcov2lcov
# golangci-lint version should be updated periodically
# we keep it fixed to avoid it from unexpectedly failing on the project
# in case of a version bump
GOLANGCI_LINT_VER := v1.49.0

Q = $(if $(filter 1,$V),,@)

.PHONY: all
all: lint test build

$(BIN_DIR):
	@mkdir -p $@

build: $(GOFILES) ;@ ## build sriovnet
	@CGO_ENABLED=0 go build -v

# Tests

.PHONY: lint
lint: | $(GOLANGCI_LINT) ; $(info  running golangci-lint...) @ ## Run lint tests
		$Q $(GOLANGCI_LINT) run

.PHONY: test tests
test: ; $(info  running unit tests...) ## Run unit tests
	$Q go test ./...

tests: test lint ; ## Run all tests

COVERAGE_MODE = count
.PHONY: test-coverage test-coverage-tools
test-coverage-tools: $(GCOV2LCOV)
test-coverage: | test-coverage-tools; $(info  running coverage tests...) @ ## Run coverage tests
	$Q go test -covermode=$(COVERAGE_MODE) -coverprofile=sriovnet.cover ./...
	$Q $(GCOV2LCOV) -infile sriovnet.cover -outfile sriovnet.info

# Tools
$(GOLANGCI_LINT): | $(BIN_DIR) ; $(info  building golangci-lint...)
	$Q GOBIN=$(BIN_DIR) go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VER)

$(GCOV2LCOV):  | $(BIN_DIR) ; $(info  building gocov2lcov...)
	$Q GOBIN=$(BIN_DIR) go install github.com/jandelgado/gcov2lcov@v1.0.5

# Misc
.PHONY: clean
clean: ; $(info  Cleaning...) @ ## Cleanup everything
	@rm -rf  $(BIN_DIR)
	@rm sriovnet.cover
	@rm sriovnet.info

.PHONY: help
help: ; @ ## Show this message
	@grep -E '^[ a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
