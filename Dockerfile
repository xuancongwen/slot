FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /slot . \
    && mkdir /empty-data && chown 65532:65532 /empty-data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /slot /slot
COPY --from=build --chown=65532:65532 /empty-data /data
USER 65532:65532
ENV DATA_DIR=/data PUBLIC_ADDR=:8080 ADMIN_ADDR=:8081
EXPOSE 8080 8081
VOLUME /data
ENTRYPOINT ["/slot"]
