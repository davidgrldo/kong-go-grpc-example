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
