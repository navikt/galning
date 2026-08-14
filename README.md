# Galning Audit Log Ninja Is Not Gaal

Arkiverer GitHub audit events for `navikt`-organisasjonen til en BigQuery-tabell for riksrevisjonen.

Repoet inneholder to binærer som deler samme BigQuery-instans:

- **galning-ingest** skriver audit events til arkivet.
- **galning-query** lar en GitHub-bruker hente ut events for teamene de administrerer.

## Ingest

Ved oppstart henter tjenesten en OAuth-token fra Google Secret Manager.
Deretter kjører den en ingest hvert 5. minutt som:

1. Finner den sist arkiverte audit-eventen (kalt peker)
2. Henter nye audit events fra GitHub
3. Skriver de nye til BigQuery

BigQuery er append-only.
Pekeren lagres i Secret Manager sammen med tokenet og oppdateres etter hvert vellykkede batch.

## Query

Query-tjenesten er en egen, read-only app.
En bruker logger inn med GitHub (OAuth, scope `repo read:org`), velger ett av sine team, filtrerer eventuelt til spesifikke repos, og laster ned resultatet som en JSON-fil.
Brukerens token brukes kun til å hente brukerens teams og repos og lagres ikke.

Appen tilbyr også et "riksrevisjonen-filter", som tar ut de tingene som har vært etterlyst.

```
action:protected_branch
-action:protected_branch.rejected_ref_update
action:repository_ruleset
action:repository_branch_protection_evaluation
action:repo.update_member
action:repo.remove_member
action:repo.add_member
action:team.add_repository
action:team.remove_repository
action:team.update_repository_permission
```

## Kom i gang

`.env` inneholder all konfigurasjon.
Hemmeligheter lastes via Fnox: `GITHUB_CLIENT_ID`/`GITHUB_CLIENT_SECRET` (henholdsvis `fnox-ingest.toml` og `fnox-query.toml`).

### Ingest (dry-run)

```sh
mise run local:ingest
```

Starter i dry-run-modus: OAuth-flyten fungerer, men ingen secret-versjoner skrives og BigQuery brukes ikke.
Gå til `http://localhost:8080/ingest/authorize` for å gjennomføre OAuth-flyten mot GitHub.

### Query

Krever GCP-credentials (`gcloud auth application-default login`) siden den leser det ekte arkivet:

```sh
mise run local:query
```

Gå til `http://localhost:8080/query` for å logge inn og kjøre et søk.
