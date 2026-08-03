# Background Refresh and Baseline Design

## Goals

- Keep the direct-egress baseline available when one public-IP provider fails.
- Let administrators configure source refresh and full-pool recheck independently.
- Make full-pool rechecks include both available and unavailable forwarding proxies.
- Remove a forwarding proxy after three consecutive full-recheck failures.
- Expose the effective schedules and recent/next execution times in the dashboard.

## Baseline probing

The baseline probe races these HTTPS endpoints through direct, no-proxy clients under one shared timeout:

- `https://api64.ipify.org/`
- `https://checkip.amazonaws.com/`
- `https://ident.me/`

The first valid IP wins and cancels the remaining requests. Redirects stay disabled, environment proxy variables remain bypassed, invalid responses are ignored, and total elapsed time remains bounded by the caller's timeout.

## Independent schedules

`PoolConfig` stores two runtime settings in seconds:

- Source refresh interval: controls scheduled source downloads and sampled candidate checks.
- Full-pool recheck interval: controls exhaustive checks of every retained forwarding proxy.

A zero value follows the corresponding CLI default. The existing `-scrape-interval` remains the source-refresh default. A new CLI full-recheck interval provides the second default. Validation applies bounded positive ranges, and dashboard/API responses return effective values plus overrides.

The source scheduler continues scanning at a bounded cadence, queues enabled sources when their effective source interval is due, and records source-refresh activity separately from full-pool checks. The full-recheck scheduler uses its current effective interval after each completed cycle so administrator changes apply without restart.

## Full-pool recheck semantics

A full-pool recheck examines all retained, enabled-source forwarding proxies, including proxies currently marked unavailable or terminally failed. Automatic full-recheck results are allowed to recover an unavailable proxy after a successful probe.

Each completed failed full-recheck increments the proxy's consecutive health-failure count. A successful full recheck resets that count. When the count reaches three, the proxy and its associated statistics are removed from the forwarding pool. Candidate-catalog membership is unaffected; a future source refresh may discover and validate the endpoint again as a new pool member.

Bounded periodic slice checks remain for lightweight health maintenance, but deletion is tied only to exhaustive full-pool cycles. Canceled or superseded cycles do not increment failures or delete nodes.

## Dashboard and API

The detection-options area exposes separate source-refresh and full-pool-recheck interval inputs. It shows the effective values and whether each follows its CLI default. Saving schedule changes affects the next cycle without restarting the process.

Compact status includes recent and next timestamps for both source refresh and full-pool recheck. The existing 15-second dashboard status poll renders those values. Node pages continue their separate bounded refresh cadence.

## Error handling

- If every baseline endpoint fails, the previous baseline is retained; with no previous value, IP-change state remains unknown.
- Source refresh failures preserve the last-known catalog for that source.
- Full-pool recheck cancellation leaves membership and failure counters unchanged for unfinished work.
- Persistence errors are logged and surfaced through existing API error handling where the change originated.

## Tests

- Baseline race selects the first valid response and does not wait for a blocked endpoint.
- Baseline requests bypass environment proxies and share the caller's deadline.
- Schedule configuration validates, persists, and reports effective/default values.
- A full recheck includes unavailable terminal nodes and can recover them.
- Three completed full-recheck failures remove a node; a success resets the count.
- Canceled or stale-generation full rechecks do not delete nodes.
- Dashboard contract tests cover both effective schedules and their timestamps.
