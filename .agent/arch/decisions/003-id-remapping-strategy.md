# ADR-003: Session-Prefixed ID Remapping Strategy

## Status

Accepted

## Date

2026-03-14

## Context

JSON-RPC 2.0 uses an `id` field to correlate requests with responses. When multiple downstream clients send requests through a shared upstream, their `id` values can collide (e.g., both send `id: 1`).

Options considered:

1. **Session prefix** — rewrite `id` as `"s{N}:{original_id}"` before sending upstream
2. **Global incrementing counter** — assign monotonic IDs, maintain mapping table
3. **UUID replacement** — replace all IDs with UUIDs, maintain mapping table

## Decision

**Session prefix** (option 1).

## Rationale

- Simplest to implement and debug — the session is visible in the id string
- No mapping table needed for routing — parse the prefix to find the session
- JSON-RPC allows `id` to be string, number, or null — upstream servers MUST echo back the same `id` they received, so string IDs are universally supported
- Low overhead — string concatenation vs table lookup
- Debuggable — intercepting traffic shows which session each request belongs to

## Format

```
Downstream id: 1        →  Upstream id: "s1:1"
Downstream id: "abc"    →  Upstream id: "s1:abc"
Downstream id: 42       →  Upstream id: "s1:42"
```

Response routing:
```
Upstream response id: "s1:42"  →  parse prefix "s1" → Session 1, restore id: 42
```

## Consequences

- Upstream servers MUST faithfully echo the `id` field — this is a JSON-RPC 2.0 requirement, so all compliant servers do this
- Original `id` type must be preserved on de-remap (number stays number, string stays string)
- Notifications (`id` absent) are broadcast to all sessions — no remapping needed
