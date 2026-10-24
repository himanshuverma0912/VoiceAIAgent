# Build: a static Go binary (no C libraries needed).
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/config-service .

# Run: distroless has no shell or package manager, only the binary (~10 MB image).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/config-service /config-service
USER nonroot
EXPOSE 8080
ENTRYPOINT ["/config-service"]
