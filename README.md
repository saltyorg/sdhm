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

`--health-addr` must be a literal IPv4 or IPv6 address, and `--health-port` must be in `1..65535`. The health endpoint is registered as `GET /health`; unsupported methods receive HTTP 405.

### Network and Alias Rules

`--networks/-n` keeps its historical comma-separated syntax. SDHM trims surrounding spaces, ignores empty items, removes duplicates while preserving first occurrence order, and requires at least one network.

The resolved default network receives both `alias` and `alias.<network>` names. Every secondary network receives only `alias.<network>`, preventing bare-name collisions. `--default-network` must name a monitored network. When it is omitted, `saltbox` is preferred if present; otherwise the first configured network becomes the default. Existing startup commands using only `--networks`, `--interval`, and `--health-port` therefore require no new flag.

### Time Duration Formats

Interval and debounce values accept positive, case-sensitive Go durations, including milliseconds and compound values such as `500ms`, `1m30s`, and `2h15m`. Exact positive integer days such as `1d` or `7d` are also accepted. Zero and negative durations are rejected.

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

From the moment the listener starts, the endpoint returns HTTP 503 with `message: "System initializing"` until hosts preparation and the first reconciliation attempt have completed. This readiness gate does not add an error record. After initialization, the endpoint returns HTTP 200 only when no concern is currently active and HTTP 503 while Docker snapshots, event streaming, hosts updates, or recovery remain failed. A failed first reconciliation therefore stays at 503 under its actual Docker or hosts concern. A concern clears only after that same operation succeeds; elapsed time alone never marks it recovered. Up to ten historical records remain available for diagnosis, so `error_count` is retained history rather than the number of active concerns. Active records use `unknown` recovery fields, while superseded or recovered records use `0s`.

## Running as a System Service

### systemd Service

Create `/etc/systemd/system/sdhm.service`:

```ini
[Unit]
Description=Saltbox Docker Hosts Manager
After=docker.service
BindsTo=docker.service

[Service]
Type=simple
ExecStart=/usr/local/bin/sdhm --networks saltbox --interval 5m
Restart=always
RestartSec=10

[Install]
WantedBy=docker.service
```

This binding stops SDHM during Docker teardown before authoritative transitional snapshots can rewrite the last converged hosts state.

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

### Operational Logs

Health answers the current question: whether SDHM is ready and whether an active concern makes it unhealthy. The journal answers the historical question: when the daemon crossed a lifecycle, failure, recovery, or automatic-repair boundary. A healthy response does not erase the corresponding warning from the journal.

For the complete Saltbox service history, use:

```bash
sudo journalctl -u saltbox_managed_docker_update_hosts.service
```

SDHM writes native journal priorities when systemd provides `JOURNAL_STREAM`, so warning-or-higher transitions can be filtered without parsing `MESSAGE`:

```bash
sudo journalctl -p warning -u saltbox_managed_docker_update_hosts.service
```

The implemented daemon transition records are:

| Transition | Level / expected journal priority | Message and fields | Meaning |
|---|---|---|---|
| Process invocation | INFO / 6 | `starting SDHM`; `version`, `networks`, `default_network`, `interval`, `health_addr` | SDHM has been invoked. `health_addr` is the configured bound address and port. |
| Initial reconciliation succeeds | INFO / 6 | `SDHM ready` | The listener is bound, preparation completed, and the first authoritative reconciliation succeeded. Health may now return 200 when no current concern is active. |
| Initial reconciliation fails | WARN / 4 | `reconciliation failed`; `phase=initial`, `err`, `retry_in` | Initialization finished but the first reconciliation failed. Health remains 503 until its Docker or hosts concern succeeds. |
| Later reconciliation fails | WARN / 4 | `reconciliation failed`; `err`, `retry_in` | A previously running reconciliation failed and will retry with bounded backoff. |
| Reconciliation recovers | INFO / 6 | `reconciliation recovered` | A reconciliation succeeded after a logged reconciliation failure. |
| Docker event stream fails | WARN / 4 | `Docker event stream unavailable`; `err`, `retry_in` | Event observation is unavailable and will reconnect with bounded backoff. |
| Docker event stream recovers | INFO / 6 | `Docker event stream recovered`; `evidence=event` or `evidence=stable` | A previously failed stream delivered an event, or stayed connected for its stability interval. |
| Marker repair succeeds | WARN / 4 | `hosts file recovered from validated backup` | Preparation restored corrupt target markers from a validated backup. |
| Late replacement rollback succeeds | WARN / 4 | `reconciliation failed`; `err` includes `target restored from retained snapshot` | A replacement failed after changing the target, but SDHM restored the retained snapshot. |
| Clean daemon completion | INFO / 6 | `SDHM stopped` | `Run` completed cleanly after shutdown cleanup; it is not emitted for a terminal daemon error. |
| Terminal command error | ERROR / 3 | One concise error string, no fields | The command is exiting unsuccessfully; invalid configuration does not add a Cobra usage dump. |

The `sdhm regenerate` command additionally records `regenerating hosts file` at INFO with its `path`, and either command warns `SDHM should run as root to modify the hosts file` when it is not running as root.

SDHM intentionally does **not** log individual Docker events, container names, unmonitored networks, successful periodic no-op reconciliations, ordinary successful hosts mutations, repeated reconnect failures with the same error, abandoned health-response writes, or intentionally unpublishable endpoint omissions. This keeps the journal a bounded transition history instead of an update-by-update event feed.

Native-priority prefix mapping is covered by local tests. This documentation does not claim live systemd journal-priority acceptance; that VM qualification remains pending.


## How It Works

1. **Startup**: SDHM pings Docker, binds the health listener in an initializing 503 state, validates or prepares the managed hosts section, and attempts one authoritative reconciliation before reporting readiness or starting background work
2. **Discovery**: One authoritative list/inspect pass gathers every publishable endpoint on the configured networks. Container-list or non-not-found inspect failures preserve the current hosts file, while endpoints without usable settings, an IP address, or aliases are omitted because they have no valid hosts mapping. In the verified crash-loop case, Docker kept the restarting container listed while clearing its IP, so omission prevents it from blocking healthy updates
3. **Scheduling**: Network events are debounced, periodic validation requests immediate reconciliation, and failed work retries with bounded backoff
4. **Hosts File Update**: The complete publishable snapshot replaces one managed section while preserving every byte outside it:
   ```
   # BEGIN DOCKER CONTAINERS
   172.18.0.2  mycontainer
   172.18.0.3  webserver
   # END DOCKER CONTAINERS
   ```
5. **Backup & Recovery**: Successful replacements refresh a valid backup transactionally. Corrupt markers are restored only from a backup with one valid ordered marker pair; a missing or invalid backup fails closed without replacing the target. Symlink, FIFO, directory, and other non-regular target or backup paths are rejected rather than replaced. `sdhm regenerate` remains available for an explicit fresh baseline.

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
