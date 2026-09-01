# SDHM - Saltbox Docker Hosts Manager

A daemon that automatically updates `/etc/hosts` with Docker container hostnames from specified Docker networks.

## Features

- **Authoritative Host Management**: Reconciles complete Docker snapshots into `/etc/hosts` after network events and periodic validation
- **Multi-Network Support**: Monitors multiple Docker networks in one atomic update, with collision-safe qualified aliases
- **Debounced Event Handling**: Prevents excessive updates during container churn
- **Periodic Validation**: Ensures `/etc/hosts` stays in sync with Docker networks
- **Current-State Health**: Reports active failures and retains bounded diagnostic history without time-based false recovery
- **Validated Recovery**: Recovers corrupt managed markers only from a valid backup and otherwise fails without overwriting the target
- **Transactional Updates**: Uses adjacent temporary files, durable atomic replacement, readback validation, and bounded rollback
- **Configurable Section Management**: Manages a clearly marked section in `/etc/hosts` while preserving other entries

## Requirements

- Linux system with Docker installed
- Root access (to modify `/etc/hosts`)
- Go 1.27+ (for building from source)

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

Choose which monitored network also receives bare aliases:
```bash
sudo sdhm --networks "saltbox,backend" --default-network backend
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
| `--default-network` | | Automatic | Monitored network that also receives bare host aliases |
| `--interval` | `-i` | `5m` | Periodic validation interval (e.g., 30s, 5m, 1h, 1d) |
| `--health-port` | `-p` | `8080` | Health check HTTP server port |
| `--health-addr` | | `127.0.0.1` | IP address to bind health check server |
| `--hosts-file` | | `/etc/hosts` | Path to hosts file (useful for testing) |
| `--backup-file` | | `/etc/hosts.backup` | Path to backup file |
| `--section-name` | | `DOCKER CONTAINERS` | Name for managed section in hosts file |
| `--debounce-delay` | | `1s` | Debounce delay for event handling |
| `--debounce-max-delay` | | `5s` | Maximum debounce delay |

### Network and Alias Rules

`--networks/-n` keeps its historical comma-separated syntax. SDHM trims surrounding spaces, ignores empty items, removes duplicates while preserving first occurrence order, and requires at least one network.

The resolved default network receives both `alias` and `alias.<network>` names. Every secondary network receives only `alias.<network>`, preventing bare-name collisions. `--default-network` must name a monitored network. When it is omitted, `saltbox` is preferred if present; otherwise the first configured network becomes the default. Existing startup commands using only `--networks`, `--interval`, and `--health-port` therefore require no new flag.

### Time Duration Formats

Interval and debounce values accept positive Go durations, including milliseconds and compound values such as `500ms`, `1m30s`, and `2h15m`. Exact positive integer days such as `1d` or `7d` are also accepted. Zero and negative durations are rejected.

### Health Check Endpoint

SDHM provides a health check HTTP endpoint for monitoring:

```bash
# Check health status
curl http://127.0.0.1:8080/health
```

Response:
```json
{
  "error_count": 0,
  "errors": [],
  "healthy": true,
  "message": "No errors recorded",
  "status": "ok"
}
```

The endpoint returns HTTP 200 only when no concern is currently active and HTTP 503 while Docker snapshots, event streaming, hosts updates, or recovery remain failed. A concern clears only after that same operation succeeds; elapsed time alone never marks it recovered. Up to ten historical records remain available for diagnosis, so `error_count` is retained history rather than the number of active concerns. Active records use `unknown` recovery fields, while superseded or recovered records use `0s`.

## Running as a System Service

### systemd Service

Create `/etc/systemd/system/sdhm.service`:

```ini
[Unit]
Description=Saltbox Docker Hosts Manager
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

1. **Startup**: SDHM pings Docker, binds the health listener, and validates or prepares the managed hosts section before starting background work
2. **Discovery**: One authoritative snapshot inspects all running containers attached to any configured network; a partial or malformed snapshot is never applied
3. **Scheduling**: Network events are debounced, periodic validation requests immediate reconciliation, and failed work retries with bounded backoff
4. **Hosts File Update**: The complete snapshot replaces one managed section while preserving every byte outside it:
   ```
   # BEGIN DOCKER CONTAINERS
   172.18.0.2  mycontainer
   172.18.0.3  webserver
   # END DOCKER CONTAINERS
   ```
5. **Backup & Recovery**: Successful replacements refresh a valid backup transactionally. Corrupt markers are restored only from a backup with one valid ordered marker pair; a missing or invalid backup fails closed without replacing the target. `sdhm regenerate` remains available for an explicit fresh baseline.

## Development

### Building

```bash
make build
```

### Testing

```bash
# Run the complete local gate used by CI
make check

# Run tests with coverage
make test-coverage

# Run tests without the race detector
make test

# Run quick tests only
make test-short
```

### Code Quality

```bash
# Format code
make fmt

# Check formatting without modifying files
make fmt-check

# Check module files without modifying them
make tidy-check

# Run race-enabled tests
make test-race

# Run formatting, module, race, vet, and build gates
make check
```

Dependency maintenance is explicit and mutating:

```bash
# Update dependencies, then tidy the module
make update

# Tidy without updating dependency versions
make tidy
```

### Available Make Targets

Run `make help` to see all available targets:

```
 all              Run tests and build the binary
 build            Build the sdhm binary
 clean            Clean build artifacts and test files
 test             Run all tests (doesn't touch production /etc/hosts)
 test-short       Run short tests
 test-coverage    Run tests with coverage report
 deps             Download dependencies
 update           Update dependencies to latest versions
 tidy             Tidy go.mod
 fmt              Format code
 fmt-check        Check that tracked Go files are formatted
 vet              Run go vet
 tidy-check       Check module files without modifying them
 test-race        Run all tests with the race detector
 check            Run formatting, module, race, vet, and build gates
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
4. Run the complete gate: `make check`
5. Submit a pull request

## License

This project is licensed under the GNU General Public License v3.0 - see the [LICENSE](LICENSE) file for details.

## Support

- **Issues**: [GitHub Issues](https://github.com/saltyorg/sdhm/issues)

## Acknowledgments

Built for the [Saltbox](https://docs.saltbox.dev) project.
