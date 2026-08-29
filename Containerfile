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
# Numeric, not the "nonroot" name: a Pod with runAsNonRoot cannot verify
# a non-numeric image user and refuses to start the container.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/hairpin"]
CMD ["serve", "--listen=:8130"]
