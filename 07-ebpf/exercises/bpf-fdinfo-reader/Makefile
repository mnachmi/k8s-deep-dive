BINARY := bpf-fdinfo-reader

.PHONY: all build run clean

all: build

build:
	go build -o $(BINARY) .

run:
	go run . --pid $$PPID

clean:
	rm -f $(BINARY)
