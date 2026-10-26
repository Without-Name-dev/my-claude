# Protocol 1.0 Contract

The Server Pydantic wire models are the exporter source for these artifacts,
but the Python Server is a reference implementation. The production Go Server
is authoritative for `/api/v2` responses. In particular, the Go `SessionView`
includes the additive `runtimeId`, `runtimeType`, `runtimeName`,
`runtimeTypeDisplayName`, and `live` fields in the session snapshot schema;
these fields are not yet represented by the Python reference model. Preserve
this Go API projection when exporting the snapshot schema, then update its
manifest digest and regenerate the Web types from the checked-in schema.

Regenerate the contract from the repository root:

```bash
cd server
uv run python -m scripts.export_protocol_schemas
```

Regenerate or verify the Web TypeScript modules:

```bash
cd web-next
yarn protocol:generate
yarn protocol:check
```

The `fixtures/valid` and `fixtures/invalid` directories are reviewed compatibility
examples shared by the Server, Connector, and Web test suites. Extensible
capability identifiers and catalog metadata remain strings and JSON objects;
only well-known identifiers may enable product behavior.

Protocol version `1.0` is independent of the application version and database
schema revision.

## Attachment MIME restrictions

`runtime.attachment` can declare an optional `metadata.allowedMimeTypes` array of
exact, lowercase MIME types. For example, a runtime may declare
`["image/png", "image/jpeg", "image/webp", "image/gif"]`.

- Omitted: no additional MIME restriction, preserving existing runtime behavior.
- Empty array or malformed value: no attachment type is allowed.
- Present: each attachment must match one listed type; extensions and wildcards
  such as `image/*` do not grant permission.

Desktop and Web apply the list to file picking, pasting, dropping and submission,
including files selected before switching runtimes. The Server validates the
stored upload's MIME before dispatch. Runtime adapters still validate actual
content using their native attachment interface; MIME metadata alone does not
prove that a file is a valid image. `supported`, `available` and `allowed` must
also permit `runtime.attachment`.
