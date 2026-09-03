# Diagram sources

Every diagram in [`../architecture.md`](../architecture.md) is also kept here as a
standalone Mermaid source file, so it can be rendered on its own, embedded in a
slide, or diffed without diffing the prose around it.

| File | Diagram type | Shows |
|---|---|---|
| `context.mmd` | C4 context | The BFF, its two data sources, the cache, the identity provider and the telemetry backend, and who talks to what. |
| `container.mmd` | C4 container | The deployable processes and ports: BFF API `:8080`, BFF admin `:9090`, `opsource` `:9101`/`:9111`, `exsource` `:9102`/`:9112`, Redis, collector, Jaeger, Prometheus, Grafana. |
| `component.mmd` | Component / package graph | Every import edge between `internal/**` packages, with the arrow pointing from importer to imported. |
| `seq-ttl-hit.mmd` | Sequence | `/status` served from a fresh operational record; the execution source is never called. |
| `seq-ttl-miss.mmd` | Sequence | The same request when the record is past its TTL, routed to the execution source by `ttl.operational.stale`. |
| `seq-both-fanout.mmd` | Sequence | `/details`: three concurrent tasks, per-source timeouts, required vs optional sources, precedence merge. |
| `seq-fallback.mmd` | Sequence | The call-time fallback `fallback.primary_failed`, which catches the first failures of an outage before the breaker has opened. |
| `seq-stale-degrade.mmd` | Sequence | Both sources down, an expired-but-resident cache entry served as `degrade.stale_cache`. |
| `breaker-states.mmd` | State machine | Closed / Open / HalfOpen with the real thresholds from `configs/bff.yaml`. |
| `routing-chain.mmd` | Flowchart | All eleven pre-flight rules — rule 4 drawn as a *pin-and-continue* node rather than a terminal one — plus `fallback.primary_failed` as a distinct post-routing step and the degradation ladder. |
| `deployment.mmd` | Deployment | ALB → EKS → BFF pods → ElastiCache / ODS / EDS / ADOT collector. |

## Rendering

GitHub renders `.mmd` fences inline in Markdown; these standalone files are for
everything else.

```bash
# One diagram to SVG (npm i -g @mermaid-js/mermaid-cli)
mmdc -i docs/diagrams/routing-chain.mmd -o routing-chain.svg

# All of them, dark and light
for f in docs/diagrams/*.mmd; do
  mmdc -i "$f" -o "${f%.mmd}.svg" -t neutral -b transparent
done
```

`context.mmd` and `container.mmd` use Mermaid's C4 syntax, which some renderers
still treat as experimental. If your renderer rejects them, everything else in
this directory uses `flowchart`, `sequenceDiagram` or `stateDiagram-v2`, which
are universally supported.

## Keeping them honest

These diagrams are written against the code, not against an intention. When you
change one of the following, change the diagram in the same commit:

| If you change | Update |
|---|---|
| The rule chain in `internal/router/router.go` | `routing-chain.mmd`, and the rule table in `../architecture.md` |
| A package's imports | `component.mmd` |
| A port, in `docker-compose.yaml` or `docs/DESIGN-CONTRACT.md` §1 | `container.mmd`, `context.mmd` |
| Breaker configuration in `configs/bff.yaml` | `breaker-states.mmd` |
| `required_sources` or `per_source_timeout` | `seq-both-fanout.mmd` |
| The degradation ladder in `internal/application/service.go` | `seq-fallback.mmd`, `seq-stale-degrade.mmd`, `routing-chain.mmd` |
