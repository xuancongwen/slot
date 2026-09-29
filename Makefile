BINARY := bin/slot

.PHONY: build run test vet fmt fmt-check bench check up clean disposable-domains

build:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o $(BINARY) ./cmd/slot

run: build
	./$(BINARY)

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

bench:
	go test -run '^$$' -bench . -benchmem ./...

# Everything that must pass before a change is done.
check: fmt-check vet test

up:
	docker compose up -d --build

clean:
	rm -rf bin

# Refresh the embedded list of throwaway inbox domains that cannot book.
disposable-domains:
	curl -fsSL https://raw.githubusercontent.com/disposable-email-domains/disposable-email-domains/main/disposable_email_blocklist.conf -o internal/slot/disposable_domains.txt
