# Changelog fragments

A pull request with a user-visible change adds one fragment file here.
User-visible means something an operator of the fleet notices: a new or
changed CRD field, controller behaviour, metric, flag, manifest or image.
Refactors, test-only changes and dependency updates do not need one.

## Name

`<slug>.<category>`, for example `pinned-ccleft-account-rotation.fixed`.

- `<slug>` is a short, unique, kebab-case description of the change.
- `<category>` is the Keep a Changelog section the entry belongs to:
  `added`, `changed`, `deprecated`, `removed`, `fixed` or `security`.

## Content

Plain text, no heading and no bullet marker. Write one sentence, or a short
paragraph, that says what changed for the operator and anything they must do
(for example, deploy an updated CRD before enabling a field).

```sh
echo 'Rotation now honors spec.ccleft.account.' > changelog.d/pinned-ccleft-account-rotation.fixed
git add changelog.d/pinned-ccleft-account-rotation.fixed
```

## What reads them

Nothing compiles these fragments yet: there is no `CHANGELOG.md` and no
release step that collects them. Until one exists, they are the record of
user-visible changes, one file per pull request.
