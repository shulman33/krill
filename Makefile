.PHONY: test krilld krilld-linux check-protocol

test:
	go vet ./...
	go test -race ./...

krilld:
	go build -o bin/krilld ./cmd/krilld

# The deploy target: static linux/amd64 binary for the KVM dev box.
krilld-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/krilld-linux-amd64 ./cmd/krilld

check-protocol:
	ci/check-protocol.sh
