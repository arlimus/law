BIN := law

.PHONY: build install clean run

build:
	go build -o $(BIN) .

install:
	go install .

run: build
	./$(BIN)

clean:
	rm -f $(BIN)
