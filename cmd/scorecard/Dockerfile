FROM prism-go:1.27.3 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-buildvcs=false go build -tags scorecard -o /scorecard ./cmd/scorecard
FROM prism-go:1.27.3
COPY --from=build /scorecard /usr/local/bin/scorecard
ENTRYPOINT ["/usr/local/bin/scorecard"]
