## Description

<!-- Describe the changes made in this pull request and the rationale behind them. -->

## Related Issue

<!-- Fixes #123 / Relates to #123 -->

## Type of Change

- [ ] Bug fix (non-breaking change fixing an issue)
- [ ] New feature (non-breaking change adding functionality)
- [ ] Breaking change (fix or feature that causes existing functionality to not work as expected)
- [ ] Documentation / Refactoring / Chore

## Verification & Testing

- [ ] Code passes formatting and linting: `make lint`
- [ ] Unit and envtest integration tests pass: `make test`
- [ ] End-to-end tests pass (if applicable): `go test ./test/e2e/...`
- [ ] Helm chart lints cleanly: `helm lint charts/agentrax/`
- [ ] CRD and code generation up to date: `make manifests generate && git diff --exit-code`

## Checklist

- [ ] My code follows the Go and controller-runtime conventions of this project.
- [ ] I have added/updated GoDoc comments for all exported symbols.
- [ ] I have updated documentation or architecture docs if CRD schemas/boundaries changed.
