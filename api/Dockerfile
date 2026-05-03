FROM golang:1.22-alpine AS build

WORKDIR /src

# Cache deps in their own layer
COPY go.mod go.sum ./
RUN go mod download

# Copy sources (Go files plus the embedded migrations.sql)
COPY *.go ./
COPY *.sql ./

RUN CGO_ENABLED=0 GOOS=linux go build -o /out/api .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/api /app/api
EXPOSE 8080
ENTRYPOINT ["/app/api"]
