BIN := law

.PHONY: build install clean run watch

build:
	go build -o $(BIN) .

install:
	go install .

run: build
	./$(BIN)

watch:
	@command -v entr >/dev/null || { echo "entr not found — install it (e.g. pacman -S entr / brew install entr)"; exit 1; }
	@find . -type f \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) | entr -c $(MAKE) install

clean:
	rm -f $(BIN)
