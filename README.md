# Distributed Collaborative Editor

This is a distributed collaborative text editor implementing Operational Transformation (OT) on top of the Raft consensus algorithm. It ensures high availability, strong consistency, and correct convergence of concurrent edits even in the face of network latency or server crashes.

Based on the [EasySync algorithm](easysync-algorithm-description.md) and embedding OT within the Raft state machine to solve the consensus/convergence problem gracefully.

## Architecture

1. **Client-Side OT**: A JavaScript client (`web/client.js`) maintains state `A` (server state), `X` (pending changes), and `Y` (unsubmitted changes).
2. **WebSocket Interface**: Clients communicate with the backend via WebSocket messages (`CONNECT`, `SUBMIT`, `ACK`, `BROADCAST`, `REDIRECT`).
3. **Raft Consensus**: Every server node runs `hashicorp/raft`. Raw submissions are placed into the Raft log.
4. **Deterministic State Machine**: When an entry commits in Raft, every node executes the exact same state machine `apply` function, performing OT mathematically to achieve convergence deterministically.
5. **Storage**: Raft log entries and metadata are stored persistently on disk using BoltDB.

## Requirements

- Go 1.21+
- Node/Browser for testing

## Building

To build the server executable:

```bash
go build -o server.exe ./cmd/server
```

## Running a Cluster Locally

You can spin up a 3-node cluster locally using different ports.

**Node 1 (Leader/Bootstrapper):**
```bash
go run ./cmd/server -id node-1 -grpc-addr localhost:12000 -ws-addr localhost:8080 -data-dir ./data/node-1 -peers localhost:12000,localhost:12001,localhost:12002 -bootstrap -init-text "Welcome to the Distributed Editor!"
```

**Node 2 (Follower):**
```bash
go run ./cmd/server -id node-2 -grpc-addr localhost:12001 -ws-addr localhost:8081 -data-dir ./data/node-2 -peers localhost:12000,localhost:12001,localhost:12002
```

**Node 3 (Follower):**
```bash
go run ./cmd/server -id node-3 -grpc-addr localhost:12002 -ws-addr localhost:8082 -data-dir ./data/node-3 -peers localhost:12000,localhost:12001,localhost:12002
```

Once running, you can connect clients by opening multiple browser tabs to any of the servers:
- `http://localhost:8080`
- `http://localhost:8081`
- `http://localhost:8082`

The follower nodes will correctly respond to client HTTP, provide catch-up connections, and `REDIRECT` writes to the leader.

## Testing

The project has comprehensive test coverage, proving both the mathematics of the OT algorithm and the distributed properties of the Raft cluster:

```bash
go test ./... -v
```

Highlights of the test suite:
- `internal/ot/ot_test.go`: Mathematic property-based checks verifying `Compose` and `Follow`.
- `tests/cluster_test.go`: Validating `A*f(A,B) == B*f(B,A)` over an in-memory 3-node cluster.
- `tests/crash_test.go`: Proving the cluster survives follower crashes, continues committing changes, and that duplicate submissions are discarded via the idempotency guard (the "No-Rollback" invariant).

## Clean Up

To start fresh, delete the data directory:

```bash
rm -rf ./data
```
