package main

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
