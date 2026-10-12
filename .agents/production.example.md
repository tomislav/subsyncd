# Production access (local template)

Copy this file to `production.local.md` in the same directory and fill it with operator-approved deployment facts. The local file is intentionally ignored by Git and may be read by agents working in this checkout.

Record only:

- host name and an already configured access alias;
- deployment manager and project name;
- exact Compose, configuration, and data paths;
- where database and configuration backups go, and the backup command to run before a migration, database write, or config edit;
- service/container names and network exposure;
- safe read-only status and log commands;
- the exact prefix for running subsyncd commands in the container (for example `docker compose exec -T subsyncd subsyncd ... --json`), and whether a manual `search` through the running daemon is approved;
- explicit approval boundaries for restarts, deployments, database operations, and media writes;
- a dated, clearly labeled last-observed state when useful.

Never record passwords, API keys, tokens, private-key material, `.env` contents, cookie values, or commands that print resolved environments. Treat absence or staleness as a reason to rediscover facts with read-only commands, not as permission to guess or mutate production.
