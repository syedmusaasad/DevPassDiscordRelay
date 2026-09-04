# Toolchain Installation Report

## Date
September 4, 2026

## Installed Components

### Go
- **Version**: go1.27.1 linux/amd64
- **Installation Path**: /usr/local/go
- **Download URL**: https://go.dev/dl/go1.27.1.linux-amd64.tar.gz
- **SHA256 Checksum**: 63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445 (verified)
- **Installation Command**: 
  ```bash
  cd /tmp && curl -sL https://go.dev/dl/go1.27.1.linux-amd64.tar.gz -o go1.27.1.linux-amd64.tar.gz
  tar -C /usr/local -xzf go1.27.1.linux-amd64.tar.gz
  ```
- **Verification**: `/usr/local/go/bin/go version` returns `go version go1.27.1 linux/amd64`

### Make
- **Version**: GNU Make 4.4.1
- **Installation Command**: 
  ```bash
  apt-get install -y make
  ```
- **Verification**: `make --version` returns GNU Make 4.4.1

### Docker
- **Version**: Docker version 29.1.3, build 29.1.3-0ubuntu4.1
- **Installation Command**: 
  ```bash
  apt-get install -y docker.io
  systemctl enable --now docker
  ```
- **Verification**: `docker info` executes successfully with exit code 0

### Environment Configuration
- **File**: /etc/profile.d/golang.sh
- **Contents**: `export PATH=$PATH:/usr/local/go/bin`
- **Purpose**: Ensures Go binary is in the PATH for all shell sessions

## Installation Status
All components installed successfully. The toolchain is ready for building static Go binaries and Docker integration as specified.

## Notes
- Go was installed from official upstream (go.dev) with SHA256 verification
- CGO_ENABLED=0 compatible for static binary builds with modernc.org/sqlite driver
- Docker daemon is running and ready for use
