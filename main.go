// Command server implements a small multi-tenant gRPC InventoryService.
// It is meant to sit *behind* Kong: Kong terminates the gRPC route and
// proxies to this service over plaintext HTTP/2 on port 50051.
//
// It also demonstrates reading metadata that Kong/plugins typically inject
// upstream (e.g. X-Consumer-Id from a key-auth plugin, X-Forwarded-Host,
// X-Forwarded-Proto) so you can see what a real gateway-fronted service
// would have available for tenant resolution / auditing.
package main

import (
	"context"
	"log"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	pb "example.com/kong-go-grpc/proto/gen"
)

const listenAddr = ":50051"

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

func (s *stockStore) get(company, sku string) (int32, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	companyStock, ok := s.data[company]
	if !ok {
		return 0, false
	}
	qty, ok := companyStock[sku]
	return qty, ok
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
	for _, key := range []string{":authority", "x-forwarded-host", "x-forwarded-proto", "x-company-id", "x-consumer-id", "x-consumer-username"} {
		if v := md.Get(key); len(v) > 0 {
			log.Printf("[%s] metadata %s = %v", rpcName, key, v)
		}
	}
}

func companyIDFromContext(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.InvalidArgument, "missing metadata")
	}
	vals := md.Get("x-company-id")
	if len(vals) == 0 || vals[0] == "" {
		return "", status.Error(codes.InvalidArgument, "x-company-id header is required")
	}
	return vals[0], nil
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

	qty, ok := s.store.get(companyID, req.GetSku())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "sku %q not found for company %q", req.GetSku(), companyID)
	}

	return &pb.GetStockResponse{
		Sku:       req.GetSku(),
		Quantity:  qty,
		Warehouse: "WH-JKT-01",
	}, nil
}

func (s *inventoryServer) StreamStockUpdates(req *pb.StreamStockRequest, stream pb.InventoryService_StreamStockUpdatesServer) error {
	logIncomingMetadata(stream.Context(), "StreamStockUpdates")

	companyID, err := companyIDFromContext(stream.Context())
	if err != nil {
		return err
	}
	_ = companyID // demo: tidak filter per company untuk streaming

	skus := []string{"SKU-001", "SKU-002", "SKU-777"}
	for i, sku := range skus {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		default:
		}

		update := &pb.StockUpdate{
			Sku:           sku,
			Quantity:      int32(10 * (i + 1)),
			UpdatedAtUnix: time.Now().Unix(),
		}
		if err := stream.Send(update); err != nil {
			return err
		}
		time.Sleep(500 * time.Millisecond)
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
