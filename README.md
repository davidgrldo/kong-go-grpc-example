# Kong + gRPC (Go)

Demo Kong sebagai API gateway di depan gRPC service multi-company. Kong
proxy gRPC over plaintext HTTP/2 (h2c) — tanpa TLS — agar demo sederhana.
`company_id` dikirim via header `X-Company-Id`, bukan di URL params.

## Arsitektur

Satu upstream service, dua route di Kong. Kong selalu bicara native gRPC
ke service — yang berbeda hanya route mana yang dipakai caller.

```
┌──────────────────┐                     ┌──────────────────────────────────────┐                     ┌─────────────────────┐
│  gRPC Client     │                     │  Kong :8000                          │                     │  inventory-service  │
│  (grpcurl/Go)    │──── h2c ──────────>│                                      │──── h2c ──────────>│  :50051             │
│                  │  :authority=       │  Route 1: [grpc]                     │  protocol=grpc      │  (main.go)          │
│                  │  inventory.local   │  match: host=inventory.local          │                     │                     │
└──────────────────┘                     │                                      │                     │  GetStock (unary)   │
                                         │  plugins:                            │                     │  StreamStockUpdates │
┌──────────────────┐                     │  - rate-limiting (100/min)           │                     │  (server-streaming) │
│  Browser         │──── HTTP GET ──────>│                                      │                     └─────────────────────┘
│  (index.html)    │  /v1/stock/{sku}   │  Route 2: [http]                     │
│                  │  X-Company-Id: ...  │  match: path=/v1/stock                │
└──────────────────┘                     │                                      │
                                         │  plugins:                            │
                                         │  - rate-limiting (100/min)           │
                                         │  - grpc-gateway (JSON ↔ protobuf)   │
                                         │  - cors (origins: *)                 │
                                         └──────────────────────────────────────┘
```

### Cara kerja routing

**Route 1 — Native gRPC** (`protocol: grpc`, match by `hosts`):

1. Client dial Kong `:8000`, set header `:authority=inventory.local`
2. Kong match route `grpc` berdasarkan host `inventory.local`
3. Kong proxy ke `inventory-service:50051` via h2c (plaintext HTTP/2)
4. Service proses gRPC call, return protobuf response
5. Kong proxy balik ke client

**Route 2 — REST/Browser** (`protocol: http`, match by `paths`):

1. Browser `GET http://localhost:8000/v1/stock/SKU-001` dengan header `X-Company-Id: company-a`
2. Kong match route `http` berdasarkan path `/v1/stock`
3. Plugin `grpc-gateway` baca `inventory.proto`, transcode HTTP request → gRPC call
4. Kong forward header `X-Company-Id` sebagai gRPC metadata `x-company-id`
5. Kong proxy gRPC call ke `inventory-service:50051`
6. Service baca `x-company-id` dari metadata, lookup stok per company
7. Service return protobuf → `grpc-gateway` encode balik jadi JSON
8. Browser terima JSON

> **Limitasi**: `grpc-gateway` hanya transcode unary RPC. `GetStock` bisa
> lewat REST; `StreamStockUpdates` hanya lewat native gRPC route. Untuk
> streaming di browser butuh `grpc-web` plugin + JS stub.

## Struktur file

```
.
├── main.go                          # gRPC server (InventoryService, port :50051)
├── inventory.proto                  # proto definition (GetStock + StreamStockUpdates)
├── proto/gen/                       # generated pb.go & grpc.pb.go (dari make proto)
├── kong.yml                         # Kong declarative config (DB-less)
├── docker-compose.yml               # Kong 3.7 + inventory-service
├── Dockerfile                       # multi-stage build untuk inventory-service
├── index.html                       # browser client (plain HTML/JS, fetch ke Kong)
├── third_party/google/api/          # annotations.proto + http.proto (untuk grpc-gateway)
├── Makefile                         # proto, tidy, up, down, logs, run-server
├── go.mod / go.sum                  # Go module: example.com/kong-go-grpc
└── README.md
```

### Komponen

- **`main.go`** — gRPC server. In-memory store untuk 2 company (`company-a`,
  `company-b`). Baca `company_id` dari header `X-Company-Id` (gRPC metadata).
  Log metadata `x-consumer-*` / `x-forwarded-*` yang di-inject Kong. Reflection
  diaktifkan agar `grpcurl` bisa pakai tanpa file `.proto`.
- **`inventory.proto`** — `InventoryService` dengan 2 RPC:
  - `GetStock(GetStockRequest) → GetStockResponse` (unary, REST-compatible)
  - `StreamStockUpdates(StreamStockRequest) → stream StockUpdate` (server-streaming)
- **`kong.yml`** — DB-less config: 1 service (`protocol: grpc`), 2 routes
  (`grpc` + `http`), 3 plugins (`rate-limiting`, `grpc-gateway`, `cors`).
- **`index.html`** — plain HTML/JS, `fetch()` ke Kong. Tidak tahu gRPC.
- **`third_party/google/api/`** — `annotations.proto` + `http.proto` untuk
  `google.api.http` option yang dibaca `grpc-gateway` plugin.

## Setup

Prasyarat: Go 1.25+, `protoc`, `protoc-gen-go`, `protoc-gen-go-grpc`,
Docker/Docker Compose.

```bash
# Install protoc plugins (sekali saja)
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# 1. Generate proto code (auto-download google/api protos jika belum ada)
make proto

# 2. Resolve dependencies
make tidy

# 3. Jalankan Kong + service via Docker
make up

# 4. Atau jalankan service langsung (tanpa Docker)
make run-server
```

## API Reference

### RPC 1: `GetStock` (Unary)

Cek stok untuk satu SKU. `company_id` dikirim via header `X-Company-Id`,
bukan di URL atau request body.

|                    |                                               |
| ------------------ | --------------------------------------------- |
| **gRPC method**    | `inventory.InventoryService/GetStock`         |
| **Type**           | Unary (request → response)                    |
| **REST endpoint**  | `GET /v1/stock/{sku}`                         |
| **Header**         | `X-Company-Id: <company_id>` (required)       |
| **Accessible via** | Native gRPC route + REST route (grpc-gateway) |

**Request — `GetStockRequest`**

| Field | Type     | Required | Description                            |
| ----- | -------- | -------- | -------------------------------------- |
| `sku` | `string` | yes      | Kode SKU, contoh: `SKU-001`, `SKU-002` |

> `company_id` tidak ada di proto request — dikirim via header
> `X-Company-Id` (gRPC metadata `x-company-id`). Server baca dari
> `metadata.FromIncomingContext(ctx)`.

**Response — `GetStockResponse`**

| Field       | Type     | Description                              |
| ----------- | -------- | ---------------------------------------- |
| `sku`       | `string` | SKU yang diminta                         |
| `quantity`  | `int32`  | Jumlah stok tersedia                     |
| `warehouse` | `string` | Kode gudang (selalu `WH-JKT-01` di demo) |

**Error codes**

| Code              | Condition                                      |
| ----------------- | ---------------------------------------------- |
| `InvalidArgument` | `X-Company-Id` header kosong atau `sku` kosong |
| `NotFound`        | SKU tidak ditemukan untuk company tersebut     |

**Contoh (gRPC)**

```bash
grpcurl -plaintext -authority inventory.local \
  -H 'x-company-id: company-a' \
  -d '{"sku":"SKU-001"}' \
  localhost:8000 inventory.InventoryService/GetStock
```

Response:

```json
{
  "sku": "SKU-001",
  "quantity": 42,
  "warehouse": "WH-JKT-01"
}
```

**Contoh (REST)**

```bash
curl -H "X-Company-Id: company-a" \
  "http://localhost:8000/v1/stock/SKU-001"
```

Response:

```json
{ "sku": "SKU-001", "quantity": 42, "warehouse": "WH-JKT-01" }
```

---

### RPC 2: `StreamStockUpdates` (Server-Streaming)

Push update stok untuk semua SKU. `company_id` dikirim via header
`X-Company-Id`. Server mengirim 3 update dengan interval 500ms per item,
lalu menutup stream.

|                    |                                                         |
| ------------------ | ------------------------------------------------------- |
| **gRPC method**    | `inventory.InventoryService/StreamStockUpdates`         |
| **Type**           | Server-streaming (request → stream of responses)        |
| **REST endpoint**  | ❌ Tidak ada — `grpc-gateway` hanya transcode unary RPC |
| **Header**         | `X-Company-Id: <company_id>` (required)                 |
| **Accessible via** | Native gRPC route only                                  |

**Request — `StreamStockRequest`**

Kosong — tidak ada field. `company_id` dikirim via header `X-Company-Id`.

**Stream response — `StockUpdate` (3 messages)**

| #   | `sku`     | `quantity` | `updated_at_unix` |
| --- | --------- | ---------- | ----------------- |
| 1   | `SKU-001` | `10`       | `<timestamp>`     |
| 2   | `SKU-002` | `20`       | `<timestamp>`     |
| 3   | `SKU-777` | `30`       | `<timestamp>`     |

**Error codes**

| Code              | Condition                    |
| ----------------- | ---------------------------- |
| `InvalidArgument` | `X-Company-Id` header kosong |

**Contoh (gRPC only)**

```bash
grpcurl -plaintext -authority inventory.local \
  -H 'x-company-id: company-a' \
  -d '{}' \
  localhost:8000 inventory.InventoryService/StreamStockUpdates
```

Response (3 messages, 500ms interval):

```json
{"sku":"SKU-001","quantity":10,"updatedAtUnix":1720948800}
{"sku":"SKU-002","quantity":20,"updatedAtUnix":1720948800}
{"sku":"SKU-777","quantity":30,"updatedAtUnix":1720948800}
```

---

### Sample data

| Company     | SKU       | Quantity |
| ----------- | --------- | -------- |
| `company-a` | `SKU-001` | 42       |
| `company-a` | `SKU-002` | 7        |
| `company-b` | `SKU-001` | 100      |
| `company-b` | `SKU-777` | 3        |

### Metadata yang di-log

Server mencatat header berikut jika Kong/plugin meng-inject-nya:

| Header                | Source               | Ada jika                     |
| --------------------- | -------------------- | ---------------------------- |
| `:authority`          | HTTP/2 pseudo-header | Selalu (gRPC client set ini) |
| `x-forwarded-host`    | Kong core            | Selalu saat le wat Kong      |
| `x-forwarded-proto`   | Kong core            | Selalu saat lewat Kong       |
| `x-company-id`        | Client header        | Selalu (client wajib set)    |
| `x-consumer-id`       | key-auth/JWT plugin  | Hanya jika auth plugin aktif |
| `x-consumer-username` | key-auth/JWT plugin  | Hanya jika auth plugin aktif |

## Yang di-skip (bukan untuk production)

- **No TLS** — pakai plaintext h2c. Production harus pakai `grpcs` + cert.
- **No auth** — tidak ada key-auth/JWT/mTLS. Siapa saja yang bisa akses
  port 8000 bisa call service.
- **No persistence** — data stok in-memory, reset saat restart.
- **CORS `*`** — terbuka lebar, scope ke origin spesifik untuk production.
- **Kong 3.6 ke bawah** — tidak bisa serve HTTP/2 + HTTP/1.1 di socket yang
  sama. Tidak relevan di sini karena port 8000 dedicated untuk gRPC.
