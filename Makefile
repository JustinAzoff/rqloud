CGO_ENABLED := 1
CC := clang
export CGO_ENABLED CC

.PHONY: build vet tidy test-nixos clean

build: rqloud counter todo

rqloud: $(wildcard *.go cmd/rqloud/*.go)
	go build -o $@ ./cmd/rqloud/

counter: $(wildcard *.go examples/counter/*.go)
	go build -o $@ ./examples/counter/

todo: $(wildcard *.go examples/todo/*.go)
	go build -o $@ ./examples/todo/

vet:
	go vet ./...

tidy:
	go mod tidy

test-nixos: counter
	nix build path:..#checks.x86_64-linux.integration -L

clean:
	rm -f rqloud todo counter
