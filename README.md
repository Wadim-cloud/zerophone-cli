# ZeroPhone CLI

A terminal-based client for [ZeroPhone](https://github.com/Wadim-cloud/zerophone) — a distributed VoIP signaling server with WebRTC voice calling.

![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)

## Features

- Browse online/offline nodes on your ZeroTier network
- Initiate voice calls with a single keypress
- Answer/reject incoming calls from the terminal
- **Delete offline nodes** from the network (press `d`)
- Active call timer display
- Real-time updates via WebSocket signaling
- ANSI color output — no heavy TUI dependencies
- Config stored in `~/.zerophone-cli.json`

## Installation

```bash
git clone https://github.com/Wadim-cloud/zerophone-cli.git
cd zerophone-cli
go build -o zerophone-cli .
```

Or download pre-built binaries from [Releases](https://github.com/Wadim-cloud/zerophone-cli/releases).

## Configuration

Create `~/.zerophone-cli.json`:

```json
{
  "network_id": "a84ac5c123456789",
  "node_id": "your-zerotier-node-id",
  "name": "Your Display Name",
  "server_addr": "http://localhost:8080"
}
```

- `network_id` — Your ZeroTier network ID (16-digit hex)
- `node_id` — This node's ZeroTier network ID (used for registration)
- `name` — Display name shown to other nodes
- `server_addr` — ZeroPhone server URL (default: `http://localhost:8080`)

## Usage

```bash
./zerophone-cli
```

Once running:

| Key | Action |
|-----|--------|
| `↑` / `↓` | Select node |
| `Enter` | Call selected / End active call |
| `a` | Answer incoming call |
| `R` (Shift+R) | Reject incoming call |
| `d` | Delete offline node (when selected) |
| `r` | Refresh node list |
| `q` or `Ctrl+C` | Quit |

## Node Deletion

You can delete **offline nodes** from the network to keep your list clean:
1. Navigate to an offline node with arrow keys
2. Press `d`
3. Node will be removed from the server and UI

Online nodes cannot be deleted (they'll re-discover themselves).

## How It Works

1. On first launch, fill in `~/.zerophone-cli.json` with your ZeroTier credentials
2. Run the CLI — it registers with the ZeroPhone server
3. Nodes on the same ZeroTier network are fetched and displayed
4. Use arrow keys to select an online node and press Enter to call
5. Incoming calls pop up with options to Answer or Reject
6. When connected, press Enter to hang up

The CLI uses:
- **HTTP REST API** for registration, node listing, signaling, and node deletion
- **WebSocket** for real-time incoming call notifications
- **Long-polling fallback** if WebSocket is unavailable

## Requirements

- Go 1.21+ (to build from source)
- ZeroPhone server running and accessible
- Terminal with ANSI color support
- Microphone (for voice calls — controlled by browser WebRTC when web client is used; CLI only manages signaling)

## Screenshot

```
  ZeroPhone CLI  [Online]   distributed VoIP

  ┌─ Nodes on a84ac5c123456789 ────────────────┐
  │                                            │
  │  ▶ My-Laptop            online             │
  │    Home-PC              offline            │
  │    Phone-Android        online             │
  │                                            │
  │  Total: 3  Online: 2  Offline: 1           │
  └────────────────────────────────────────────┘

  [↑↓]Select  [Enter]Call/End  [a]Ans  [R]Rej  \
  [d]Del  [r]Ref  [q]Quit  19:57:23
```

## License

MIT
