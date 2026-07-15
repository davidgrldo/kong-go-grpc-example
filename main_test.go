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
