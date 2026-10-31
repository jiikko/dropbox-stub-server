.PHONY: test lint build run

test:
	go test -count=1 -race ./...

lint:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...

build:
	go build -o dropbox-stub-server .

# make run ROOT=~/Movies/stub-root [ADDR=127.0.0.1:8765]
run:
	go run . -root "$(ROOT)" -addr "$(or $(ADDR),127.0.0.1:8765)"
