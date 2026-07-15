# Kong gRPC Demo Hardening Design

- **Status:** Approved
- **Date:** 2026-07-15
- **Repository:** `davidgrldo/kong-go-grpc-example`
- **Branch:** `codex/harden-multitenant-demo`

## Context

The project demonstrates a Go gRPC inventory service behind Kong with one native gRPC route and one REST-to-gRPC route. The current demo works, but it treats caller-supplied `X-Company-Id` metadata as tenant identity, streams fabricated cross-tenant data, exposes the Kong Admin API, starts without readiness guarantees, and has no automated tests.

This change keeps the project small and educational while making the demo's security and tenant-isolation claims internally consistent.

## Goals

- Authenticate both native gRPC and REST callers with Kong Key Auth.
- Derive tenant identity only from Kong-injected consumer metadata.
- Return and stream only the authenticated tenant's stored inventory.
- Make streaming cancellation-aware and remove the final unnecessary delay.
- Restrict the local Compose stack to the intended proxy surface.
- Use supported runtime branches and deterministic startup checks.
- Add focused tests and a protobuf drift check without new application dependencies.
- Correct the browser behavior and README examples to match the running system.

## Non-goals

- Production credential storage, key rotation, JWT/OIDC, mTLS, or TLS termination.
- Persistent inventory storage, mutation APIs, event delivery, or multi-node state.
- Exposing the inventory container directly outside the Compose network.
- Changing protobuf request or response fields.
- Building a general authentication or configuration framework.

The committed API keys are intentionally demo credentials. The README must state that they are public, local-only examples.

## Architecture and Trust Boundary

The request path will be:

1. A REST or native gRPC client sends an `apikey` header to Kong.
2. A service-scoped Key Auth plugin validates the credential for both routes.
3. Kong overwrites the upstream `X-Consumer-Username` header with the matched Consumer username.
4. The Go service accepts exactly one non-empty `x-consumer-username` metadata value and uses it as the company ID.
5. The store resolves inventory only inside that company map.

The two Consumers are:

| Consumer username | Demo API key |
| --- | --- |
| `company-a` | `company-a-demo-key` |
| `company-b` | `company-b-demo-key` |

`X-Company-Id` is removed from the contract and has no effect. Direct access to port `50051` remains untrusted and therefore must stay unpublished. A spoofing acceptance test must prove that a valid company A key plus caller-supplied `X-Consumer-Username: company-b` still reaches the service as `company-a`.

## Kong Configuration

`kong.yml` will add the two Consumers and a service-scoped `key-auth` plugin with:

- `key_names: [apikey]`
- header lookup enabled
- query-string and body lookup disabled
- `hide_credentials: true`
- `run_on_preflight: false`

The existing rate limiter remains service-scoped and will therefore operate on authenticated Consumers. The gRPC-Gateway plugin remains scoped only to the REST route.

The REST-route CORS configuration will:

- allow `Accept`, `Content-Type`, and `apikey` request headers;
- expose `grpc-status` and `grpc-message` response headers;
- handle preflight locally without forwarding it upstream.

Missing or invalid API keys are rejected by Kong before the Go service runs.

## Go Service Behavior

### Tenant resolution

`companyIDFromContext` will read only `x-consumer-username`. Missing, empty, or duplicate values return `codes.Unauthenticated`. An authenticated username with no company entry returns `codes.PermissionDenied`.

The service will never fall back to `x-company-id`.

### Store access

The store will distinguish these cases:

- company exists and SKU exists;
- company exists but SKU is absent;
- company does not exist.

For streaming, the store returns a copied snapshot sorted lexicographically by SKU. The lock is released before any message is sent or timer is started.

### Unary lookup

`GetStock` processes requests in this order:

1. resolve authenticated company;
2. reject an empty SKU with `codes.InvalidArgument`;
3. reject an unprovisioned company with `codes.PermissionDenied`;
4. reject an absent SKU for a known company with `codes.NotFound`;
5. return the stored quantity and warehouse.

### Streaming

`StreamStockUpdates` takes one company snapshot and sends each stored SKU with its actual quantity. Company A receives `SKU-001=42` and `SKU-002=7`; company B receives `SKU-001=100` and `SKU-777=3`.

Items are sent in sorted order. `updated_at_unix` is created immediately before each send. A 500 ms context-selectable timer runs only when another item remains. Cancellation and deadline errors are converted to their matching gRPC status codes, and `stream.Send` errors are returned unchanged.

The protobuf wire shape stays unchanged. Comments in `inventory.proto` are updated to describe Kong Consumer metadata, then generated files are refreshed.

## Error Contract

| Condition | Result |
| --- | --- |
| Missing or invalid API key at Kong | unauthenticated request rejected before upstream |
| Missing, empty, or ambiguous consumer metadata | `Unauthenticated` |
| Authenticated consumer without a company map | `PermissionDenied` |
| Empty SKU | `InvalidArgument` |
| Missing SKU for a known company | `NotFound` |
| Stream canceled | `Canceled` |
| Stream deadline exceeded | `DeadlineExceeded` |
| Stream send failure | original send error |

## Compose and Runtime

The Kong image changes to `kong/kong-gateway:3.14.0.8-ubuntu`, the current 3.14 LTS patch selected for this design. Factual erratum: starting with Kong Gateway 3.10, Enterprise Free Mode is unavailable; license-free startup follows expired-license behavior. This is an explicit tradeoff: the newest Apache-licensed `kong` image is already outside full support, while the actively supported LTS is distributed through `kong/kong-gateway`.

The Go builder changes to `golang:1.25-alpine3.24`, and the final runtime changes to `alpine:3.24`.

Kong will listen only on plaintext port `8000` for this documented local demo through `KONG_PROXY_LISTEN="0.0.0.0:8000 http2"`. Compose publishes it as `127.0.0.1:8000:8000`. The unused TLS proxy port and the Admin API port are removed. `KONG_ADMIN_LISTEN` is set to `off`.

Readiness uses:

- an inventory TCP healthcheck using `nc -w 1 127.0.0.1 50051 </dev/null` from Alpine BusyBox;
- long-form `depends_on` with `condition: service_healthy`;
- Kong's unexported Status API on `127.0.0.1:8100`;
- the image's bundled `resty` runtime and `resty.http` client to require HTTP 200 from `http://127.0.0.1:8100/status/ready` as the Kong healthcheck;
- `docker compose up --build -d --wait` in `make up`.

`make reload` validates and gracefully reloads the bind-mounted DB-less configuration through `docker compose exec kong kong reload`. Recreating the whole stack for every `kong.yml` edit is unnecessary.

## Browser Client

The browser form replaces Company ID with a demo API key and sends it through the `apikey` header. The default key is `company-a-demo-key`.

For non-2xx transcoded responses, the page displays the response body when present and falls back to `grpc-message` when the body is empty. It does not decode the header as a URI component.

The explanatory copy will describe an HTTP GET request rather than a JSON request body.

## Protobuf and Tooling

The vendored `third_party/google/api` files remain the source of the HTTP annotations. Network downloads from mutable `googleapis/master` are removed.

The generator versions remain aligned with the checked-in headers:

- `protoc-gen-go v1.36.11`
- `protoc-gen-go-grpc v1.6.2`

They are tracked as Go tool dependencies and invoked by `make proto`. `protoc 35.1` remains an explicit external prerequisite, and `make proto` fails fast when the executable or version does not match. This avoids introducing a compiler download framework.

`make proto-check` runs generation and then `git diff --exit-code -- proto/gen` so stale generated code fails visibly.

## Tests

Tests stay in package `main` and call server methods directly. A small recording stream embeds `grpc.ServerStream` and implements only `Context` and typed `Send`. Go's standard-library `testing/synctest` virtualizes the 500 ms timer; no sleep injection, network listener, or additional test dependency is needed.

Required cases:

- missing, empty, and duplicate consumer usernames are unauthenticated;
- `x-company-id` cannot change the authenticated company;
- unknown authenticated company is permission denied;
- empty and absent SKUs use the documented codes;
- snapshots are copied and sorted;
- each company streams only its actual inventory;
- recorded send times advance by exactly 500 ms, each payload timestamp matches the Unix second of its send, and there is no delay after the final send;
- cancellation and deadlines stop the interval promptly;
- send errors propagate unchanged.

Runtime acceptance checks cover both protocols:

- missing and invalid keys fail;
- company A and company B keys return 42 and 100 for `SKU-001`;
- spoofed consumer headers cannot override the authenticated Consumer;
- REST preflight allows `apikey`;
- browser error text is visible;
- port `8001` is not listening or published;
- Compose reaches healthy state through `--wait`.

## Docker Build Context

A small `.dockerignore` excludes files that are not required to compile the Go binary:

```text
.git
.env
.env.*
*.log
coverage.out
README.md
docs/
docker-compose.yml
kong.yml
index.html
Makefile
inventory.proto
third_party/
```

The Dockerfile continues to copy the module files first for dependency caching and copies only the resulting binary into the final image.

## Documentation

The README will be updated to cover:

- Key Auth and the two demo credentials;
- Consumer-derived tenant identity;
- current image and tool versions;
- `make up`, `make reload`, and readiness behavior;
- disabled Admin API and loopback-only proxy;
- corrected native gRPC and REST examples;
- two tenant-specific streaming results;
- quoted ProtoJSON representation for `int64` values;
- port 8000 serving both HTTP/1.1 REST and h2c gRPC;
- explicit local-demo limitations and threat-model-based TLS/mTLS guidance.

## Files Expected to Change

- `main.go`
- `main_test.go` (new)
- `inventory.proto`
- `proto/gen/inventory.pb.go` if regeneration changes descriptors
- `proto/gen/inventory_grpc.pb.go` if regeneration changes comments or headers
- `kong.yml`
- `docker-compose.yml`
- `Dockerfile`
- `.dockerignore` (new)
- `Makefile`
- `go.mod`
- `go.sum`
- `index.html`
- `README.md`

## Verification Gate

Implementation is complete only when all applicable checks pass:

```text
go test -race ./...
go vet ./...
go build ./...
go mod verify
go mod tidy -diff
make proto-check
docker compose config -q
kong config parse against the selected image
live REST and native gRPC smoke checks
git diff --check
```

Temporary containers and networks must be removed, and only intended files may remain changed.

## Delivery

Implementation remains on `codex/harden-multitenant-demo` in the fork. After local review, checks, and an intentional commit, the branch is pushed to `davidgrldo/kong-go-grpc-example`. The eventual PR is opened as a draft from that branch to `revell29/kong-go-grpc-example:master`.

## References

- [Kong Key Auth](https://developer.konghq.com/plugins/key-auth/)
- [Kong CORS configuration](https://developer.konghq.com/plugins/cors/reference/)
- [Kong Gateway support policy](https://developer.konghq.com/gateway/version-support-policy/)
- [Kong Gateway changelog](https://developer.konghq.com/gateway/changelog/)
- [Kong readiness probes](https://developer.konghq.com/gateway/traffic-control/health-check-probes/)
- [Docker Compose startup ordering](https://docs.docker.com/compose/how-tos/startup-order/)
- [Alpine release branches](https://www.alpinelinux.org/releases/)
- [ProtoJSON format](https://protobuf.dev/programming-guides/json/)
- [Go tool dependencies](https://go.dev/doc/modules/managing-dependencies)
