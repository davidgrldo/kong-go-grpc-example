// Command server implements a small multi-tenant gRPC InventoryService.
// It sits behind Kong, which authenticates callers and injects the matched
// Consumer username for tenant resolution before proxying plaintext HTTP/2
// to this unpublished service on port 50051.
package main

import (
	"context"
	"log"
	"net"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	pb "example.com/kong-go-grpc/proto/gen"
)

const (
	listenAddr     = ":50051"
	streamInterval = 500 * time.Millisecond
)

// stockStore is a fake per-company, per-SKU inventory table.
// companyID -> sku -> quantity
type stockStore struct {
	mu   sync.Mutex
	data map[string]map[string]int32
}

func newStockStore() *stockStore {
	return &stockStore{
		data: map[string]map[string]int32{
			"company-a": {"SKU-001": 42, "SKU-002": 7},
			"company-b": {"SKU-001": 100, "SKU-777": 3},
		},
	}
}

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

type inventoryServer struct {
	pb.UnimplementedInventoryServiceServer
	store *stockStore
}

func logIncomingMetadata(ctx context.Context, rpcName string) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		log.Printf("[%s] no metadata on request", rpcName)
		return
	}
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
}

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

func (s *inventoryServer) GetStock(ctx context.Context, req *pb.GetStockRequest) (*pb.GetStockResponse, error) {
	logIncomingMetadata(ctx, "GetStock")

	companyID, err := companyIDFromContext(ctx)
	if err != nil {
		return nil, err
	}

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
}

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

func main() {
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", listenAddr, err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterInventoryServiceServer(grpcServer, &inventoryServer{store: newStockStore()})

	// Enables `grpcurl -plaintext` without needing to ship .proto files to the client.
	reflection.Register(grpcServer)

	log.Printf("inventory gRPC server listening on %s (plaintext h2c, no TLS)", listenAddr)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("grpc server error: %v", err)
	}
}
