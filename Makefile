BINARY := smtp-store
CMD := ./cmd/smtp-store
DIST := dist
BIN := bin
TARGETS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64

.PHONY: build test clean clean-dist cross-build

build:
	@mkdir -p $(BIN)
	go build -trimpath -o $(BIN)/$(BINARY) $(CMD)

test:
	go test ./...

clean:
	rm -rf $(BIN)

clean-dist:
	rm -rf $(DIST)

cross-build: clean-dist
	@mkdir -p $(DIST)
	@for target in $(TARGETS); do \
		os=$${target%/*}; \
		arch=$${target#*/}; \
		outdir="$(DIST)/$(BINARY)_$${os}_$${arch}"; \
		mkdir -p "$${outdir}"; \
		echo "Building $${os}/$${arch}"; \
		GOOS=$${os} GOARCH=$${arch} CGO_ENABLED=0 go build -trimpath -o "$${outdir}/$(BINARY)" $(CMD); \
		cp config.example.yaml "$${outdir}/config.yaml"; \
		tar -C "$${outdir}" -czf "$(DIST)/$(BINARY)_$${os}_$${arch}.tar.gz" $(BINARY) config.yaml; \
	done
