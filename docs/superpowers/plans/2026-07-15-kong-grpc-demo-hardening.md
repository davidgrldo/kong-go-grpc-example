# Kong gRPC Demo Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Authenticate both Kong entry points, derive tenant identity from the authenticated Consumer, stream only that tenant's stored inventory, and ship a reproducible, test-covered local demo from the fork.

**Architecture:** Kong validates the `apikey` header at service scope, overwrites `X-Consumer-Username`, and proxies native gRPC or REST-transcoded requests to an unpublished Go service. The Go service trusts exactly one injected consumer username, resolves a copied per-company snapshot, and returns precise gRPC status codes. Docker Compose exposes only the loopback proxy and gates startup on inventory and Kong readiness.

**Tech Stack:** Go 1.25 module semantics (executed with Go 1.26-compatible tooling), gRPC-Go, Protocol Buffers 35.1, Kong Gateway 3.14 LTS in DB-less mode, Docker Compose, standard-library `testing` and `testing/synctest`, HTML and browser `fetch`.

## Global Constraints

- Work only on `codex/harden-multitenant-demo` in `davidgrldo/kong-go-grpc-example`; preserve `upstream` as `revell29/kong-go-grpc-example`.
- Keep the protobuf wire shape unchanged. Tenant identity stays out of request messages.
- Trust only `x-consumer-username`; never fall back to `x-company-id`.
- Keep port `50051`, Kong Status port `8100`, TLS port `8443`, and Admin port `8001` unpublished.
- Keep committed API keys explicitly demo-only. Do not introduce production credential management.
- Add no application or test dependency for timing; use `testing/synctest`.
- Follow red-green-refactor for Go behavior. Run focused tests before the full gate.
- Use `apply_patch` for tracked file edits, inspect every generated diff, and preserve unrelated user changes.
- Do not push or create the draft PR until every local verification step and review pass is complete.

---

## Task 1: Enforce authenticated tenant identity in unary lookups

**Files:**

- Create: `main_test.go`
- Modify: `main.go:1-114`

**Interfaces:**

- `companyIDFromContext(context.Context) (string, error)` accepts exactly one non-empty `x-consumer-username` value.
- `(*stockStore).get(company, sku string) (quantity int32, companyFound bool, skuFound bool)` distinguishes an unprovisioned Consumer from an absent SKU.
- `(*stockStore).snapshot(company string) ([]stock, bool)` returns a sorted independent copy.
- `(*inventoryServer).GetStock` applies identity, request, company, and SKU validation in that order.

- [ ] **Step 1: Add focused failing tests for identity, store semantics, and unary lookup**

Create `main_test.go` with this initial content:

```go
package main

import (
	"context"
	"reflect"
	"testing"

	pb "example.com/kong-go-grpc/proto/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func incomingContext(base context.Context, pairs ...string) context.Context {
	return metadata.NewIncomingContext(base, metadata.Pairs(pairs...))
}

func companyContext(base context.Context, company string) context.Context {
	return incomingContext(base, "x-consumer-username", company)
}

func TestCompanyIDFromContextRejectsInvalidMetadata(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
	}{
		{name: "missing metadata", ctx: context.Background()},
		{name: "empty username", ctx: incomingContext(context.Background(), "x-consumer-username", "")},
		{name: "legacy company header only", ctx: incomingContext(context.Background(), "x-company-id", "company-a")},
		{name: "duplicate username", ctx: incomingContext(context.Background(),
			"x-consumer-username", "company-a",
			"x-consumer-username", "company-b",
		)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := companyIDFromContext(tt.ctx)
			if got, want := status.Code(err), codes.Unauthenticated; got != want {
				t.Fatalf("status.Code(companyIDFromContext()) = %v, want %v; err = %v", got, want, err)
			}
		})
	}
}

func TestCompanyIDFromContextAcceptsOneUsername(t *testing.T) {
	got, err := companyIDFromContext(companyContext(context.Background(), "company-a"))
	if err != nil {
		t.Fatalf("companyIDFromContext() error = %v", err)
	}
	if want := "company-a"; got != want {
		t.Fatalf("companyIDFromContext() = %q, want %q", got, want)
	}
}

func TestStockStoreSnapshotIsSortedAndIndependent(t *testing.T) {
	store := newStockStore()

	got, ok := store.snapshot("company-a")
	if !ok {
		t.Fatal("snapshot(company-a) reported an unknown company")
	}
	want := []stock{
		{sku: "SKU-001", quantity: 42},
		{sku: "SKU-002", quantity: 7},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot(company-a) = %#v, want %#v", got, want)
	}

	got[0].quantity = 999
	quantity, companyFound, skuFound := store.get("company-a", "SKU-001")
	if !companyFound || !skuFound || quantity != 42 {
		t.Fatalf("mutating snapshot changed store: get() = (%d, %t, %t)", quantity, companyFound, skuFound)
	}

	if snapshot, found := store.snapshot("company-unknown"); found || snapshot != nil {
		t.Fatalf("snapshot(unknown) = (%#v, %t), want (nil, false)", snapshot, found)
	}
}

func TestGetStockUsesAuthenticatedCompany(t *testing.T) {
	server := &inventoryServer{store: newStockStore()}
	ctx := incomingContext(context.Background(),
		"x-consumer-username", "company-a",
		"x-company-id", "company-b",
	)

	response, err := server.GetStock(ctx, &pb.GetStockRequest{Sku: "SKU-001"})
	if err != nil {
		t.Fatalf("GetStock() error = %v", err)
	}
	if got, want := response.GetQuantity(), int32(42); got != want {
		t.Fatalf("GetStock() quantity = %d, want %d", got, want)
	}
}

func TestGetStockChecksIdentityBeforeSKU(t *testing.T) {
	server := &inventoryServer{store: newStockStore()}
	_, err := server.GetStock(context.Background(), &pb.GetStockRequest{})
	if got, want := status.Code(err), codes.Unauthenticated; got != want {
		t.Fatalf("GetStock() code = %v, want %v; err = %v", got, want, err)
	}
}

func TestGetStockReturnsDocumentedErrors(t *testing.T) {
	server := &inventoryServer{store: newStockStore()}
	tests := []struct {
		name    string
		company string
		sku     string
		want    codes.Code
	}{
		{name: "unknown company", company: "company-unknown", sku: "SKU-001", want: codes.PermissionDenied},
		{name: "empty SKU", company: "company-a", sku: "", want: codes.InvalidArgument},
		{name: "empty SKU precedes unknown company", company: "company-unknown", sku: "", want: codes.InvalidArgument},
		{name: "missing SKU", company: "company-a", sku: "SKU-404", want: codes.NotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := server.GetStock(companyContext(context.Background(), tt.company), &pb.GetStockRequest{Sku: tt.sku})
			if got := status.Code(err); got != tt.want {
				t.Fatalf("GetStock() code = %v, want %v; err = %v", got, tt.want, err)
			}
		})
	}
}
```

- [ ] **Step 2: Run the focused tests and confirm the red state**

Run:

```bash
go test ./... -run 'Test(CompanyID|StockStore|GetStock)'
```

Expected: compilation fails because `stock`, `snapshot`, and the three-result `get` contract do not exist yet. A failure caused by an unrelated package or environment issue must be resolved before implementation.

- [ ] **Step 3: Implement tenant resolution and three-state store access**

In `main.go`, add `sort` to imports, define the snapshot value, and replace the store access methods with:

```go
type stock struct {
	sku      string
	quantity int32
}

func (s *stockStore) get(company, sku string) (quantity int32, companyFound, skuFound bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	companyStock, companyFound := s.data[company]
	if !companyFound {
		return 0, false, false
	}
	quantity, skuFound = companyStock[sku]
	return quantity, true, skuFound
}

func (s *stockStore) snapshot(company string) ([]stock, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	companyStock, ok := s.data[company]
	if !ok {
		return nil, false
	}

	result := make([]stock, 0, len(companyStock))
	for sku, quantity := range companyStock {
		result = append(result, stock{sku: sku, quantity: quantity})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].sku < result[j].sku
	})
	return result, true
}
```

Replace `companyIDFromContext` with:

```go
func companyIDFromContext(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "authenticated consumer metadata is required")
	}
	values := md.Get("x-consumer-username")
	if len(values) != 1 || values[0] == "" {
		return "", status.Error(codes.Unauthenticated, "exactly one x-consumer-username value is required")
	}
	return values[0], nil
}
```

Replace the package comment with:

```go
// Command server implements a small multi-tenant gRPC InventoryService.
// It sits behind Kong, which authenticates callers and injects the matched
// Consumer username for tenant resolution before proxying plaintext HTTP/2
// to this unpublished service on port 50051.
package main
```

Replace the metadata-key loop in `logIncomingMetadata` with:

```go
	for _, key := range []string{
		":authority",
		"x-forwarded-host",
		"x-forwarded-proto",
		"x-consumer-id",
		"x-consumer-username",
	} {
		if values := md.Get(key); len(values) > 0 {
			log.Printf("[%s] metadata %s = %v", rpcName, key, values)
		}
	}
```

- [ ] **Step 4: Implement the unary error order**

Replace the lookup block in `GetStock` with:

```go
	if req.GetSku() == "" {
		return nil, status.Error(codes.InvalidArgument, "sku is required")
	}

	quantity, companyFound, skuFound := s.store.get(companyID, req.GetSku())
	if !companyFound {
		return nil, status.Errorf(codes.PermissionDenied, "company %q is not provisioned", companyID)
	}
	if !skuFound {
		return nil, status.Errorf(codes.NotFound, "sku %q not found for company %q", req.GetSku(), companyID)
	}

	return &pb.GetStockResponse{
		Sku:       req.GetSku(),
		Quantity:  quantity,
		Warehouse: "WH-JKT-01",
	}, nil
```

Keep identity resolution before the empty-SKU check.

- [ ] **Step 5: Format and prove the unary behavior is green**

Run:

```bash
gofmt -w main.go main_test.go
go test ./... -run 'Test(CompanyID|StockStore|GetStock)'
go test ./...
```

Expected: all focused tests and the full package test pass.

- [ ] **Step 6: Commit the unary tenant boundary**

Run:

```bash
git add main.go main_test.go
git diff --cached --check
git commit -m "feat: enforce authenticated tenant inventory"
```

Expected: one commit containing only `main.go` and `main_test.go`.

---

## Task 2: Stream sorted tenant snapshots with cancellation-aware timing

**Files:**

- Modify: `main_test.go`
- Modify: `main.go:116-150`

**Interfaces:**

- `streamInterval` is `500 * time.Millisecond`.
- `StreamStockUpdates` snapshots once, returns `PermissionDenied` for an unknown company, and sends actual sorted inventory.
- Cancellation and deadline errors are converted with `status.FromContextError`; send errors are returned unchanged.

- [ ] **Step 1: Extend the test imports and add a recording stream**

Change the `main_test.go` import block to:

```go
import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	pb "example.com/kong-go-grpc/proto/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)
```

Append these test helpers:

```go
type recordedUpdate struct {
	sku           string
	quantity      int32
	updatedAtUnix int64
	sentAt        time.Time
}

type recordingStockStream struct {
	grpc.ServerStream
	ctx     context.Context
	sent    []recordedUpdate
	sendErr error
	failAt  int
	onSend  func(int)
}

func (s *recordingStockStream) Context() context.Context {
	return s.ctx
}

func (s *recordingStockStream) Send(update *pb.StockUpdate) error {
	if s.sendErr != nil && len(s.sent) == s.failAt {
		return s.sendErr
	}
	s.sent = append(s.sent, recordedUpdate{
		sku:           update.GetSku(),
		quantity:      update.GetQuantity(),
		updatedAtUnix: update.GetUpdatedAtUnix(),
		sentAt:        time.Now(),
	})
	if s.onSend != nil {
		s.onSend(len(s.sent))
	}
	return nil
}
```

- [ ] **Step 2: Add failing streaming behavior tests**

Append:

```go
func TestStreamStockUpdatesUsesTenantSnapshotAndExactTiming(t *testing.T) {
	tests := []struct {
		name    string
		company string
		want    []stock
	}{
		{name: "company A", company: "company-a", want: []stock{
			{sku: "SKU-001", quantity: 42},
			{sku: "SKU-002", quantity: 7},
		}},
		{name: "company B", company: "company-b", want: []stock{
			{sku: "SKU-001", quantity: 100},
			{sku: "SKU-777", quantity: 3},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				server := &inventoryServer{store: newStockStore()}
				stream := &recordingStockStream{ctx: companyContext(t.Context(), tt.company)}
				startedAt := time.Now()

				err := server.StreamStockUpdates(&pb.StreamStockRequest{}, stream)
				if err != nil {
					t.Fatalf("StreamStockUpdates() error = %v", err)
				}
				if got, want := time.Since(startedAt), streamInterval; got != want {
					t.Fatalf("stream duration = %v, want %v", got, want)
				}
				if got, want := len(stream.sent), len(tt.want); got != want {
					t.Fatalf("sent message count = %d, want %d", got, want)
				}
				for i, want := range tt.want {
					got := stream.sent[i]
					if got.sku != want.sku || got.quantity != want.quantity {
						t.Errorf("message %d = (%q, %d), want (%q, %d)", i, got.sku, got.quantity, want.sku, want.quantity)
					}
					if got.updatedAtUnix != got.sentAt.Unix() {
						t.Errorf("message %d timestamp = %d, send Unix second = %d", i, got.updatedAtUnix, got.sentAt.Unix())
					}
				}
				if got, want := stream.sent[1].sentAt.Sub(stream.sent[0].sentAt), streamInterval; got != want {
					t.Fatalf("send interval = %v, want %v", got, want)
				}
			})
		})
	}
}

func TestStreamStockUpdatesRejectsInvalidIdentity(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
	}{
		{name: "missing metadata", ctx: context.Background()},
		{name: "empty username", ctx: incomingContext(context.Background(), "x-consumer-username", "")},
		{name: "duplicate username", ctx: incomingContext(context.Background(),
			"x-consumer-username", "company-a",
			"x-consumer-username", "company-b",
		)},
	}

	server := &inventoryServer{store: newStockStore()}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := &recordingStockStream{ctx: tt.ctx}
			err := server.StreamStockUpdates(&pb.StreamStockRequest{}, stream)
			if got, want := status.Code(err), codes.Unauthenticated; got != want {
				t.Fatalf("StreamStockUpdates() code = %v, want %v; err = %v", got, want, err)
			}
			if len(stream.sent) != 0 {
				t.Fatalf("invalid identity received %d messages", len(stream.sent))
			}
		})
	}
}

func TestStreamStockUpdatesRejectsUnknownCompany(t *testing.T) {
	server := &inventoryServer{store: newStockStore()}
	stream := &recordingStockStream{ctx: companyContext(context.Background(), "company-unknown")}

	err := server.StreamStockUpdates(&pb.StreamStockRequest{}, stream)
	if got, want := status.Code(err), codes.PermissionDenied; got != want {
		t.Fatalf("StreamStockUpdates() code = %v, want %v; err = %v", got, want, err)
	}
	if len(stream.sent) != 0 {
		t.Fatalf("unknown company received %d messages", len(stream.sent))
	}
}

func TestStreamStockUpdatesRejectsFinishedContextBeforeSend(t *testing.T) {
	canceled, cancel := context.WithCancel(companyContext(context.Background(), "company-a"))
	cancel()
	expired, cancelDeadline := context.WithDeadline(
		companyContext(context.Background(), "company-a"),
		time.Unix(0, 0),
	)
	defer cancelDeadline()

	tests := []struct {
		name string
		ctx  context.Context
		want codes.Code
	}{
		{name: "canceled", ctx: canceled, want: codes.Canceled},
		{name: "deadline exceeded", ctx: expired, want: codes.DeadlineExceeded},
	}
	server := &inventoryServer{store: newStockStore()}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := &recordingStockStream{ctx: tt.ctx}
			err := server.StreamStockUpdates(&pb.StreamStockRequest{}, stream)
			if got := status.Code(err); got != tt.want {
				t.Fatalf("StreamStockUpdates() code = %v, want %v; err = %v", got, tt.want, err)
			}
			if len(stream.sent) != 0 {
				t.Fatalf("finished context received %d messages", len(stream.sent))
			}
		})
	}
}

func TestStreamStockUpdatesStopsOnCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := &inventoryServer{store: newStockStore()}
		ctx, cancel := context.WithCancel(companyContext(t.Context(), "company-a"))
		stream := &recordingStockStream{ctx: ctx}
		stream.onSend = func(count int) {
			if count == 1 {
				cancel()
			}
		}
		startedAt := time.Now()

		err := server.StreamStockUpdates(&pb.StreamStockRequest{}, stream)
		if got, want := status.Code(err), codes.Canceled; got != want {
			t.Fatalf("StreamStockUpdates() code = %v, want %v; err = %v", got, want, err)
		}
		if got, want := len(stream.sent), 1; got != want {
			t.Fatalf("sent message count = %d, want %d", got, want)
		}
		if got := time.Since(startedAt); got != 0 {
			t.Fatalf("canceled stream duration = %v, want 0", got)
		}
	})
}

func TestStreamStockUpdatesStopsOnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := &inventoryServer{store: newStockStore()}
		ctx, cancel := context.WithTimeout(companyContext(t.Context(), "company-a"), 250*time.Millisecond)
		defer cancel()
		stream := &recordingStockStream{ctx: ctx}
		startedAt := time.Now()

		err := server.StreamStockUpdates(&pb.StreamStockRequest{}, stream)
		if got, want := status.Code(err), codes.DeadlineExceeded; got != want {
			t.Fatalf("StreamStockUpdates() code = %v, want %v; err = %v", got, want, err)
		}
		if got, want := len(stream.sent), 1; got != want {
			t.Fatalf("sent message count = %d, want %d", got, want)
		}
		if got, want := time.Since(startedAt), 250*time.Millisecond; got != want {
			t.Fatalf("deadline stream duration = %v, want %v", got, want)
		}
	})
}

func TestStreamStockUpdatesReturnsSendErrorUnchanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := &inventoryServer{store: newStockStore()}
		wantErr := errors.New("send failed")
		stream := &recordingStockStream{
			ctx:     companyContext(t.Context(), "company-a"),
			sendErr: wantErr,
			failAt:  0,
		}
		startedAt := time.Now()

		err := server.StreamStockUpdates(&pb.StreamStockRequest{}, stream)
		if err != wantErr {
			t.Fatalf("StreamStockUpdates() error = %v, want %v", err, wantErr)
		}
		if got := time.Since(startedAt); got != 0 {
			t.Fatalf("failed stream duration = %v, want 0", got)
		}
	})
}
```

- [ ] **Step 3: Run the streaming tests and confirm the red state**

Run:

```bash
go test ./... -run TestStreamStockUpdates
```

Expected: failures show fabricated cross-tenant data, an unnecessary final delay, raw context errors, or the missing `streamInterval` constant.

- [ ] **Step 4: Implement one-snapshot streaming**

Add this constant near `listenAddr`:

```go
const (
	listenAddr     = ":50051"
	streamInterval = 500 * time.Millisecond
)
```

Replace `StreamStockUpdates` with:

```go
func (s *inventoryServer) StreamStockUpdates(_ *pb.StreamStockRequest, stream pb.InventoryService_StreamStockUpdatesServer) error {
	ctx := stream.Context()
	logIncomingMetadata(ctx, "StreamStockUpdates")

	companyID, err := companyIDFromContext(ctx)
	if err != nil {
		return err
	}
	items, companyFound := s.store.snapshot(companyID)
	if !companyFound {
		return status.Errorf(codes.PermissionDenied, "company %q is not provisioned", companyID)
	}
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}

	for i, item := range items {
		if err := ctx.Err(); err != nil {
			return status.FromContextError(err).Err()
		}
		update := &pb.StockUpdate{
			Sku:           item.sku,
			Quantity:      item.quantity,
			UpdatedAtUnix: time.Now().Unix(),
		}
		if err := stream.Send(update); err != nil {
			return err
		}
		if i == len(items)-1 {
			continue
		}

		timer := time.NewTimer(streamInterval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return status.FromContextError(ctx.Err()).Err()
		}
	}
	return nil
}
```

- [ ] **Step 5: Format and run race-aware streaming verification**

Run:

```bash
gofmt -w main.go main_test.go
go test ./... -run TestStreamStockUpdates
go test -race ./...
go vet ./...
```

Expected: all commands exit zero; virtual-time tests complete without a real 500 ms wait.

- [ ] **Step 6: Commit streaming isolation and timing**

Run:

```bash
git add main.go main_test.go
git diff --cached --check
git commit -m "feat: stream tenant inventory snapshots"
```

Expected: one focused streaming commit.

---

## Task 3: Pin protobuf generators and make generated-code drift visible

**Files:**

- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `Makefile`
- Modify: `inventory.proto`
- Regenerate and inspect: `proto/gen/inventory.pb.go`
- Regenerate and inspect: `proto/gen/inventory_grpc.pb.go`

**Interfaces:**

- Go tool dependencies resolve `protoc-gen-go` at `v1.36.11` and `protoc-gen-go-grpc` at `v1.6.2`.
- `make proto` requires exactly `libprotoc 35.1`, `protoc-gen-go v1.36.11`, and `protoc-gen-go-grpc 1.6.2`, performs no network download, and invokes module-pinned generators.
- `make proto-check` regenerates and fails on an unstaged diff under `proto/gen`.

- [ ] **Step 1: Record generator binaries as module tools**

Run:

```bash
go get -tool google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
go get -tool google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
go mod tidy
```

Confirm `go.mod` contains this tool block and retains `go 1.25.3`:

```go
tool (
	google.golang.org/grpc/cmd/protoc-gen-go-grpc
	google.golang.org/protobuf/cmd/protoc-gen-go
)
```

Run:

```bash
go tool -n protoc-gen-go
go tool -n protoc-gen-go-grpc
```

Expected: each command prints one executable path from Go's build cache and exits zero.

- [ ] **Step 2: Update only protobuf comments, not fields or RPC signatures**

Replace the service comments in `inventory.proto` with:

```proto
service InventoryService {
  // Unary call: look up stock for one SKU.
  // Kong Key Auth injects the authenticated Consumer username as
  // x-consumer-username metadata; tenant identity is not part of the request.
  // The google.api.http option exposes this as GET /v1/stock/{sku}.
  rpc GetStock(GetStockRequest) returns (GetStockResponse) {
    option (google.api.http) = {
      get: "/v1/stock/{sku}"
    };
  }

  // Server-streaming call: push the authenticated Consumer's stock snapshot.
  // Kong Key Auth injects tenant identity as x-consumer-username metadata.
  // NOT exposed over REST: Kong's grpc-gateway plugin transcodes unary RPCs.
  // Reach this method through the native gRPC route.
  rpc StreamStockUpdates(StreamStockRequest) returns (stream StockUpdate);
}
```

Before generation, prove that the wire declarations remain exactly:

```bash
git diff --word-diff=porcelain -- inventory.proto
```

Expected: only comment text changes; no `message`, field number, field type, RPC name, or HTTP annotation changes.

- [ ] **Step 3: Replace the Makefile with deterministic generation and readiness targets**

Use this complete Makefile:

```make
GO_MODULE := example.com/kong-go-grpc
PROTOC ?= protoc
PROTOC_INCLUDE ?= $(abspath $(dir $(shell command -v $(PROTOC)))/../include)

PROTOC_VERSION := libprotoc 35.1
PROTOC_GEN_GO_VERSION := protoc-gen-go v1.36.11
PROTOC_GEN_GO_GRPC_VERSION := protoc-gen-go-grpc 1.6.2

.PHONY: proto-toolchain proto proto-check tidy up reload down logs run-server

proto-toolchain:
	@actual="$$("$(PROTOC)" --version)"; \
		test "$$actual" = "$(PROTOC_VERSION)" || { \
			echo "expected $(PROTOC_VERSION), got $$actual" >&2; exit 1; \
		}
	@test -f "$(PROTOC_INCLUDE)/google/protobuf/descriptor.proto" || { \
		echo "missing $(PROTOC_INCLUDE)/google/protobuf/descriptor.proto" >&2; \
		exit 1; \
	}
	@actual="$$(go tool protoc-gen-go --version)"; \
		test "$$actual" = "$(PROTOC_GEN_GO_VERSION)" || { \
			echo "expected $(PROTOC_GEN_GO_VERSION), got $$actual" >&2; exit 1; \
		}
	@actual="$$(go tool protoc-gen-go-grpc --version)"; \
		test "$$actual" = "$(PROTOC_GEN_GO_GRPC_VERSION)" || { \
			echo "expected $(PROTOC_GEN_GO_GRPC_VERSION), got $$actual" >&2; exit 1; \
		}

proto: proto-toolchain
	"$(PROTOC)" \
		-I . -I third_party -I "$(PROTOC_INCLUDE)" \
		--plugin=protoc-gen-go="$$(go tool -n protoc-gen-go)" \
		--plugin=protoc-gen-go-grpc="$$(go tool -n protoc-gen-go-grpc)" \
		--go_out=. --go_opt=module=$(GO_MODULE) \
		--go-grpc_out=. --go-grpc_opt=module=$(GO_MODULE) \
		inventory.proto

proto-check: proto
	git diff --exit-code -- proto/gen

tidy:
	go mod tidy

up:
	docker compose up --build -d --wait

reload:
	docker compose exec -T kong kong reload

down:
	docker compose down

logs:
	docker compose logs -f

run-server:
	go run .
```

The generation recipe must not contain `curl`, `googleapis/master`, `go install`, or `@latest`.

- [ ] **Step 4: Confirm the fail-fast compiler guard before generation**

Run:

```bash
make proto
```

Expected on the current machine before installing the approved compiler: a failed comparison against `libprotoc 35.1` and a non-zero exit. Install or otherwise place the exact `protoc 35.1` executable on `PATH`, then rerun the same command.

For this macOS arm64 workspace, install the official release asset with:

```bash
tool_dir="$(mktemp -d)"
curl --fail --location \
  --output "$tool_dir/protoc-35.1-osx-aarch_64.zip" \
  https://github.com/protocolbuffers/protobuf/releases/download/v35.1/protoc-35.1-osx-aarch_64.zip
unzip -q "$tool_dir/protoc-35.1-osx-aarch_64.zip" -d "$tool_dir/protoc-35.1"
install -m 0755 "$tool_dir/protoc-35.1/bin/protoc" ~/.local/bin/protoc
mkdir -p ~/.local/include
cp -R "$tool_dir/protoc-35.1/include/." ~/.local/include/
protoc --version
```

Expected: the last command prints `libprotoc 35.1`, and `~/.local/include/google/protobuf/descriptor.proto` exists. This is an environment prerequisite only; do not add compiler download logic to the repository.

Expected after the prerequisite is present: generation exits zero; the headers remain `protoc-gen-go v1.36.11`, `protoc v7.35.1`, and `protoc-gen-go-grpc v1.6.2`, and generated gRPC comments mention `x-consumer-username`.

- [ ] **Step 5: Inspect, stage, and verify deterministic generation**

Run:

```bash
git diff -- inventory.proto proto/gen go.mod go.sum Makefile
git diff --check
git add inventory.proto proto/gen/inventory.pb.go proto/gen/inventory_grpc.pb.go go.mod go.sum Makefile
make proto-check
go mod verify
go mod tidy -diff
go test ./...
```

Expected: any generated diff is limited to comment-derived output, protobuf wire declarations remain unchanged, `make proto-check` produces no new unstaged generated diff, and every command exits zero.

- [ ] **Step 6: Commit the reproducible protobuf toolchain**

Run:

```bash
git diff --cached --check
git commit -m "build: pin protobuf generation tools"
```

Expected: one commit containing the tool declarations, Make targets, proto comments, and their generated output.

---

## Task 4: Put Key Auth and runtime containment at the Kong boundary

**Files:**

- Modify: `kong.yml`
- Modify: `docker-compose.yml`
- Modify: `Dockerfile`
- Create: `.dockerignore`
- Modify: `index.html`

**Interfaces:**

- Both routes require `apikey`; only header lookup is enabled.
- `company-a-demo-key` maps to Consumer `company-a`; `company-b-demo-key` maps to `company-b`.
- CORS allows `apikey` and exposes `grpc-status` and `grpc-message`.
- Compose publishes only `127.0.0.1:8000` and reports both services healthy.
- The browser sends `apikey` and shows a non-empty body or raw `grpc-message` on errors.

- [ ] **Step 1: Replace the declarative Kong configuration**

Use this complete `kong.yml`:

```yaml
_format_version: "3.0"
_transform: true

consumers:
  - username: company-a
    keyauth_credentials:
      - key: company-a-demo-key

  - username: company-b
    keyauth_credentials:
      - key: company-b-demo-key

services:
  - name: inventory-grpc-service
    protocol: grpc
    host: inventory-service
    port: 50051
    routes:
      - name: inventory-grpc-route
        protocols:
          - grpc
        hosts:
          - inventory.local

      - name: inventory-rest-route
        protocols:
          - http
        paths:
          - /v1/stock

    plugins:
      - name: key-auth
        config:
          key_names:
            - apikey
          key_in_header: true
          key_in_query: false
          key_in_body: false
          hide_credentials: true
          run_on_preflight: false

      - name: rate-limiting
        config:
          minute: 100
          policy: local

      - name: grpc-gateway
        route: inventory-rest-route
        config:
          proto: /kong/proto/inventory.proto

      - name: cors
        route: inventory-rest-route
        config:
          origins:
            - "*"
          methods:
            - GET
          headers:
            - Accept
            - Content-Type
            - apikey
          exposed_headers:
            - grpc-status
            - grpc-message
          preflight_continue: false
```

- [ ] **Step 2: Restrict Compose listeners and add health ordering**

Replace `docker-compose.yml` with:

```yaml
networks:
  kong-grpc-net:
    driver: bridge

services:
  inventory-service:
    build:
      context: .
      dockerfile: Dockerfile
    container_name: inventory-service
    networks:
      - kong-grpc-net
    expose:
      - "50051"
    healthcheck:
      test: ["CMD-SHELL", "nc -w 1 127.0.0.1 50051 </dev/null"]
      interval: 5s
      timeout: 2s
      retries: 10
      start_period: 2s

  kong:
    image: kong/kong-gateway:3.14.0.8-ubuntu
    container_name: kong
    depends_on:
      inventory-service:
        condition: service_healthy
    networks:
      - kong-grpc-net
    environment:
      KONG_DATABASE: "off"
      KONG_DECLARATIVE_CONFIG: /kong/kong.yml
      KONG_PROXY_LISTEN: "0.0.0.0:8000 http2"
      KONG_ADMIN_LISTEN: "off"
      KONG_STATUS_LISTEN: "127.0.0.1:8100"
      KONG_PROXY_ACCESS_LOG: /dev/stdout
      KONG_PROXY_ERROR_LOG: /dev/stderr
    volumes:
      - ./kong.yml:/kong/kong.yml:ro
      - ./inventory.proto:/kong/proto/inventory.proto:ro
      - ./third_party/google:/kong/proto/google:ro
    ports:
      - "127.0.0.1:8000:8000"
    healthcheck:
      test:
        - CMD
        - resty
        - -e
        - >-
          local http = require "resty.http"; local res, err =
          http.new():request_uri("http://127.0.0.1:8100/status/ready");
          assert(res and res.status == 200,
          err or ("HTTP " .. (res and res.status or "nil")))
      interval: 5s
      timeout: 3s
      retries: 12
      start_period: 10s
```

Do not add host mappings for `50051`, `8100`, `8443`, or `8001`.

- [ ] **Step 3: Pin supported build and runtime branches and minimize build context**

Replace `Dockerfile` with:

```dockerfile
FROM golang:1.25-alpine3.24 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o inventory-service .

FROM alpine:3.24
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/inventory-service /usr/local/bin/inventory-service
EXPOSE 50051
CMD ["inventory-service"]
```

Create `.dockerignore` with exactly:

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

- [ ] **Step 4: Switch the browser demo from Company ID to API key**

Keep the existing HTML and CSS shell. Replace the explanatory paragraph and form labels with:

```html
<p style="font-size: 13px; color: #555">
  Halaman ini memanggil <code>GET /v1/stock/{sku}</code> dengan demo
  <code>apikey</code> di Kong (port 8000). Kong memvalidasi key, menentukan
  Consumer, lalu men-transcode HTTP GET menjadi call gRPC ke
  inventory-service. Demo key ini publik dan hanya untuk stack lokal.
</p>

<label>
  Kong base URL
  <input id="base" value="http://localhost:8000" />
</label>
<label>
  Demo API key
  <input
    id="apikey"
    type="password"
    value="company-a-demo-key"
    autocomplete="off"
  />
</label>
<label>
  SKU
  <input id="sku" value="SKU-001" />
</label>
```

Replace the script with:

```html
<script>
  async function checkStock() {
    const base = document.getElementById("base").value.replace(/\/$/, "");
    const apikey = document.getElementById("apikey").value;
    const sku = encodeURIComponent(document.getElementById("sku").value);
    const out = document.getElementById("out");
    const url = `${base}/v1/stock/${sku}`;

    out.textContent = `GET ${url}\napikey: [hidden]\n\nMemproses request`;
    try {
      const res = await fetch(url, {
        headers: { apikey },
      });
      const body = await res.text();
      if (res.ok) {
        out.textContent = `HTTP ${res.status}\n\n${body}`;
        return;
      }

      const grpcMessage = res.headers.get("grpc-message");
      const detail = body || grpcMessage || res.statusText || "Request failed";
      out.textContent = `HTTP ${res.status}\n\n${detail}`;
    } catch (err) {
      out.textContent = `Gagal fetch: ${err.message}\n\nPastikan Kong aktif di ${base} dan CORS mengizinkan origin halaman ini.`;
    }
  }
</script>
```

Do not call `decodeURIComponent` on `grpc-message`.

- [ ] **Step 5: Validate static configuration against the selected image**

Run:

```bash
docker compose config -q
docker run --rm \
  -e KONG_DATABASE=off \
  -v "$PWD/kong.yml:/kong/kong.yml:ro" \
  -v "$PWD/inventory.proto:/kong/proto/inventory.proto:ro" \
  -v "$PWD/third_party/google:/kong/proto/google:ro" \
  kong/kong-gateway:3.14.0.8-ubuntu \
  kong config parse /kong/kong.yml
```

Expected: Compose exits silently with zero; Kong reports that the file can be parsed and exits zero. Factual erratum: starting with Kong Gateway 3.10, Enterprise Free Mode is unavailable, and license-free startup follows expired-license behavior; related license notices are separate from schema validation.

Run these containment assertions:

```bash
docker compose config | grep -F 'host_ip: 127.0.0.1'
docker compose config | grep -F 'published: "8000"'
! docker compose config | grep -E 'published: "?(50051|8001|8100|8443)"?'
! rg -n 'X-Company-Id|x-company-id' kong.yml index.html
```

Expected: the first two assertions print the loopback host and proxy publication; both negative assertions exit zero.

- [ ] **Step 6: Build the images and commit boundary configuration**

Run:

```bash
docker compose build
git diff --check
git add kong.yml docker-compose.yml Dockerfile .dockerignore index.html
git diff --cached --check
git commit -m "feat: authenticate and contain Kong demo traffic"
```

Expected: the inventory image builds with the pinned Go and Alpine tags and the commit contains only boundary, runtime, and browser files.

---

## Task 5: Document and prove the authenticated local workflow

**Files:**

- Modify: `README.md`

**Interfaces:**

- Documentation uses `apikey` in every client example and never presents `X-Company-Id` as trusted input.
- REST and native gRPC examples show company A and B isolation.
- Streaming examples contain two tenant-specific records and quote `updatedAtUnix` because ProtoJSON encodes `int64` as a string.
- Operations documentation explains readiness, reload, the disabled Admin API, the mixed HTTP/1.1 and h2c proxy listener, and local-only credentials.

- [ ] **Step 1: Rewrite the README around the actual trust boundary**

Replace the current README content with these sections in order:

1. `# Kong + gRPC (Go)` — describe a local multi-tenant demo with Kong Key Auth, native gRPC, and REST-to-gRPC transcoding.
2. `## Trust boundary and request flow` — show `client apikey -> Kong Key Auth -> X-Consumer-Username -> Go inventory map`, and state that port `50051` is unpublished.
3. `## Architecture` — explain that loopback port `8000` accepts both HTTP/1.1 REST and plaintext HTTP/2 gRPC; only `GetStock` is transcoded.
4. `## Demo credentials` — include the exact two-Consumer table and label both keys public and local-only.
5. `## Prerequisites and startup` — require Go 1.25.3 or newer, `protoc 35.1`, Docker with Compose, `curl`, `jq`, and `grpcurl`; document `make proto`, `make proto-check`, `make up`, `make reload`, `make logs`, and `make down`.
6. `## API examples` — include the exact REST, unary gRPC, and server-streaming commands below.
7. `## Error behavior` — reproduce the approved gRPC error matrix and explain that Kong rejects missing or invalid API keys first.
8. `## Readiness and exposed ports` — explain inventory TCP readiness, Kong `/status/ready`, `make up` using `--wait`, Admin API off, and only `127.0.0.1:8000` published.
9. `## Local-demo limitations` — list plaintext h2c, static public credentials, in-memory data, local rate limiting, permissive CORS, and no persistence. State that TLS or mTLS depends on the production threat model rather than claiming one transport is universally mandatory.
10. `## Repository layout` — update image/tool versions and include `.dockerignore` and `main_test.go`.

Name the exact runtime and generator versions: `kong/kong-gateway:3.14.0.8-ubuntu`, `golang:1.25-alpine3.24`, `alpine:3.24`, `protoc 35.1`, `protoc-gen-go v1.36.11`, and `protoc-gen-go-grpc v1.6.2`. Explain that the selected supported Kong LTS is the Enterprise distribution. Add a factual erratum that starting with Kong Gateway 3.10, Enterprise Free Mode is unavailable and license-free startup follows expired-license behavior.

Document this pinned `grpcurl` installation when it is not already available:

```bash
GOBIN=~/.local/bin go install github.com/fullstorydev/grpcurl/cmd/grpcurl@v1.9.3
```

Use these exact credential rows:

```markdown
| Consumer | Demo API key |
| --- | --- |
| `company-a` | `company-a-demo-key` |
| `company-b` | `company-b-demo-key` |
```

Use these REST examples:

```bash
curl -H 'apikey: company-a-demo-key' \
  http://localhost:8000/v1/stock/SKU-001

curl -H 'apikey: company-b-demo-key' \
  http://localhost:8000/v1/stock/SKU-001
```

Document quantities `42` and `100`, respectively.

Use these unary gRPC examples:

```bash
grpcurl -plaintext -authority inventory.local \
  -H 'apikey: company-a-demo-key' \
  -d '{"sku":"SKU-001"}' \
  localhost:8000 inventory.InventoryService/GetStock

grpcurl -plaintext -authority inventory.local \
  -H 'apikey: company-b-demo-key' \
  -d '{"sku":"SKU-001"}' \
  localhost:8000 inventory.InventoryService/GetStock
```

Use these stream examples:

```bash
grpcurl -plaintext -authority inventory.local \
  -H 'apikey: company-a-demo-key' \
  -d '{}' \
  localhost:8000 inventory.InventoryService/StreamStockUpdates

grpcurl -plaintext -authority inventory.local \
  -H 'apikey: company-b-demo-key' \
  -d '{}' \
  localhost:8000 inventory.InventoryService/StreamStockUpdates
```

Show company A records `SKU-001=42`, `SKU-002=7` and company B records `SKU-001=100`, `SKU-777=3`. In every sample stream response, render `updatedAtUnix` as a quoted decimal string and explain that its runtime value changes.

Run:

```bash
! rg -n 'No auth|X-Company-Id|x-company-id|kong:3\.7|alpine:3\.20|@latest|googleapis/master|3 update' README.md
! rg -n '"quantity"[[:space:]]*:[[:space:]]*10([,}])' README.md
```

Expected: no stale behavior or version text remains.

- [ ] **Step 2: Start the full stack and confirm deterministic readiness**

Run:

```bash
set -euo pipefail
if ! command -v grpcurl >/dev/null 2>&1; then
  GOBIN=~/.local/bin go install github.com/fullstorydev/grpcurl/cmd/grpcurl@v1.9.3
fi
command -v curl jq grpcurl nc python3 docker go protoc >/dev/null
grpcurl -version
make up
docker compose ps
docker inspect --format '{{.State.Health.Status}}' inventory-service
docker inspect --format '{{.State.Health.Status}}' kong
```

Expected: `make up` exits only after both containers are ready; both inspect commands print `healthy`.

- [ ] **Step 3: Execute REST authentication, isolation, spoofing, and CORS checks**

Run:

```bash
set -euo pipefail
preflight_headers=
error_headers=
trap 'rm -f "${preflight_headers:-}" "${error_headers:-}"' EXIT
missing_status="$(curl -sS -o /dev/null -w '%{http_code}' \
  http://localhost:8000/v1/stock/SKU-001)"
test "$missing_status" = 401

invalid_status="$(curl -sS -o /dev/null -w '%{http_code}' \
  -H 'apikey: invalid-demo-key' \
  http://localhost:8000/v1/stock/SKU-001)"
test "$invalid_status" = 401

curl --fail --silent --show-error \
  -H 'apikey: company-a-demo-key' \
  http://localhost:8000/v1/stock/SKU-001 | \
  jq -e '.sku == "SKU-001" and .quantity == 42'
curl --fail --silent --show-error \
  -H 'apikey: company-b-demo-key' \
  http://localhost:8000/v1/stock/SKU-001 | \
  jq -e '.sku == "SKU-001" and .quantity == 100'
curl --fail --silent --show-error \
  -H 'apikey: company-a-demo-key' \
  -H 'x-consumer-username: company-b' \
  -H 'x-company-id: company-b' \
  http://localhost:8000/v1/stock/SKU-001 | \
  jq -e '.sku == "SKU-001" and .quantity == 42'

preflight_headers="$(mktemp)"
curl --silent --show-error -D "$preflight_headers" -o /dev/null -X OPTIONS \
  -H 'Origin: http://localhost:8080' \
  -H 'Access-Control-Request-Method: GET' \
  -H 'Access-Control-Request-Headers: apikey' \
  http://localhost:8000/v1/stock/SKU-001
tr -d '\r' < "$preflight_headers" | rg -q '^HTTP/[0-9.]+ 200 '
tr -d '\r' < "$preflight_headers" | rg -iq '^access-control-allow-origin:[[:space:]]*\*'
tr -d '\r' < "$preflight_headers" | rg -iq '^access-control-allow-methods:.*GET'
tr -d '\r' < "$preflight_headers" | rg -iq '^access-control-allow-headers:.*apikey'

error_headers="$(mktemp)"
error_status="$(curl --silent --show-error \
  -D "$error_headers" -o /dev/null -w '%{http_code}' \
  -H 'Origin: http://localhost:8080' \
  -H 'apikey: company-a-demo-key' \
  http://localhost:8000/v1/stock/SKU-404)"
test "$error_status" = 404
tr -d '\r' < "$error_headers" | \
  rg -iq '^access-control-expose-headers:[[:space:]]*grpc-status,[[:space:]]*grpc-message'
```

Expected:

- missing and invalid credentials return HTTP `401`;
- company A returns HTTP `200` with quantity `42`;
- company B returns HTTP `200` with quantity `100`;
- company A's key plus both spoofed tenant headers still returns quantity `42`;
- the preflight returns HTTP `200` locally and includes `Access-Control-Allow-Origin: *`, `GET`, and `apikey` in its allow headers;
- an actual error response exposes both `grpc-status` and `grpc-message` to browser JavaScript.

- [ ] **Step 4: Execute native gRPC authentication, isolation, spoofing, and streaming checks**

Run the unauthenticated request and require a non-zero exit:

```bash
set -euo pipefail
! grpcurl -plaintext -authority inventory.local \
  -d '{"sku":"SKU-001"}' \
  localhost:8000 inventory.InventoryService/GetStock
! grpcurl -plaintext -authority inventory.local \
  -H 'apikey: invalid-demo-key' \
  -d '{"sku":"SKU-001"}' \
  localhost:8000 inventory.InventoryService/GetStock
```

Do not assert an exact grpcurl error string: Kong's HTTP `401` may be rendered differently by grpcurl versions.

Run authenticated unary and spoof checks:

```bash
set -euo pipefail
grpcurl -plaintext -authority inventory.local \
  -H 'apikey: company-a-demo-key' \
  -d '{"sku":"SKU-001"}' \
  localhost:8000 inventory.InventoryService/GetStock | \
  jq -e '.sku == "SKU-001" and .quantity == 42'
grpcurl -plaintext -authority inventory.local \
  -H 'apikey: company-b-demo-key' \
  -d '{"sku":"SKU-001"}' \
  localhost:8000 inventory.InventoryService/GetStock | \
  jq -e '.sku == "SKU-001" and .quantity == 100'
grpcurl -plaintext -authority inventory.local \
  -H 'apikey: company-a-demo-key' \
  -H 'x-consumer-username: company-b' \
  -H 'x-company-id: company-b' \
  -d '{"sku":"SKU-001"}' \
  localhost:8000 inventory.InventoryService/GetStock | \
  jq -e '.sku == "SKU-001" and .quantity == 42'
```

Expected quantities: `42`, `100`, and `42`.

Assert both stream payloads:

```bash
set -euo pipefail
grpcurl -plaintext -authority inventory.local \
  -H 'apikey: company-a-demo-key' \
  -d '{}' \
  localhost:8000 inventory.InventoryService/StreamStockUpdates | \
  jq -s -e 'length == 2
    and .[0].sku == "SKU-001" and .[0].quantity == 42
    and .[1].sku == "SKU-002" and .[1].quantity == 7
    and ([.[].updatedAtUnix | type] == ["string", "string"])'
grpcurl -plaintext -authority inventory.local \
  -H 'apikey: company-b-demo-key' \
  -d '{}' \
  localhost:8000 inventory.InventoryService/StreamStockUpdates | \
  jq -s -e 'length == 2
    and .[0].sku == "SKU-001" and .[0].quantity == 100
    and .[1].sku == "SKU-777" and .[1].quantity == 3
    and ([.[].updatedAtUnix | type] == ["string", "string"])'
```

Expected: both assertions exit zero. The virtual-time unit tests, rather than wall-clock shell timing, prove the exact 500 ms interval and absence of a final delay.

- [ ] **Step 5: Verify browser errors, reload, and unpublished management ports**

Serve `index.html` from a separate terminal:

```bash
python3 -m http.server --bind 127.0.0.1 8080
```

Open `http://localhost:8080/index.html`, keep `company-a-demo-key`, request `SKU-404`, and confirm the visible result contains the HTTP error detail rather than a blank output. Stop the temporary HTTP server afterward.

Run:

```bash
set -euo pipefail
make reload
! curl --connect-timeout 1 http://127.0.0.1:8001/
! nc -z -w 1 127.0.0.1 8001
! nc -z -w 1 127.0.0.1 8443
! nc -z -w 1 127.0.0.1 50051
```

Expected: Kong reloads successfully; every unpublished-port check exits zero because no host mapping exists.

- [ ] **Step 6: Run the complete verification gate and clean temporary runtime state**

Run in this order:

```bash
set -euo pipefail
go test -race ./...
go vet ./...
go build ./...
go mod verify
go mod tidy -diff
make proto-check
docker compose config -q
docker run --rm \
  -e KONG_DATABASE=off \
  -v "$PWD/kong.yml:/kong/kong.yml:ro" \
  -v "$PWD/inventory.proto:/kong/proto/inventory.proto:ro" \
  -v "$PWD/third_party/google:/kong/proto/google:ro" \
  kong/kong-gateway:3.14.0.8-ubuntu \
  kong config parse /kong/kong.yml
git diff --check
make down
docker compose ps --all
```

Expected: all gates exit zero, Kong prints `parse successful`, and the final Compose listing has no service rows.

- [ ] **Step 7: Commit the corrected project documentation**

Run:

```bash
git add README.md
git diff --cached --check
git commit -m "docs: document authenticated Kong demo"
```

Expected: one documentation-only commit.

---

## Task 6: Review, push the fork branch, and open the upstream draft PR

**Files:**

- Review only: every changed file relative to `upstream/master`
- External state: fork branch and one draft pull request

**Interfaces:**

- Push target: `origin/codex/harden-multitenant-demo` in `davidgrldo/kong-go-grpc-example`.
- Draft PR target: `revell29/kong-go-grpc-example:master`.

- [ ] **Step 1: Audit final scope and history**

Run:

```bash
set -euo pipefail
git fetch upstream master
test "$(git branch --show-current)" = codex/harden-multitenant-demo
test "$(git remote get-url origin)" = git@github.com:davidgrldo/kong-go-grpc-example.git
test "$(git remote get-url upstream)" = git@github.com:revell29/kong-go-grpc-example.git
test -z "$(git status --porcelain)"
git merge-base --is-ancestor upstream/master HEAD
git status --short
git log --oneline --decorate upstream/master..HEAD
git diff --stat upstream/master...HEAD
git diff --check upstream/master...HEAD
```

Expected: the remotes and branch match the delivery contract, the worktree is clean, history contains the approved design and plan plus focused implementation commits, the stat contains only expected files, and the diff check exits zero.

- [ ] **Step 2: Request an independent code review**

Use `superpowers:requesting-code-review` against `upstream/master...HEAD`. Require the reviewer to check:

- authentication metadata ambiguity and spoof resistance;
- unary error precedence and stream cancellation behavior;
- protobuf wire compatibility and generator reproducibility;
- Kong plugin scope, CORS, and hidden credentials;
- Compose port exposure and readiness semantics;
- README commands against observed smoke-test output.

Resolve every High or Medium finding before continuing. Re-run the smallest relevant focused test plus the complete verification gate after any fix, then commit the fix with a specific message. If a fix touches `main.go`, `kong.yml`, `docker-compose.yml`, `index.html`, or generated API behavior, also repeat the affected REST, native gRPC, CORS, spoofing, streaming, and browser checks from Task 5. Submit each fix batch for another independent review and repeat until the final `HEAD` has no unresolved High or Medium findings.

- [ ] **Step 3: Push the completed branch to the fork**

Run:

```bash
set -euo pipefail
test -z "$(git status --porcelain)"
git merge-base --is-ancestor upstream/master HEAD
git push -u origin codex/harden-multitenant-demo
test "$(git rev-parse HEAD)" = "$(git rev-parse '@{upstream}')"
test "$(git rev-parse HEAD)" = "$(git rev-parse origin/codex/harden-multitenant-demo)"
```

Expected: the remote branch is created or updated successfully and local tracking points to `origin/codex/harden-multitenant-demo`.

- [ ] **Step 4: Open the upstream pull request as a draft**

Use the GitHub publishing workflow, preferring the connected GitHub app and falling back to `gh`, with:

- repository: `revell29/kong-go-grpc-example`;
- base: `master`;
- head: `davidgrldo:codex/harden-multitenant-demo`;
- title: `Harden multi-tenant Kong gRPC demo`;
- state: draft;
- body summary: Kong Key Auth tenant boundary, actual isolated stream snapshots, cancellation-aware timing, readiness and port containment, pinned protobuf/runtime tooling, updated browser and docs;
- test section: every command from Task 5's complete verification gate plus REST and native gRPC smoke checks;
- limitation note: committed demo keys and plaintext h2c are local examples, not production credential or transport guidance.

If `gh` is required, run:

```bash
set -euo pipefail
pr_url="$(gh pr create \
  --repo revell29/kong-go-grpc-example \
  --base master \
  --head davidgrldo:codex/harden-multitenant-demo \
  --draft \
  --title 'Harden multi-tenant Kong gRPC demo' \
  --body $'## Summary\n\n- authenticate both routes with Kong Key Auth\n- isolate unary and streaming inventory by authenticated Consumer\n- add tests, readiness, runtime pins, and reproducible protobuf generation\n- update the browser demo and documentation\n\n## Verification\n\n- go test -race ./...\n- go vet ./...\n- go build ./...\n- go mod verify\n- go mod tidy -diff\n- make proto-check\n- docker compose config -q\n- Kong declarative config parse\n- live REST and native gRPC authentication, isolation, spoof, CORS, and streaming smoke checks\n\n## Local-demo limitations\n\nThe committed API keys and plaintext h2c listener are intentionally local-demo examples, not production credential or transport guidance.')"

head_oid="$(git rev-parse HEAD)"
gh pr view "$pr_url" \
  --json url,title,body,isDraft,baseRefName,headRefName,headRefOid,headRepositoryOwner | \
  jq -e --arg oid "$head_oid" '
    .isDraft == true
    and .baseRefName == "master"
    and .headRefName == "codex/harden-multitenant-demo"
    and .headRepositoryOwner.login == "davidgrldo"
    and .headRefOid == $oid
    and .title == "Harden multi-tenant Kong gRPC demo"
    and (.body | contains("Local-demo limitations"))'
```

Expected: GitHub returns a draft PR URL under `https://github.com/revell29/kong-go-grpc-example/pull/`, and the metadata assertion exits zero. When the connected GitHub app creates the PR, perform the equivalent metadata read through the app.

- [ ] **Step 5: Report delivery evidence**

Report the final commit IDs, pushed branch, draft PR URL, verification results, and the corrected approved-spec commit ID. Do not mark the work complete if any required check is skipped or failing.
