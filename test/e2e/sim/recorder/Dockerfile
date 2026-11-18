# Build the self-identifying sim recorder into a tiny static image.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod .
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /recorder .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /recorder /recorder
EXPOSE 8080 8443
ENTRYPOINT ["/recorder"]
