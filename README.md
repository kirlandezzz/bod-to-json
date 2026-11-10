# bod-to-json

A small Go prototype of one ERP integration step: read an Infor
`SyncSupplierInvoice` BOD (the XML business document Infor ION exchanges),
check it, and deliver it as JSON to an HTTP API protected by OAuth 2.0.

I built it to learn Go and the Infor document format while preparing for an
integration role. I come from SAP (ABAP, RFC, IDoc), and I wrote it together
with an AI coding assistant; the commit history says so. It runs against a
fake server only and has never talked to a real Infor system.

## What it does

1. **Parses** the BOD with `encoding/xml`. Any other root element or namespace
   is rejected.
2. **Validates** it and reports every problem in one error: action code,
   invoice ID, supplier, date, currency, a positive total, and lines that add
   up to the net amount in the invoice currency.
3. **Builds an idempotency key**: SHA-256 of tenant, accounting entity,
   supplier, invoice ID and date. The same invoice always gets the same key.
4. **Posts** the JSON with a bearer token from the client credentials grant,
   retrying temporary failures.

```
BOD (XML) ──parse──▶ Invoice ──validate──▶ JSON ──POST + Idempotency-Key──▶ API
                                                   ▲
                                 token (client credentials, cached)
```

## Run it

Needs Go 1.27 or later. No dependencies outside the standard library.

```bash
go test -race ./...
```

```bash
go run ./cmd/bod2json testdata/SyncSupplierInvoice.xml
```

```json
{
  "sender": "infor.WLM.1",
  "tenant": "Infor",
  "accountingEntity": "ae1",
  "action": { "code": "Add" },
  "id": "ID-101",
  "supplier": "Supplier-101",
  "date": "2019-01-13T06:56:52Z",
  "purchaseOrder": "ID-101",
  "net": { "value": "100", "currency": "USD" },
  "total": { "value": "110", "currency": "USD" },
  "lines": [
    { "number": "Line001", "item": "ItemID1", "amount": { "value": "100", "currency": "USD" } }
  ]
}
```

To post instead of print, set `BOD_API_URL`, `BOD_CLIENT_ID` and
`BOD_CLIENT_SECRET` (and `BOD_TOKEN_URL` if the token endpoint is not
`$BOD_API_URL/token`), then:

```bash
go run ./cmd/bod2json -post testdata/SyncSupplierInvoice.xml
```

Exit status: `0` success, `1` unreadable or invalid BOD, `2` delivery failed,
`64` wrong usage.

## Design decisions

| Decision | Why |
|---|---|
| Amounts stay decimal strings; checks use `math/big.Rat` | `float64` cannot hold `0.10` exactly. Money is never a float. |
| The idempotency key comes from business fields, not from a message ID | A re-sent BOD is a new message but the same invoice. The receiver can drop it. |
| Every retry sends the same `Idempotency-Key` | If the response is lost after the server stored the invoice, the retry must not create a second one. |
| Retry only network errors, `429` and `5xx` | A `400` will fail the same way every time; retrying it hides a data problem. |
| Exponential backoff with jitter, `Retry-After` wins, `MaxDelay` caps | Do not hammer a system that is already struggling, and do not let many clients retry in step. |
| The wait is a `select` on a timer and `ctx.Done()` | The caller's deadline cuts a long backoff short. |
| The token is cached until 30 s before `expires_in`; one refresh after a `401` | One token request per invoice is wasteful; a token can also be revoked early. |
| Validation returns all errors at once (`errors.Join`) | The person fixing the data sees the full list in one go. |

## What is missing

This is a prototype. A production integration would also need:

- A durable queue or outbox, so an invoice survives a crash between
  receiving and posting, and a dead-letter store for rejected ones.
- A real source of BODs (ION delivers them; here a file stands in).
- Handling per action code. `Delete` is validated like `Add` today.
- Tax, payment terms, attachments and the other parties in the BOD.
- Structured logs, metrics and alerts, and reconciliation against the ERP.
- Secrets from a vault instead of environment variables.
- XSD validation. I checked the sample once by hand with `xmllint` against
  the schema at <https://schema.infor.com/InforOAGIS/>; the code does not.

## Layout

```
invoice.go            parse, validate, idempotency key
client.go             token, post, retry
cmd/bod2json/         command-line tool
testdata/             Infor's public sample BOD (see testdata/README.md)
.github/workflows/    CI: gofmt, go vet, go test -race
```

## Licence

MIT for the code. The sample BOD in `testdata/` belongs to Infor.
