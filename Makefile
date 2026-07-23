.PHONY: test krilld krilld-linux krill krill-linux mcp check-protocol

test:
	go vet ./...
	go test -race ./...

krilld:
	go build -o bin/krilld ./cmd/krilld

krill:
	go build -o bin/krill ./cmd/krill

# The deploy targets: static linux/amd64 binaries for the KVM dev box.
krilld-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/krilld-linux-amd64 ./cmd/krilld

krill-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/krill-linux-amd64 ./cmd/krill

mcp:
	cd mcp && npm install && npm run build

check-protocol:
	ci/check-protocol.sh
