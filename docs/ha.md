# High Availability

The collector supports an optional **Active/Standby** high-availability mode.
It is designed for a two-node deployment:

- **Primary**: preferred node, typically the DC instance.
- **Standby**: backup node, typically the DR instance.

The HA manager lives in `internal/ha/ha.go` and is wired from
`cmd/snmpcollector/main.go`.

## Behaviour

### Primary role

On startup, the Primary performs a best-effort `POST /demote` to the peer.
If the peer is reachable, it yields before the Primary begins polling. If the
peer is down, the Primary proceeds to Active anyway.

If the Primary process restarts later, it repeats the same preemption step and
reclaims the preferred role.

### Standby role

The Standby starts idle and polls the peer's `GET /health` endpoint every
`ha.health_check_interval`. After `ha.failover_threshold` consecutive failures,
it promotes itself to Active and starts polling.

When the Primary comes back, it sends `POST /demote` to the Standby. The
handler transitions the Standby back to idle, calls the stop-polling callback,
and only then returns `200 OK`. That makes the handoff deterministic: the
Primary does not resume until the Standby has stopped its collector pipeline.

## Wiring

The runtime wiring is:

1. `ha.OnStartPolling(func() { app.Start(ctx) })`
2. `ha.OnStopPolling(func() { app.Stop() })`
3. `health.NewServer(addr, collectorID, logger)`
4. `healthSrv.Handle("/demote", ha.DemoteHandler())`
5. `ha.Start(ctx)`

The health server must be running for HA to work because it exposes both the
peer health endpoint and the `/demote` control endpoint.

## Configuration

```yaml
ha:
  enabled: true
  role: primary
  peer_url: "http://10.0.0.2:9080"
  health_check_interval: 5s
  health_check_timeout: 5s
  failover_threshold: 3
  demote_timeout: 30s
```

Only these fields are exposed as CLI flags today:

- `-ha.enable`
- `-ha.role`
- `-ha.peer.url`

The timing fields are YAML-only and fall back to the defaults above when
omitted.

## Operational Notes

- Set `health.addr` on both nodes so each one can serve `/health` and `/demote`.
- Use `ha.role: primary` on the preferred node and `ha.role: standby` on the
  backup node.
- `ha.peer.url` should point at the peer's health server base URL, for example
  `http://localhost:9081`.
- `ha.demote_timeout` must be long enough for the Standby's pipeline to drain
  in-flight work before the Primary resumes polling.
