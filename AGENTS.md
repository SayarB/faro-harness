# Project Instructions for Agents (`faro-harness`)

## Core Design Principle: Minimal Code, Model-Driven Harness

Keep the codebase minimal. Implement the smallest necessary version of a feature first and only add complexity when real usage proves it is needed.

### Put in code ONLY what must be in code
- **Hard boundaries**: security and safety invariants that cannot rely on model compliance—such as filesystem/path confinement, human approval gates, OS-level sandboxing, atomic writes/backups, timeouts, and output/resource limits.
- **Necessary tools & primitives**: clean, focused tools that give the model the capabilities it needs to read, search, compute, and deliver outputs.

### Put in prompts & guidance (not code)
- **Procedures and workflows**: multi-step habits, file-organization conventions, tool-selection order, formatting preferences, and soft guardrails.
- **Decision-making**: let the model decide how to approach a task using the provided tools and prompt guidance rather than hardcoding brittle rules, heuristics, or orchestration logic in Go.

### Practical rules
- **No premature abstraction**: do not introduce speculative interfaces, multi-backend frameworks, or "future-proofing" layers before a second concrete implementation actually needs them.
- **Be honest about enforcement**: never state in prompts, tool descriptions, or approval UI that a hard boundary is enforced by code/OS when it is only a convention.

## Quick Verification
```bash
go test ./internal/... ./cmd/...
go build ./cmd/faro-cli
```
