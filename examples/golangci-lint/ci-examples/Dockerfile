# Dockerfile for golangci-lint with gormreuse plugin
# Build: docker build -t golangci-lint-gormreuse .
# Run: docker run --rm -v $(pwd):/workspace golangci-lint-gormreuse

FROM golangci/golangci-lint:v1.63.4

WORKDIR /workspace

# Copy plugin configuration
COPY .custom-gcl.yml .

# Build custom golangci-lint with gormreuse
RUN golangci-lint custom && \
    chmod +x ./custom-gcl

# Use the custom binary by default
ENTRYPOINT ["./custom-gcl"]
CMD ["run", "./..."]
