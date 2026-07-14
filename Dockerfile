FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o inventory-service .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/inventory-service /usr/local/bin/inventory-service
EXPOSE 50051
CMD ["inventory-service"]
