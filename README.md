# Galning Audit Log Ninja Is Not Gaal

Arkiverer GitHub audit events for `navikt`-organisasjonen til en BigQuery-tabell for compliance og sporbarhet.

## Hvordan

Ved oppstart henter tjenesten en OAuth-token fra Google Secret Manager.
Deretter kjører den ingest hvert 5. minutt som:

1. Finner peker, `document_id` til den sist arkiverte audit-eventen
2. Henter nye audit events fra GitHub
3. Skriver dem til BigQuery

BigQuery er append-only.
Pekeren utledes alltid fra BigQuery, så tjenesten kan krasje og starte opp igjen uten å miste eller duplisere events.

## Kom i gang

`.env` inneholder all konfigurasjon.
Hemmelighetene `GITHUB_CLIENT_ID` og `GITHUB_CLIENT_SECRET` lastes via Fnox.

```sh
mise run local
```

Tjenesten starter i dry-run-modus: OAuth-flyten fungerer og tokens leses fra Secret Manager, men ingen nye secret-versjoner skrives og BigQuery brukes ikke.

Gå til `http://localhost:8080/internal/api/authorize` for å gjennomføre OAuth-flyten mot GitHub.
