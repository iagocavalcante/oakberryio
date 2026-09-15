# BOX is the ssh target for the oak host, e.g. `make install BOX=oak@10.0.0.5`.
BOX ?=

# VERSION is embedded in the oak CLI via -X main.version (see cmd/oak/main.go).
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build install test usb clean

build:
	mkdir -p bin
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/oakd ./cmd/oakd
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/oak-init ./cmd/oak-init
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(VERSION)" -o bin/oak ./cmd/oak
	go build -ldflags="-X main.version=$(VERSION)" -o bin/oak-darwin ./cmd/oak

install: build
	@test -n "$(BOX)" || { echo "usage: make install BOX=user@host" >&2; exit 1; }
	ssh "$(BOX)" 'mkdir -p /tmp/oak-deploy'
	scp bin/oakd bin/oak-init scripts/backup.sh "$(BOX):/tmp/"
	scp deploy/* "$(BOX):/tmp/oak-deploy/"
	ssh "$(BOX)" '\
		sudo install -m755 /tmp/oakd /usr/local/bin/oakd && \
		sudo install -m755 /tmp/oak-init /usr/local/bin/oak-init && \
		sudo install -m755 /tmp/backup.sh /usr/local/bin/oak-backup.sh && \
		sudo install -m644 /tmp/oak-deploy/*.service /tmp/oak-deploy/*.timer /etc/systemd/system/ && \
		sudo systemctl daemon-reload && \
		sudo systemctl restart oakd && \
		sudo systemctl enable --now oak-backup.timer && \
		sudo systemctl enable --now oak-docker-forward.service'

test:
	go test ./...

usb:
	./scripts/make-usb.sh

clean:
	rm -rf bin
