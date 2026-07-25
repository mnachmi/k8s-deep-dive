.PHONY: build run clean

build:
	go build -o bin/kube-inspect ./cmd/kube-inspect/

run: build
	./bin/kube-inspect --help

clean:
	rm -rf bin/
