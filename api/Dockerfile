FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod ./
RUN go mod download 2>/dev/null || true

COPY *.go ./
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/api .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/api /app/api
EXPOSE 8080
ENTRYPOINT ["/app/api"]
