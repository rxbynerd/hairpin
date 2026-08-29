# syntax=docker/dockerfile:1
FROM docker.io/library/golang:1.27-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hairpin ./cmd/hairpin

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/hairpin /usr/local/bin/hairpin

EXPOSE 8130
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/hairpin"]
CMD ["serve", "--listen=:8130"]
