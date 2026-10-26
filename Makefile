# This Makefile is meant to be used by people that do not usually work
# with Go source code. If you know what GOPATH is then you probably
# don't need to bother with make.

.PHONY: geth geth-linux evm faucet all test truffle-test lint fmt clean devtools help
.PHONY: docker

GOBIN = ./build/bin
GO ?= latest
GORUN = go run
GIT_COMMIT=$(shell git rev-parse HEAD)
GIT_COMMIT_DATE=$(shell git log -n1 --pretty='format:%cd' --date=format:'%Y%m%d')

# Cross build settings for the linux target.
#
# A linux binary cannot be produced with CGO_ENABLED=0: cmd/geth pulls in
# prysm's crypto/bls, which imports both blst and herumi unconditionally
# (prysm v5 crypto/bls/bls.go:8-10), and both are cgo wrappers around C
# libraries. So the cross build needs a real linux C toolchain, and on macOS the
# practical way to get one is a container.
#
# Two deliberate choices here, both forced by Docker Desktop on macOS:
#
#   - The image is the Debian based golang image, not the alpine one used by
#     ./Dockerfile. Debian ships gcc and make already, so no apk/apt step is
#     needed at all. That matters on hosts where the container's DNS answers
#     with AAAA records but there is no IPv6 route: apk's HTTP client does not
#     fall back to IPv4 and hangs forever.
#
#   - The module and build caches live in docker volumes, not in host
#     directories. Binding a host Go module cache into a container costs
#     minutes on virtiofs (it is a few hundred thousand small files) while a
#     volume starts in under a second. The first build downloads dependencies
#     through LINUX_GOPROXY; later builds reuse the volume and are incremental.
LINUX_PLATFORM ?= linux/amd64
# Override when Docker Hub is unreachable, e.g.
#   LINUX_GOLANG_IMAGE=docker.m.daocloud.io/library/golang:1.26
LINUX_GOLANG_IMAGE ?= golang:1.26
LINUX_GOPROXY ?= https://goproxy.cn,direct
LINUX_CACHE_PREFIX ?= bsc-linux
LINUX_GOMOD_VOLUME = $(LINUX_CACHE_PREFIX)-gomod
LINUX_GOCACHE_VOLUME = $(LINUX_CACHE_PREFIX)-gocache
LINUX_BINARY = $(GOBIN)/geth-$(subst /,-,$(LINUX_PLATFORM))
# Scratch dir bind mounted over the build output directory. build/ci.go pins
# GOBIN to <repo>/build/bin (build/ci.go:173), so without this the container
# would overwrite the host's native binaries with linux ones.
LINUX_STAGE = build/.linux-stage

#? geth: Build geth.
geth:
	$(GORUN) build/ci.go install ./cmd/geth
	@echo "Done building."
	@echo "Run \"$(GOBIN)/geth\" to launch geth."

#? geth-linux: Build geth for linux through docker (macOS needs this, see LINUX_PLATFORM).
geth-linux:
	@mkdir -p $(GOBIN) $(LINUX_STAGE)
	@docker volume create $(LINUX_GOMOD_VOLUME) >/dev/null
	@docker volume create $(LINUX_GOCACHE_VOLUME) >/dev/null
	docker run --rm --platform $(LINUX_PLATFORM) \
		-v "$(CURDIR)":/go-ethereum \
		-v "$(CURDIR)/$(LINUX_STAGE)":/go-ethereum/build/bin \
		-v $(LINUX_GOMOD_VOLUME):/go/pkg/mod \
		-v $(LINUX_GOCACHE_VOLUME):/root/.cache/go-build \
		-w /go-ethereum \
		-e GOPROXY=$(LINUX_GOPROXY) \
		-e CGO_CFLAGS="-O -D__BLST_PORTABLE__" \
		-e CGO_CFLAGS_ALLOW="-O -D__BLST_PORTABLE__" \
		$(LINUX_GOLANG_IMAGE) \
		go run build/ci.go install -static ./cmd/geth
	@mv -f $(LINUX_STAGE)/geth $(LINUX_BINARY)
	@rm -rf $(LINUX_STAGE)
	@chmod +x $(LINUX_BINARY)
	@echo "Done building."
	@echo "Copy \"$(LINUX_BINARY)\" to a $(LINUX_PLATFORM) host and run it there."

#? faucet: Build faucet
faucet:
	$(GORUN) build/ci.go install ./cmd/faucet
	@echo "Done building faucet"

#? evm: Build evm.
evm:
	$(GORUN) build/ci.go install ./cmd/evm
	@echo "Done building."
	@echo "Run \"$(GOBIN)/evm\" to launch evm."

#? all: Build all packages and executables.
all:
	$(GORUN) build/ci.go install

#? test: Run the tests.
test: all
	$(GORUN) build/ci.go test -timeout 1h

#? truffle-test: Run the integration test.
truffle-test:
	rm -rf ./tests/truffle/storage/bsc-validator1
	rm -rf ./tests/truffle/storage/bsc-rpc
	docker build . -f ./docker/Dockerfile --target bsc -t bsc
	docker build . -f ./docker/Dockerfile --target bsc-genesis -t bsc-genesis
	docker build . -f ./docker/Dockerfile.truffle -t truffle-test
	docker compose -f ./tests/truffle/docker-compose.yml up genesis
	docker compose -f ./tests/truffle/docker-compose.yml up -d bsc-rpc bsc-validator1
	sleep 60
	docker compose -f ./tests/truffle/docker-compose.yml up --exit-code-from truffle-test truffle-test
	docker compose -f ./tests/truffle/docker-compose.yml down

#? lint: Run certain pre-selected linters.
lint: ## Run linters.
	$(GORUN) build/ci.go lint

#? fmt: Ensure consistent code formatting.
fmt:
	gofmt -s -w $(shell find . -name "*.go")

#? clean: Clean go cache, built executables, and the auto generated folder.
clean:
	go clean -cache
	rm -fr build/_workspace/pkg/ $(GOBIN)/*

# The devtools target installs tools required for 'go generate'.
# You need to put $GOBIN (or $GOPATH/bin) in your PATH to use 'go generate'.

#? devtools: Install recommended developer tools.
devtools:
	env GOBIN= go install golang.org/x/tools/cmd/stringer@latest
	env GOBIN= go install github.com/fjl/gencodec@latest
	env GOBIN= go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	env GOBIN= go install ./cmd/abigen
	@type "solc" 2> /dev/null || echo 'Please install solc'
	@type "protoc" 2> /dev/null || echo 'Please install protoc'

#? help: Build docker image
docker:
	docker build --pull -t bnb-chain/bsc:latest -f Dockerfile .

#? help: Get more info on make commands.
help: Makefile
	@echo ''
	@echo 'Usage:'
	@echo '  make [target]'
	@echo ''
	@echo 'Targets:'
	@sed -n 's/^#?//p' $< | column -t -s ':' |  sort | sed -e 's/^/ /'
