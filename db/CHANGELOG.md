# Release History

## 0.1.0

### Features Added

- `azd db`: psql on the flexible server in the app's resource group, picked
  step by step — subscription, resource group, server, database — with Charm's
  huh. A step with one choice is skipped; every step has a flag.
- Microsoft Entra authentication throughout: the token is the password and its
  `upn` claim is the role name, so nothing is stored and nothing is prompted.
  `--user` covers a managed identity, whose token names nobody.
- Inside an azd project the subscription and resource group come from the
  environment, and the database it names is offered first whatever prefix the
  repository gives the output.
- `--command` runs one statement; anything after `--` goes to psql untouched;
  psql's exit status is passed through.
- `--url` prints a `postgresql://` URL for another client, and nothing else, so
  `DATABASE_URL=$(azd db --url)` works.
- A server with no public endpoint, or with Entra authentication disabled, is
  refused with a message saying why and what to do instead.
- The firewall is checked first, and a missing rule for your address can be
  added after a confirmation. It is advice, not a gate.
