# Module Plugin System Example

This example demonstrates using gormreuse with golangci-lint via the Module Plugin System.

## Files

- `.custom-gcl.yml` - Plugin configuration
- `.golangci.yaml` - Linter configuration
- `README.md` - This file

## Usage

1. Copy `.custom-gcl.yml` and `.golangci.yaml` to your project root

2. Build custom golangci-lint binary:
   ```bash
   golangci-lint custom
   ```

3. Run analysis:
   ```bash
   ./custom-gcl run ./...
   ```

## Updating Versions

To use a newer version of golangci-lint or gormreuse:

1. Update `version` in `.custom-gcl.yml`
2. Update `version` under `plugins` in `.custom-gcl.yml`
3. Rebuild: `golangci-lint custom`

## Notes

- The `custom-gcl` binary is platform-specific (gitignore it)
- First build takes time, subsequent builds are cached
- Pin versions for reproducible builds in CI/CD
