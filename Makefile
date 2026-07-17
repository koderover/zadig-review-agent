APP := zadig-review-agent
CMD := ./cmd/zadig-review-agent

.PHONY: all fmt test build clean

all: fmt test build

fmt:
	gofmt -w cmd internal

test:
	go test ./...

build:
	go build -o bin/$(APP) $(CMD)

clean:
	rm -rf bin
