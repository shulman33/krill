.PHONY: test krilld krilld-linux krill krill-linux doorman doorman-linux fencetool-linux mcp check-protocol

test:
	go vet ./...
	go test -race ./...

krilld:
	go build -o bin/krilld ./cmd/krilld

krill:
	go build -o bin/krill ./cmd/krill

# The M4 front door. Runs unprivileged and separate from krilld on purpose.
doorman:
	go build -o bin/krill-doorman ./cmd/krill-doorman

# The deploy targets: static linux/amd64 binaries for the KVM dev box.
krilld-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/krilld-linux-amd64 ./cmd/krilld

krill-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/krill-linux-amd64 ./cmd/krill

doorman-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/krill-doorman-linux-amd64 ./cmd/krill-doorman

# The C2 gate's stale-epoch prober (m3-gates).
fencetool-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/fencetool-linux-amd64 ./m3-gates/fencetool

mcp:
	cd mcp && npm install && npm run build

check-protocol:
	ci/check-protocol.sh
