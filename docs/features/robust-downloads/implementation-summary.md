# Implementation summary — Robust downloads

**Feature:** `robust-downloads` · **Package:** `pkg/download` · **Status:** implemented

## What was built

`pkg/download` now resumes interrupted downloads, retries transient failures, validates content before promotion, and sweeps stale partials only in safe album contexts.

### Modules

| File | Role |
|---|---|
| `fetch.go` | Per-photo `fetcher` value type: try loop, `plainGet` / `rangeGet`, `ingest`, `promote`, `copyCapped` |
| `validate.go` | Outcome taxonomy helpers, status/transport classification, header parsing, backoff, `validateFile` / HTML sniff |
| `sweep.go` | End-of-album ID-absent `.part`/`.cand` removal |
| `downloader.go` | Orchestration: 60s `http.Client`, limiter-per-try, `allowIDAbsentSweep`, enqueue-time `seenIDs` |

### Outcome taxonomy (BR1)

Each try resolves to exactly one of:

- **Done** — validated file renamed to final (only after `promote` succeeds)
- **Incomplete** — preserve authoritative `.part`; consumes a try; retried up to 3 tries
- **Unusable** — delete the offending file (candidate or fresh bad `.part`); consumes a try
- **Fatal** — no retry (hard 4xx, filesystem errors, parent-context cancellation)

HTTP **416** is a **signal**, not an outcome (explicit branch in `rangeGet`).

### On-disk file model

| File | Role |
|---|---|
| `{id}.{ext}` | Final authoritative file; only target of cross-album hardlink map |
| `{id}.{ext}.part` | Authoritative partial; survives incomplete/fatal/cancel; opened append-only when non-empty |
| `{id}.{ext}.cand` | Disposable candidate (Range ignored / 416 mismatch / Gate-A fallback); deleted at try start and on any non-Done result |
| `{id}.{ext}.tmp` | Legacy only — never written, never resumed, excluded from `alreadyDownloaded` |

### Retry / backoff

- Max **3 tries**; sleep only between tries that will actually run (2 sleeps max)
- Backoff **2s / 4s** × (1 + jitter ∈ [0, 0.2]); 429 may use `Retry-After` (cap 60s)
- 60s client timeout preserved on `http.Client.Timeout` → Incomplete, `.part` kept

### Validation rules (high level)

- Expected total: `< 0` unknown (skip size check), `== 0` empty, `> 0` known
- Zero-byte always Unusable (independent of total)
- Size vs total → Incomplete / Unusable; HTML Content-Type or on-disk sniff → Unusable
- Gated 206 append that would validate Unusable is **downgraded** to Incomplete (`.part` kept)
- Read-side copy errors short-circuit to Incomplete **without** validation

### Sweep

- Runs after workers finish, only if `allowIDAbsentSweep && allPagesOK && parentCtx` clean
- Deletes `*.part` / `*.cand` whose leading ID segment is absent from enqueue-time `seenIDs`
- Never outside fully-listed per-album directories

### ADRs

See `docs/architecture/ADR-001.md` … `ADR-014.md` (fetcher value type; sleeper/jitter; end-of-album sweep; parent-ctx cancellation; client timeout location; read/write split; `allowIDAbsentSweep`; 416 branch; validate/promote errors; length sentinel; ingest extraction; rangeGet classification; best-effort promote cleanup; read-error short-circuit). Output-root locking and local reuse: `docs/architecture/ADR-023.md`.

## Per-task AC mapping (summary)

| Tasks (approx.) | Acceptance criteria covered |
|---|---|
| Core try loop / backoff / transport | AC-001, AC-002, AC-003, AC-004, AC-015 |
| Size tracking & validation | AC-005, AC-006, AC-014, AC-016 |
| Range resume & gates | AC-007, AC-008, AC-009, AC-010, AC-011 |
| Candidate / 416 / promote | AC-012, AC-013, AC-019, AC-020 |
| Album sweep scope | AC-017, AC-018 |
| README / known limitations | AC-003 & AC-017 documented; ADR-001…014, ADR-023 |

This table is the reuse map for reviewers and future features touching `pkg/download`.

## Notes for future work

- Extension-change orphaning of `{id}.{old-ext}.part` remains a known limitation (document in README).
- Concurrent CLI instances on one tree are serialized by the output-root lock (ADR-023); a second process is rejected rather than racing.
- Legacy `.tmp` is never resumed; a one-shot migrator could delete or rename them if needed.
