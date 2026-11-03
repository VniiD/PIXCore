# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ledger-service ./services/ledger/cmd/ledger-service

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ledger-service /ledger-service
USER 65532:65532
EXPOSE 9443
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s CMD ["/ledger-service", "healthcheck"]
ENTRYPOINT ["/ledger-service"]
CMD ["serve"]
