# Sources reviewed on 2026-10-30

## Reference project and license

Reference: https://github.com/tm1981/tmdb-caching-api

Reviewed its repository listing and README to understand the cached movie/TV API concept. It is a Next.js service using SQL/Prisma with an admin dashboard, authentication and lazy synchronization. Those implementation choices are not carried into tmdb-relay.

The repository listing showed no LICENSE file or declared license, and GitHub's repository metadata `license` field was null at review time. No license permission to copy source was established. No source code was reused, translated, or vendored. This implementation was written independently in Go. Public endpoint names in routes.txt come from TMDB's official API contract, not the reference implementation.

## Official TMDB contract

- v3 reference: https://developer.themoviedb.org/reference/intro/getting-started
- v3 OpenAPI route index: https://developer.themoviedb.org/openapi/tmdb-api.json
- v4 public list details: https://developer.themoviedb.org/v4/reference/list-details
- v4 list item status: https://developer.themoviedb.org/v4/reference/list-item-status
- v4 account recommendations: https://developer.themoviedb.org/v4/reference/account-movie-recommendations
- v3 list item status: https://developer.themoviedb.org/reference/list-check-item-status
- v3 public movie lists: https://developer.themoviedb.org/reference/movie-lists
- Attribution and terms: https://developer.themoviedb.org/docs/faq

The v4 details documentation explicitly says private lists require the owner's user access token. Its item-status documentation explicitly requires ownership and a user access token, so item status is excluded even though it is a GET. Account recommendations are personalized and excluded. Only public v4 list details qualify for this relay. The v3 item-status route is documented separately as a public list read without a user-token requirement and is included.

routes.txt is a reviewed static snapshot of public GET route templates. Account/authentication/session/account-state/rating endpoints were removed. This is not an automatic import of future API additions. Route parameter schemas, request/response models, and examples have not been copied into the implementation.

## Valkey client

Maintained client: https://github.com/valkey-io/valkey-go

Consulted its current connection URL, DisableCache, timeout and command documentation. The service uses direct GET/SET with Valkey TTLs, not the client's local response cache.
