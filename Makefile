PREFIX ?= $(HOME)/.local/bin
BINARY = tmux-ai-status

.PHONY: build install uninstall test clean

build:
	go build -o $(BINARY) .

install: build
	mkdir -p $(PREFIX)
	cp $(BINARY) $(PREFIX)/$(BINARY)-bin

uninstall:
	rm -f $(PREFIX)/$(BINARY)

test:
	go test -v ./...

bench:
	go test -bench=. -benchmem ./...

clean:
	rm -f $(BINARY)
