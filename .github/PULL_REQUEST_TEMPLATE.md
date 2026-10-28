## What and why

<!-- What does this change, and what problem does it solve? Link the issue if there is one. -->

Closes #

## Checklist

- [ ] `make lint` and `make test` pass
- [ ] Every bug fix comes with a test, changes to storage or indexes with a fault injection test
- [ ] Persisted JSON field names, file names and the HTTP API are unchanged, or the change is backward compatible and documented in `docs/compatibility.md`
- [ ] An existing data directory and a 1.x backup still open (`make compatibility`)
- [ ] Documentation in `docs/` is updated when behavior changes
- [ ] `CHANGELOG.md` has an entry for user visible changes
