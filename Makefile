.PHONY: proto tidy up down logs run-server

# Requires locally installed: protoc, protoc-gen-go, protoc-gen-go-grpc
# (go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#  go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest)
proto:
	@mkdir -p third_party/google/api
	@if [ ! -f third_party/google/api/http.proto ]; then \
		curl -sL https://raw.githubusercontent.com/googleapis/googleapis/master/google/api/http.proto \
			-o third_party/google/api/http.proto; \
	fi
	@if [ ! -f third_party/google/api/annotations.proto ]; then \
		curl -sL https://raw.githubusercontent.com/googleapis/googleapis/master/google/api/annotations.proto \
			-o third_party/google/api/annotations.proto; \
	fi
	protoc \
		-I . -I third_party \
		--go_out=. --go_opt=module=example.com/kong-go-grpc \
		--go-grpc_out=. --go-grpc_opt=module=example.com/kong-go-grpc \
		inventory.proto

tidy:
	go mod tidy

up:
	docker compose up --build -d

down:
	docker compose down

logs:
	docker compose logs -f

run-server:
	go run .
