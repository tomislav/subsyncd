# Development reference

These documents describe the behavior the implementation must preserve. They are living references, not task histories or design-approval records.

- [Architecture](../architecture.md) describes component boundaries, durable state, and the end-to-end processing flow.
- [Operations internals](operations.md) covers persistence, reconciliation, webhooks, filesystem safety, locking, migrations, and recovery.
- [Provider and matching internals](providers.md) covers provider adapters, routing, scoring, caches, cooldowns, LAPSE, rejections, and scheduling.
- [Logging internals](logging.md) defines structured events, levels, correlation fields, and privacy rules.
- [Silo API reference](../references/silo.md) records the external notification contract and local path-mapping decisions.

Update the relevant reference when behavior changes. Completed plans, review transcripts, verification ledgers, and implementation status belong in Git history rather than the working tree.
