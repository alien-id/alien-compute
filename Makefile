# Alien Fleet — Phase-0 testnet reference implementation
# See docs/BUILD.md for prerequisites and details.

LLAMA_DIR   := third_party/llama.cpp
CUDA_ARCH   ?= 89            # RTX 4090 (Ada); H100=90, RTX 30xx=86
HOST_CC     ?= gcc-14        # CUDA-compatible host compiler
MODELS_DIR  := models

.PHONY: all build dist test vet llama models testnet demo report clean

all: build

## Go binaries -----------------------------------------------------------

build:
	go build -o bin/fleetd ./cmd/fleetd
	go build -o bin/fleet-proxy ./cmd/fleet-proxy

dist:
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -trimpath -o bin/dist/fleetd-linux-amd64       ./cmd/fleetd
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -trimpath -o bin/dist/fleet-proxy-linux-amd64  ./cmd/fleet-proxy
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o bin/dist/fleet-proxy-darwin-arm64 ./cmd/fleet-proxy
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o bin/dist/fleet-proxy-windows-amd64.exe ./cmd/fleet-proxy

test:
	go test ./internal/...

vet:
	go vet ./...
	gofmt -l ./cmd ./internal

## Inference backend (llama.cpp, CUDA) ------------------------------------

llama:
	@test -d $(LLAMA_DIR) || git clone --depth 1 https://github.com/ggml-org/llama.cpp.git $(LLAMA_DIR)
	cmake -S $(LLAMA_DIR) -B $(LLAMA_DIR)/build \
	  -DGGML_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES=$(CUDA_ARCH) \
	  -DCMAKE_C_COMPILER=$(HOST_CC) -DCMAKE_CXX_COMPILER=$(subst gcc,g++,$(HOST_CC)) \
	  -DCMAKE_CUDA_HOST_COMPILER=$(subst gcc,g++,$(HOST_CC)) \
	  -DLLAMA_CURL=OFF -DCMAKE_BUILD_TYPE=Release
	cmake --build $(LLAMA_DIR)/build --target llama-server -j $$(nproc)

## Model bundles (Gemma 4, Q4_K_M) ----------------------------------------

models:
	mkdir -p $(MODELS_DIR)
	curl -fSL --retry 3 -C - -o $(MODELS_DIR)/gemma-4-12B-it-Q4_K_M.gguf \
	  https://huggingface.co/ggml-org/gemma-4-12B-it-GGUF/resolve/main/gemma-4-12B-it-Q4_K_M.gguf
	curl -fSL --retry 3 -C - -o $(MODELS_DIR)/gemma-4-26B-A4B-it-Q4_K_M.gguf \
	  https://huggingface.co/ggml-org/gemma-4-26B-A4B-it-GGUF/resolve/main/gemma-4-26B-A4B-it-Q4_K_M.gguf
	curl -fSL --retry 3 -C - -o $(MODELS_DIR)/gemma-4-E4B-it-Q4_K_M.gguf \
	  https://huggingface.co/unsloth/gemma-4-E4B-it-GGUF/resolve/main/gemma-4-E4B-it-Q4_K_M.gguf

## Local testnet ----------------------------------------------------------

testnet: build
	FRESH=$${FRESH:-0} ./demo/testnet.sh

demo:
	./demo/demo.sh

report:
	./demo/report.sh

clean:
	rm -rf bin
