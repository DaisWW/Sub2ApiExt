# Sub2API Pulse Monitoring

`monitoring/` is a small, read-only worker and dashboard for the live account and group health of a Sub2API installation.

It discovers accounts and their real group memberships from PostgreSQL, probes eligible accounts with a minimal provider request, aggregates active groups, and stores its own history in `monitoring_*` tables. Inactive groups are shown as paused and are excluded from probe and failure totals. Probe failures never update `accounts.status`, `accounts.schedulable`, or routing state.

The worker is history-first. A recent real request from `usage_logs` is used instead of an active probe for a healthy account; accounts already marked `error` are always actively probed so recovery is detected even when there is no new traffic. When a probe is needed, model selection uses `credentials.monitor_model`, the latest `usage_logs.model` (mapped through `credentials.model_mapping`), a stable mapping target, and only then the platform default. A configured proxy failure is reported as a probe error; the worker never silently falls back to a direct connection.

The dashboard is available on port `8090` and exposes:

- 24-hour, 7-day, and 30-day views;
- all, group, and account filters;
- availability, first-byte fastest/median/slowest, and total-latency median;
- lazy history inspection;
- active probe button;
- consecutive-failure/recovery alerts and browser notifications.

Metrics use `usage_logs.first_token_ms` as the real request TTFT. For active probes, "first byte" is the HTTP first-response-byte approximation and is labeled accordingly in the history view. `ops_error_logs` is intentionally not unioned into request availability because retries can create duplicate error rows; probe failures remain the independent error signal.

The worker accepts `MONITORING_API_TOKEN` for a simple bearer-protected API. Keep it on a private network or behind the same reverse proxy as the main application.

## Deployment

Start the existing Sub2API and PostgreSQL containers, then double-click or run:

```bat
deploy.bat
```

The installer builds `sub2api-ext-monitoring:local`, discovers the existing Sub2API Docker network and database credentials, and installs the runtime files under `C:\ProgramData\Sub2API\extensions\monitoring`. The running container has no bind mount to this Git checkout and uses `restart: unless-stopped`.

The dashboard defaults to `http://localhost:18090`. Change `MONITORING_BIND_HOST` or `MONITORING_PORT` in the installed `.env` file, then run `manage.bat restart` from the runtime directory.

The worker uses the same `DATABASE_*` variables as `sub2api`. It does not require the gateway's TOTP encryption key because account credentials are stored as JSONB in the existing database and are only held in memory for the request that is currently being probed.

Useful settings:

| Variable | Default | Meaning |
| --- | --- | --- |
| `MONITORING_LISTEN_ADDR` | `:8090` | HTTP bind address |
| `MONITORING_INTERVAL` | `60s` | Probe interval |
| `MONITORING_REQUEST_TIMEOUT` | `30s` | Per-request timeout |
| `MONITORING_WINDOW_DAYS` | `7` | Dashboard default window |
| `MONITORING_PROBE_CONCURRENCY` | `8` | Account probe concurrency |
| `MONITORING_FAILURE_THRESHOLD` | `2` | Consecutive failures before alert |
| `MONITORING_RECOVERY_THRESHOLD` | `1` | Consecutive successes before recovery alert |
| `MONITORING_ALLOW_PRIVATE_HOSTS` | `false` | Allow explicitly configured internal endpoints |
| `MONITORING_API_TOKEN` | empty | Optional bearer token |

For OAuth/cookie accounts, the worker looks for the existing credential keys (`access_token`, `token`, `session_key`, and similar). Account-specific `base_url`, `endpoint`, and `monitor_model` credential overrides are respected. Provider-specific token refresh remains the responsibility of the main gateway; a failed refresh is surfaced as an alert rather than silently mutating the account.
