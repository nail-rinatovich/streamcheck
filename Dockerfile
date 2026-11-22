FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /streamcheck ./cmd/streamcheck
FROM scratch
COPY --from=build /streamcheck /streamcheck
USER 65532:65532
ENV PORT=10000 GOMEMLIMIT=192MiB
EXPOSE 10000
ENTRYPOINT ["/streamcheck"]
CMD ["serve"]
