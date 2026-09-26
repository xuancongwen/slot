BINARY := slot

.PHONY: build run test vet fmt fmt-check bench check up clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o $(BINARY) .

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
	go test -run '^$$' -bench . -benchmem

# Everything that must pass before a change is done.
check: fmt-check vet test

up:
	docker compose up -d --build

clean:
	rm -f $(BINARY)
