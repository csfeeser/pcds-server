BINARY = pcds-server

build:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o $(BINARY) main.go

run:
	go run main.go

clean:
	rm -f $(BINARY)

.PHONY: build run clean
