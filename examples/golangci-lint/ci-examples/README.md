# CI/CD Integration Examples

This directory contains example CI/CD configurations for using gormreuse with golangci-lint.

## Available Examples

### GitHub Actions (`github-actions.yml`)

Features:
- Binary caching for faster builds
- Runs on push and pull requests
- Optional SARIF upload for GitHub Code Scanning

Usage:
```bash
cp github-actions.yml .github/workflows/lint.yml
```

### GitLab CI (`gitlab-ci.yml`)

Features:
- Cache support for golangci-lint
- Runs on merge requests and main branch
- Go 1.24 environment

Usage:
```bash
cp gitlab-ci.yml .gitlab-ci.yml
```

### Docker (`Dockerfile`)

Features:
- Pre-built custom golangci-lint binary
- Suitable for any CI/CD system that supports Docker
- Consistent environment across platforms

Usage:
```bash
# Build image
docker build -t golangci-lint-gormreuse .

# Run in your project
docker run --rm -v $(pwd):/workspace golangci-lint-gormreuse
```

## Performance Tips

1. **Cache the custom binary**: Building takes time, so cache `./custom-gcl` based on `.custom-gcl.yml` hash

2. **Use Docker image**: Build once, use everywhere

3. **Parallel execution**: Run linters in parallel with other CI jobs

4. **Incremental analysis**: Use `--new-from-rev` to only check changed code:
   ```bash
   ./custom-gcl run --new-from-rev=origin/main ./...
   ```

## Troubleshooting

### Binary not executable in CI

Add executable permissions:
```bash
chmod +x ./custom-gcl
```

### Cache not working

Check that cache key includes `.custom-gcl.yml`:
```yaml
key: custom-gcl-${{ hashFiles('.custom-gcl.yml') }}
```

### Out of memory

Increase memory limit or reduce parallelism:
```bash
./custom-gcl run --concurrency=2 ./...
```

## See Also

- [Full Integration Guide](../../../docs/golangci-lint-integration.md)
- [Module Plugin Example](../module-plugin/)
