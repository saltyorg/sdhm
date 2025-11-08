# SDHM - Salty Docker Hosts Manager

A daemon that automatically updates `/etc/hosts` with Docker container hostnames from specified Docker networks.

## Features

- **Automatic Host Management**: Monitors Docker network events (connect/disconnect) and updates `/etc/hosts` in real-time
- **Multi-Network Support**: Monitor multiple Docker networks simultaneously
- **Debounced Event Handling**: Prevents excessive updates during container churn
- **Periodic Validation**: Ensures `/etc/hosts` stays in sync with Docker networks
- **Health Check Endpoint**: Built-in HTTP health check for monitoring and service management
- **Automatic Recovery**: Recovers from corrupted hosts files using automatic backups
- **Configurable Section Management**: Manages a clearly marked section in `/etc/hosts` while preserving other entries

## Requirements

- Linux system with Docker installed
- Root access (to modify `/etc/hosts`)
- Go 1.25.3+ (for building from source)

## Installation

### Option 1: Using Make (Recommended)

```bash
# Clone the repository
git clone https://github.com/saltyorg/sdhm.git
cd sdhm

# Build the binary
make build

# Install to /usr/local/bin (requires root)
sudo make install
```

### Option 2: Download Pre-built Binary

Download the latest release from the [releases page](https://github.com/saltyorg/sdhm/releases), then:

```bash
# Download and install
curl -s https://api.github.com/repos/saltyorg/sdhm/releases/latest | jq -r '.assets[] | select(.name == "sdhm_linux_amd64") | .browser_download_url' | xargs sudo curl -Lo /usr/local/bin/sdhm && sudo chmod +x /usr/local/bin/sdhm
```

## Usage

### Basic Usage

Run with default settings (monitors the `saltbox` network):

```bash
sudo sdhm
```

### Common Use Cases

Monitor a specific Docker network:
```bash
sudo sdhm --networks mynetwork
```

Monitor multiple Docker networks:
```bash
sudo sdhm --networks "bridge,mynetwork,webproxy"
```

Run with custom validation interval:
```bash
sudo sdhm --interval 10m
```

Enable health check on all interfaces (useful for Docker health checks):
```bash
sudo sdhm --health-addr 0.0.0.0 --health-port 8080
```

Testing with a custom hosts file (useful for development):
```bash
sdhm --hosts-file /tmp/test-hosts --networks bridge
```

### Configuration Options

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--networks` | `-n` | `saltbox` | Comma-separated list of Docker networks to monitor |
| `--interval` | `-i` | `5m` | Periodic validation interval (e.g., 30s, 5m, 1h, 1d) |
| `--health-port` | `-p` | `8080` | Health check HTTP server port |
| `--health-addr` | | `127.0.0.1` | IP address to bind health check server |
| `--hosts-file` | | `/etc/hosts` | Path to hosts file (useful for testing) |
| `--backup-file` | | `/etc/hosts.backup` | Path to backup file |
| `--section-name` | | `DOCKER CONTAINERS` | Name for managed section in hosts file |
| `--debounce-delay` | | `1s` | Debounce delay for event handling |
| `--debounce-max-delay` | | `5s` | Maximum debounce delay |

### Time Duration Formats

Duration values support the following units:
- `s` - seconds (e.g., `30s`)
- `m` - minutes (e.g., `5m`)
- `h` - hours (e.g., `1h`)
- `d` - days (e.g., `1d`)

### Health Check Endpoint

SDHM provides a health check HTTP endpoint for monitoring:

```bash
# Check health status
curl http://127.0.0.1:8080/health
```

Response:
```json
{
  "status": "ok",
  "uptime": "1h23m45s"
}
```

## Running as a System Service

### systemd Service

Create `/etc/systemd/system/sdhm.service`:

```ini
[Unit]
Description=Salty Docker Hosts Manager
After=docker.service
Requires=docker.service

[Service]
Type=simple
ExecStart=/usr/local/bin/sdhm --networks saltbox --interval 5m
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

Enable and start the service:

```bash
sudo systemctl daemon-reload
sudo systemctl enable sdhm
sudo systemctl start sdhm
```

Check service status:

```bash
sudo systemctl status sdhm
sudo journalctl -u sdhm -f
```


## How It Works

1. **Monitoring**: SDHM connects to the Docker daemon and listens for network events
2. **Event Handling**: When containers connect/disconnect from monitored networks, events are debounced to prevent excessive updates
3. **Hosts File Update**: Container hostnames and IPs are added to a managed section in `/etc/hosts`:
   ```
   # BEGIN DOCKER CONTAINERS
   172.18.0.2  mycontainer
   172.18.0.3  webserver
   # END DOCKER CONTAINERS
   ```
4. **Periodic Validation**: Every interval, SDHM validates that `/etc/hosts` matches the current Docker network state
5. **Backup & Recovery**: Before each update, `/etc/hosts` is backed up; if corruption is detected, it's automatically restored

## Development

### Building

```bash
make build
```

### Testing

```bash
# Run all tests
make test

# Run tests with coverage
make test-coverage

# Run quick tests only
make test-short
```

### Code Quality

```bash
# Format code
make fmt

# Run go vet
make vet

# Update dependencies
make update

# Modernize code (format, vet, update, apply latest Go patterns)
make modernize
```

### Available Make Targets

Run `make help` to see all available targets:

```
 all              All targets (test + build)
 build            Build the sdhm binary
 clean            Clean build artifacts and test files
 test             Run all tests (doesn't touch production /etc/hosts)
 test-short       Run short tests
 test-coverage    Run tests with coverage report
 deps             Download dependencies
 update           Update dependencies to latest versions
 tidy             Tidy go.mod
 modernize        Modernize the project (format, vet, update, tidy)
 fmt              Format code
 vet              Run go vet
 lint             Run golangci-lint
 run              Run the application with example interval
 install          Install the binary to /usr/local/bin
 uninstall        Remove the binary from /usr/local/bin
 help             Show this help message
```

## Troubleshooting

### Permission Denied

SDHM requires root access to modify `/etc/hosts`:

```bash
# Run with sudo
sudo sdhm
```

### Container Hostnames Not Updating

1. Verify the correct network is being monitored:
   ```bash
   docker network ls
   sudo sdhm --networks your-network-name
   ```

2. Check Docker events are being received:
   ```bash
   docker events --filter type=network
   ```

3. Verify containers are connected to the monitored network:
   ```bash
   docker network inspect your-network-name
   ```

### Health Check Fails

If accessing the health check from another container:

```bash
# Allow binding to all interfaces
sudo sdhm --health-addr 0.0.0.0
```

## Contributing

Contributions are welcome! Please:

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Run tests: `make test`
5. Submit a pull request

## License

This project is licensed under the GNU General Public License v3.0 - see the [LICENSE](LICENSE) file for details.

## Support

- **Issues**: [GitHub Issues](https://github.com/saltyorg/sdhm/issues)

## Acknowledgments

Built for the [Saltbox](https://docs.saltbox.dev) project.
