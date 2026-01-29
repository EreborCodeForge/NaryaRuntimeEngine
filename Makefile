# Narya Runtime Engine - Makefile
# Compilação para múltiplas plataformas

BINARY_NAME=narya
VERSION=1.0.0
BUILD_DIR=../../build
GO_FILES=$(wildcard *.go)

# Flags de build
LDFLAGS=-ldflags "-s -w -X main.version=$(VERSION)"

.PHONY: all build clean deps test linux darwin windows install help

# Build padrão para a plataforma atual
all: build

# Baixa dependências
deps:
	@echo "Baixando dependências..."
	go mod download
	go mod tidy

# Build para a plataforma atual
build: deps
	@echo "Compilando $(BINARY_NAME) para $(shell go env GOOS)/$(shell go env GOARCH)..."
	go build $(LDFLAGS) -o $(BINARY_NAME)$(shell go env GOEXE) .
	@echo "Build concluído: $(BINARY_NAME)$(shell go env GOEXE)"

# Build para Linux (amd64)
linux: deps
	@echo "Compilando para Linux amd64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 .
	@echo "Build concluído: $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64"

# Build para Linux ARM64
linux-arm64: deps
	@echo "Compilando para Linux arm64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 .
	@echo "Build concluído: $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64"

# Build para macOS (amd64)
darwin: deps
	@echo "Compilando para macOS amd64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 .
	@echo "Build concluído: $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64"

# Build para macOS ARM64 (Apple Silicon)
darwin-arm64: deps
	@echo "Compilando para macOS arm64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 .
	@echo "Build concluído: $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64"

# Build para Windows
windows: deps
	@echo "Compilando para Windows amd64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe .
	@echo "Build concluído: $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe"

# Build para todas as plataformas
release: linux linux-arm64 darwin darwin-arm64 windows
	@echo ""
	@echo "Builds de release concluídos:"
	@ls -la $(BUILD_DIR)/

# Testes
test:
	@echo "Executando testes..."
	go test -v ./...

# Testes com coverage
test-coverage:
	@echo "Executando testes com coverage..."
	go test -v -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Relatório de coverage gerado: coverage.html"

# Benchmark
bench:
	@echo "Executando benchmarks..."
	go test -bench=. -benchmem ./...

# Lint (requer golangci-lint)
lint:
	@echo "Executando linter..."
	golangci-lint run

# Formata código
fmt:
	@echo "Formatando código..."
	go fmt ./...

# Verifica código
vet:
	@echo "Verificando código..."
	go vet ./...

# Instala o binário
install: build
	@echo "Instalando $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	cp $(BINARY_NAME)$(shell go env GOEXE) $(BUILD_DIR)/
	@echo "Instalado em $(BUILD_DIR)/$(BINARY_NAME)$(shell go env GOEXE)"

# Limpa arquivos de build
clean:
	@echo "Limpando arquivos de build..."
	rm -f $(BINARY_NAME) $(BINARY_NAME).exe
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html
	@echo "Limpeza concluída"

# Ajuda
help:
	@echo "Narya Runtime Engine - Comandos disponíveis:"
	@echo ""
	@echo "  make build        - Compila para a plataforma atual"
	@echo "  make linux        - Compila para Linux amd64"
	@echo "  make linux-arm64  - Compila para Linux arm64"
	@echo "  make darwin       - Compila para macOS amd64"
	@echo "  make darwin-arm64 - Compila para macOS arm64 (Apple Silicon)"
	@echo "  make windows      - Compila para Windows amd64"
	@echo "  make release      - Compila para todas as plataformas"
	@echo "  make test         - Executa testes"
	@echo "  make test-coverage- Executa testes com coverage"
	@echo "  make bench        - Executa benchmarks"
	@echo "  make lint         - Executa linter"
	@echo "  make fmt          - Formata código"
	@echo "  make vet          - Verifica código"
	@echo "  make install      - Instala o binário"
	@echo "  make clean        - Limpa arquivos de build"
	@echo "  make deps         - Baixa dependências"
	@echo "  make help         - Exibe esta ajuda"
