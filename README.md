# tmdb-relay

A small anonymous, read-only TMDB HTTP relay written in Go, with a Valkey response cache. Clients send ordinary versioned requests such as `/3/movie/550`; the relay supplies the operator's private **TMDB API Read Access Token**. There are no client credentials, registrations, application request quotas, frontend, SQL database, accounts, search modifications, captured searches, or admin endpoints. TMDB's upstream rate limits and terms still apply.

## Run with Docker

1. A local `.env` has been created for this checkout. On another checkout, copy `.env.example` to `.env`.
2. Set `TMDB_READ_ACCESS_TOKEN` in `.env` to your API Read Access Token from TMDB API settings, without a `Bearer ` prefix. Use the application read token, not a user access token or session credential.
3. Run:

```sh
docker compose up --build -d
curl http://localhost:8080/healthz
curl http://localhost:8080/readyz
curl http://localhost:8080/config
curl 'http://localhost:8080/3/movie/550?language=fr-FR&append_to_response=credits,images'
curl 'http://localhost:8080/3/search/movie?query=Alien&page=1&region=FR'
curl 'http://localhost:8080/4/list/1?page=1'
```

Public list IDs can disappear or become private; TMDB may return an error for the example. Do not add Authorization, API keys, sessions, or cookies to client requests.

Compose supplies a private-network Valkey with a **256 MiB maximum dataset size and allkeys-lru eviction**, no persistence, and no published cache port. Eviction causes an ordinary cache miss. Valkey process overhead can exceed its dataset limit. Set `PORT` in `.env` to choose the external Docker port (default `8080`); the service uses its built-in `:8080` listener inside the container. Leave `LISTEN_ADDR` unset for Docker. Put your own HTTPS ingress in front of it for public use. `.env` is the single source for service settings: Compose passes it through without replacing `VALKEY_URL`. Its default `redis://valkey:6379/0` reaches the bundled cache; change it in `.env` to use an external cache. The container runs as an unprivileged user with a read-only filesystem. Stop with `docker compose down`.

## Production with released containers

Download `compose.prod.yaml` and `env.example` from a GitHub release into a deployment directory, copy `env.example` to `.env`, and set your TMDB token. If using the repository instead, copy `.env.example`. Keep `VALKEY_URL=redis://valkey:6379/0` for the bundled cache and choose an available external `PORT`. Only the production Compose file and your `.env` are needed to run; no source checkout or Go toolchain is required.

```sh
docker compose -f compose.prod.yaml pull
docker compose -f compose.prod.yaml up -d --wait
docker compose -f compose.prod.yaml ps
```

The default image is `ghcr.io/thegeeking/tmdb-proxy:1`, which becomes available after the first v1 release has been published. For reproducible deployments, set `RELAY_IMAGE` in `.env` to an exact version such as `ghcr.io/thegeeking/tmdb-proxy:1.0.0` or an image digest. Public package visibility is needed for anonymous pulls; otherwise log in to GHCR first.

The production file runs the relay and Valkey with read-only filesystems, no Linux capabilities, process/CPU/memory limits, health checks and rotated logs. Valkey has no published port, a 256 MiB dataset limit with LRU eviction, and a 512 MiB container memory limit allowing room for overhead. The relay also has a 512 MiB memory limit. These are starting limits: increase them for workloads with many simultaneous distinct requests or large responses. The cache is disposable and has no persistent volume; recreating Valkey clears it. Docker restarts exited processes; a failing health check alone does not restart a container.

The external port serves HTTP; terminate HTTPS at your ingress/reverse proxy. Leave `LISTEN_ADDR` unset so port mapping and health checks use the built-in internal port 8080. Do not run development and production stacks against the same project at the same time. From an existing checkout, stop the development stack before switching to the production file.

To update, run the same `pull` and `up -d --wait` commands again. With a major/minor alias this applies the image currently published for that alias; containers do not update themselves. To roll back, set `RELAY_IMAGE` to a prior exact version and rerun those commands. Inspect logs with `docker compose -f compose.prod.yaml logs --tail=100`; stop with `docker compose -f compose.prod.yaml down`.

## Development with automatic rebuilds

Use the same `.env` and the same two services:

```sh
docker compose up --build --watch
```

Compose Watch rebuilds and replaces only the Go relay when Go sources, go.mod, go.sum, routes.txt or Dockerfile change. Valkey keeps running. No local Go installation, separate development environment file, or Go watcher dependency is needed. This is a full rebuild/restart rather than in-process hot reload. See [Docker's Compose Watch documentation](https://docs.docker.com/compose/how-tos/file-watch/). Your Compose version must support watch `include` patterns; `docker compose config --quiet` checks the configuration.

After changing `.env` or compose.yaml, rerun the command so the containers receive the new configuration. `.env` is deliberately excluded from source watching and the image build.

## Optional native Go execution

For development outside Docker, install Go 1.26.10 or newer and provide a reachable Valkey instance. The Docker hostname `valkey` is only available inside the Compose network. Override `VALKEY_URL` in the process environment with that instance's address, then run `go run .` (or build with `go build -o tmdb-relay .`). The bundled cache is not published to the host. Plain `go run` does not watch files; use the Compose workflow above for automatic rebuilds.

The executable reads an optional `.env` in its current working directory. It accepts literal `KEY=value` lines, blank lines, comments starting with `#`, and optional matching single/double quotes. It does not execute shell expressions, expand variables, or parse inline comments. Existing process environment variables take precedence, including empty values. `.env` is ignored by Git and excluded from the Docker build. Never commit it or include it in support logs.

## Configuration

Only the token is required; unset/empty optional settings use these defaults. Invalid settings fail startup without printing their values. Valkey must be reachable at startup; subsequent cache failures use the fallback described below.

| Variable | Default | Meaning |
| --- | --- | --- |
| `TMDB_READ_ACCESS_TOKEN` | required | Private application read token; no whitespace or Bearer prefix. |
| `PORT` | `8080` | External Docker port used by Compose; ignored when running Go directly. |
| `RELAY_IMAGE` | `ghcr.io/thegeeking/tmdb-proxy:1` | Production Compose image; accepts a version tag or digest. |
| `LISTEN_ADDR` | `:8080` | HTTP host:port; port 1–65535. |
| `VALKEY_URL` | `redis://valkey:6379/0` | Single Valkey instance, database selection, optional authentication. |
| `TMDB_API_VERSIONS` | `3,4` | Comma-separated subset of `3,4`. |
| `CACHE_TTL_DETAILS_SECONDS` | `86400` | Details and remaining public resources: 24 hours. |
| `CACHE_TTL_SEARCH_SECONDS` | `300` | `/3/search/*`: 5 minutes. |
| `CACHE_TTL_LISTS_SECONDS` | `900` | Lists, discover, movie lists, popular/top-rated/now-playing/upcoming/airing/on-the-air collections: 15 minutes. |
| `CACHE_TTL_TRENDING_SECONDS` | `300` | `/3/trending/*`: 5 minutes. |
| `CACHE_TTL_REFERENCE_SECONDS` | `604800` | Configuration, certifications, genres, watch-provider catalogs: 7 days. |
| `TMDB_REQUESTS_PER_SECOND` | `40` | Upstream requests/second per process, shared across all routes; evenly paced without bursts. |
| `TMDB_QUEUE_SIZE` | `200` | Maximum distinct misses waiting for dispatch; excess receives local 429 with Retry-After. |
| `TMDB_TIMEOUT_SECONDS` | `15` | Deadline for the complete upstream request including body. |
| `VALKEY_TIMEOUT_SECONDS` | `2` | Deadline per cache operation and connection timeout. |
| `MAX_RESPONSE_BYTES` | `10485760` | Maximum upstream body, successful or otherwise: 10 MiB. |
| `CORS_ALLOWED_ORIGINS` | empty | CORS disabled; comma-separated exact HTTP(S) origins without trailing slash, or `*`. No credentialed CORS. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. |

Rate and queue size are integers from 1 to 10000. TTL and timeout values are positive integer seconds up to 2592000 (30 days); response size is 1–1073741824 bytes. Keep timeouts small in practice. CORS controls browsers, not access to this anonymous service. GET preflights are supported for configured origins without custom request headers.

Authenticated and TLS cache examples:

```dotenv
VALKEY_URL=redis://default:percent-encoded-password@valkey:6379/0
VALKEY_URL=rediss://username:percent-encoded-password@cache.example.com:6380/0
```

Percent-encode special characters in username/password. `rediss` verifies the server certificate using system roots; there is no insecure TLS option. URL query options, fragments, Unix sockets, Sentinel, and clusters are outside this small service's configuration contract. Keep Valkey private and use authentication/TLS across untrusted networks. The cache contains public TMDB response data.

## API coverage and safety

The exact finite allowlist is [routes.txt](routes.txt), embedded at build time. It covers public GET resources from the official v3 route index: movies, TV, seasons, episodes, episode groups, people, collections, companies, networks, credits, keywords, reviews, search, discover, find, trending, public lists, change feeds, configuration, certifications, genres, and watch providers. Placeholder IDs are positive decimal integers, except season/episode numbers allow zero and external/credit/review/episode-group identifiers allow letters, digits, underscore, colon, hyphen, and internal periods. Trending windows are `day` or `week`. Paths must match exactly, without trailing slashes, dot segments, or percent-encoded path aliases.

**v4 supports only `GET /4/list/{list_id}`**, for public lists. The relay also checks that a successful list response explicitly contains `"public":true`; otherwise it returns a local 404 without storing or exposing that list. `/4/list/{id}/item_status` requires the owner's user token and is excluded. All v4 account recommendations, favorites, rated items, watchlists, account lists, authentication and access-token workflows are excluded. See [the documentation review](docs/sources.md) for official sources and the reference-project license check.

All account, session, guest-session, authentication, account-state, rating, and mutation routes are rejected. Methods other than GET return 405; browser CORS OPTIONS preflights are handled locally. Unknown/new TMDB routes remain blocked until reviewed and added to the allowlist.

Query names and values are forwarded without rebuilding TMDB's parameter schemas. Names are sorted for cache identity; duplicate-value order is retained because order may affect upstream semantics. Different versions, paths, languages, regions, pages, filters, and other query variants have separate entries. Unknown ordinary query parameters are forwarded for TMDB to interpret. Malformed queries and credential-like parameter names (including API keys, tokens, sessions, authorization, password and username) return 400. `append_to_response` is restricted to suffixes that resolve to another explicitly supported public route; private or unknown appends return 400. Authorization, Proxy-Authorization, X-Api-Key, X-Auth-Token, and Cookie headers are rejected. No caller headers are copied upstream; only the configured token, JSON Accept header and identity encoding are sent.

The upstream is fixed to `https://api.themoviedb.org`; redirects are returned without following or exposing their Location. This is not an arbitrary URL proxy. Successful JSON bytes, status and content type are preserved, with ETag, Last-Modified, Content-Language, Content-Encoding and Retry-After when supplied. Search results and image paths are never modified. Transport errors, timeouts (504), oversized bodies (502), or detected credential echoes (502) produce small generic local errors; their exception to byte preservation prevents secret leakage and unbounded buffering. No cookies, upstream authorization, or hop-by-hop headers are relayed. There are no debug/pprof endpoints, and logs omit tokens, URLs, queries, response bodies and connection errors that could contain secrets.

## Cache semantics

- A fresh Valkey entry is returned directly.
- A missing or expired entry waits for a full TMDB fetch. There is no stale-while-revalidate, background refresh, or stale fallback.
- Only valid JSON with a JSON media type and a 2xx status is eligible. Upstream no-store/private/no-cache directives are honored; shorter max-age/s-maxage values (minus upstream Age) also reduce the configured TTL.
- A successful eligible fetch writes Valkey with a TTL **before returning**. The stored envelope preserves the exact body bytes, status and selected headers. Its absolute freshness deadline also prevents extending freshness during reads.
- Upstream errors, including 429 and Retry-After, are returned without retrying and never cached. A 429 pauses new upstream dispatches for its Retry-After (integer seconds or HTTP date; integer values capped at 30 days), with a 1-second fallback. Existing in-flight calls may still complete. An expired success is never substituted for an error.
- Valkey read failures are treated as misses. Write failures return the successful upstream response with `Cache-Control: no-store`. Corrupt cache entries are treated as misses. Liveness remains healthy, but readiness fails if Valkey cannot answer PING.
- Simultaneous identical requests share a fetch **within one process**, including cache failures and upstream errors. Each instance has its own in-flight group; there is no distributed lock. A disconnected caller stops waiting; the shared fetch continues under a bounded service context for other waiters.
- Cache keys hash the API version, path and canonical query, with a credential-derived namespace to isolate operators sharing a cache. Rotating the token starts a new namespace; old entries expire naturally.
- Downstream cache headers use the floored **remaining** freshness time with `must-revalidate, proxy-revalidate`. Responses with under one second remaining, errors and uncached results use `no-store`. No stale allowance is advertised. Downstream clients must honor HTTP cache directives; the relay cannot control misconfigured intermediaries.

`GET /healthz` checks that the process can serve HTTP. `GET /readyz` checks Valkey with a timeout, without spending a TMDB call. Neither checks the validity of the operator token. Shutdown drains active requests, bounded by upstream/cache timeouts. Server header, request, write and idle timeouts are set. Compose allows 90 seconds for shutdown with default timeouts; adjust its grace period if increasing them substantially.

`GET /config` returns the instance's public settings as JSON. `rate_limit` contains `requests_per_second` and `queue_size` (per process); `cache_ttl_seconds` contains the configured cache durations for `details`, `search`, `lists`, `trending`, and `reference`, in seconds. Actual cache freshness can be shorter due to upstream cache directives or eviction. The endpoint does not call TMDB or Valkey and uses `Cache-Control: no-store`. Credentials and connection settings are never included.

## Verification

```sh
gofmt -w .
go test -count=1 ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Tests use an HTTP mock upstream and miniredis, a Valkey/Redis protocol-compatible fixture, with no real TMDB credential. They cover freshness and expiration, blocking refresh, no stale fallback, simultaneous requests, canonical query/version isolation, route and append validation, rejected credentials/writes, response/error bytes, Retry-After, cache read/write failures, TLS configuration, startup validation, CORS, timeouts, size limits, redirects and private v4 lists. Fixture tests do not replace a real Valkey or Docker smoke test. Real TMDB access requires your token; no test sends requests to TMDB.

## Releases

Release Please runs on pushes to `main` and opens or updates a release PR from Conventional Commits, including the version and CHANGELOG.md. Merge that PR to create the Git tag and GitHub release. The first release defaults to `v1.0.0`; subsequent versions follow commit types (`fix:`, `feat:`, and breaking changes).

The same workflow then checks formatting, runs vet and race-enabled tests, builds binaries for Linux, macOS (`darwin`) and Windows on amd64 and arm64, publishes the container, and uploads binaries, `env.example`, `compose.prod.yaml` and SHA-256 checksums to the release. Publishing is driven directly by Release Please's job outputs in the same workflow: it does not depend on a bot-created tag triggering another workflow.

No custom secrets are needed. GitHub supplies `GITHUB_TOKEN`; the jobs explicitly request release, package and PR permissions. In Settings > Actions > General, enable **Allow GitHub Actions to create and approve pull requests**. Release Please uses that permission to open the PR; it does not automatically approve or merge it. The TMDB token stays in your runtime `.env`. Separate PR-check workflows are not automatically triggered by PRs created using this built-in token; configure a GitHub App or PAT only if you later need that behavior.

Manually pushed version tags remain supported as an alternative. Tags such as `v1.1.0-rc.1` create prereleases. See [Release Please's documentation](https://github.com/googleapis/release-please-action) for the PR/release lifecycle and built-in token behavior.

Use a new version tag for each release. The workflow does not overwrite an existing GitHub release. Release binaries still need a reachable Valkey instance and your private TMDB token. Copy the downloaded `env.example` to `.env`, configure them, and run the binary. Never upload your local `.env` as a release asset.

The same workflow publishes a multi-platform container to **GitHub Container Registry (GHCR)** for Linux amd64 and arm64:

```sh
docker pull ghcr.io/owner/repository:1.0.0
```

Replace `owner/repository` with your lowercase GitHub repository path. Git tags retain the `v` prefix; image tags omit it. A stable release such as `v1.2.3` publishes the same image as `:1.2.3`, `:1.2`, `:1`, and `:latest`. Major and minor aliases move on each stable publication: `:1` follows releases within major 1, and `:1.2` follows patches within 1.2. Publish stable versions in increasing order to keep these aliases moving forward. Prereleases such as `v1.3.0-rc.1` publish only `:1.3.0-rc.1`. Pulling an alias gets its current image; existing running containers are not updated automatically.

The workflow uses the built-in GitHub token with `packages: write`; no Docker Hub account or registry secret is needed. For anonymous pulls, make the package public in its GitHub package settings; private packages require registry authentication. Credentials are supplied at runtime, never built into the image.

Use `compose.prod.yaml` to run the published image with the bundled Valkey, as described in the production setup above. Set `RELAY_IMAGE` to `ghcr.io/owner/repository:1.2` to follow a minor version or `ghcr.io/owner/repository:1.2.3` to pin an exact release.

## Attribution and limitations

Use an approved TMDB logo in the consuming application's About/Credits area and include TMDB's required endorsement disclaimer. See [TMDB's official attribution and licensing guidance](https://developer.themoviedb.org/docs/faq) for the current exact wording and approved logos. Commercial use may require a separate agreement. This relay does not grant redistribution rights or bypass TMDB's limits.

Cache hits bypass the upstream limiter; identical simultaneous misses share one queue slot. A cache miss dispatches immediately when rate capacity is available. Only misses exceeding the paced rate queue before the separate upstream timeout begins; there is no separate queue timeout. Queue overflow returns 429 with Retry-After estimating when the current queue clears (at least 1 second); new traffic can extend that wait. During a TMDB cooldown, misses receive 429 immediately, and already queued misses are released with 429 too. Retry-After reflects the remaining TMDB cooldown (HTTP dates are preserved), without adding queue time. Queue order is not guaranteed. TMDB [documents a changeable ceiling around 40 requests/second](https://developer.themoviedb.org/docs/rate-limiting), without endpoint-specific budgets; the default is 40; lower it if TMDB throttles your traffic. The limiter and cooldown are per process, including during cache outages. Divide the configured rate across replicas or other applications sharing the token; restarts reset the budget and cooldown.

There are no client quotas or live upstream retries; TMDB can throttle the shared operator token. Every distinct miss consumes upstream access, and bodies are buffered in memory up to the configured limit per distinct in-flight request. This service does not impose a total concurrency or aggregate memory bound. Use deployment resource limits appropriate to expected traffic. Private and personalized endpoints are intentionally unavailable. Changes to public lists are visible after their TTL, unless the entry is evicted earlier. New TMDB endpoints require a reviewed allowlist update.
