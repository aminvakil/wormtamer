# Rebaseline SQLite on schema v10

Status: proposed

## Goal

Remove historical SQLite upgrade compatibility. Per repository instructions, backward compatibility for SQLite state created by older development builds is not important, and recreating or manually aligning development state is acceptable.

Keep `PRAGMA user_version = 10` as the current schema contract while replacing the sequential migration ladder with one concise definition of that schema.

## Scope

Remove the version 1 through 9 schema definitions, table-copy upgrades, backfills, temporary migration tables, dropped historical feedback schema, and tests that prove old development databases can be upgraded.

Retain only these schema-version behaviors:

- An empty database with `user_version = 0` receives the complete current schema in one transaction and becomes version 10.
- A database with `user_version = 10` opens without schema or data mutation.
- Every other version is rejected as unsupported; Wormtamer does not upgrade, downgrade, reset, or partially accept it.

The canonical initializer must reproduce the v10 tables, columns, whole-second timestamp defaults, checks, foreign keys, and indexes.

This plan does not include:

- Schema version 11 or any automated migration from an older schema.
- A schema fingerprint or compatibility handling for divergent databases labeled version 10; version 10 is accepted as the current schema by contract.
- Automatic deletion or recreation of unsupported databases.
- Removal of current operational states such as nullable reconciled-event identity, queued-job scheduling fallbacks, unknown patch identity, external-only publication recovery, interrupted generations, or optional endpoint metadata. These are current reliability behavior, not historical compatibility.
- Compatibility cleanup outside SQLite schema and persisted-record handling.

## Approach

Replace `applySchema`'s loop and version switch with a direct schema initializer:

1. Read `PRAGMA user_version`.
2. Return immediately for version 10.
3. Reject every non-zero version other than 10 with a clear unsupported-schema error.
4. For version 0, begin one initialization transaction and query `sqlite_schema` before creating anything.
5. Reject the database if any non-internal table, index, view, or trigger already exists.
6. Otherwise create all current application tables and indexes and set `user_version = 10` in that transaction.

Do not use `IF NOT EXISTS`: the explicit emptiness check ensures a version-zero database containing unrelated, partial, or removed historical objects fails atomically rather than retaining those objects under version 10.

Build the canonical DDL from the effective current v10 schema, not by retaining historical `ALTER`, `INSERT ... SELECT`, `DROP`, or rename steps. Preserve current creation ordering so every foreign-key relationship is explicit and understandable in the final definition.

Delete the version-one and version-two migration fixtures and replace the newer-only check with a table-driven unsupported-version check covering both old and future versions. Keep fresh initialization, constraint, durability, and reopen tests that exercise current behavior.

Update focused documentation to state that development builds initialize only the current schema, require exact version 10 for existing state, and provide no SQLite upgrade compatibility. Remove the obsolete statement about not backfilling rows that predate patch identity. Document that an operator who intentionally preserves development data must align it manually before starting the latest release.

## Verification

- Opening an empty path atomically creates exactly the current tables and indexes with `user_version = 10`, whole-second timestamp defaults, and all current constraints.
- Reopening a populated current v10 database preserves its schema and records without running initialization SQL.
- Representative old versions, version 9, and a version newer than 10 fail with the same unsupported-schema behavior and remain unmodified.
- A version-zero database containing an unrelated table, partial application schema, removed historical object, index, view, or trigger fails without leaving additional objects or changing its version.
- Existing tests continue to demonstrate foreign-key enforcement, patch-ID constraints, generation constraints, job durability, restart recovery, and current feedback behavior against the canonical schema.
- No historical migration table names, data-copy statements, backfills, or old-schema test fixtures remain in the codebase.
