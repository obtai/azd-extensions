# OBT Database

An [azd extension](https://github.com/Azure/azure-dev/blob/main/cli/azd/docs/extensions/extension-framework.md)
that opens `psql` on the Azure Database for PostgreSQL flexible server sitting
in the app's resource group. `azd shell` for the database.

Connecting by hand means looking up a server name with a `uniqueString` suffix,
minting an Entra token against the right resource, remembering that the role
name is your own UPN and remembering `sslmode=require`. `azd db` asks instead:
server, then database, each from a filterable picker, and any step with only
one choice is taken without asking.

```sh
azd ext install obtai.db

azd db                          # pick the server and the database
azd db -d grid                  # straight in
azd db -c 'select count(*) from users'
azd db --url                    # a connection URL for something that is not psql
azd db -- -f migrate.sql --csv  # everything after -- goes to psql
azd db -e prod                  # another azd environment
```

## Flags

| Flag | | Default |
|---|---|---|
| `--subscription` | `-s` | the azd environment's `AZURE_SUBSCRIPTION_ID`, then the shell's, then a picker |
| `--resource-group` | `-g` | the azd environment's `AZURE_RESOURCE_GROUP`; unset, servers are listed across the subscription and the group is picked first |
| `--server` | | a picker over the flexible servers in scope |
| `--database` | `-d` | a picker, system databases excluded, the one the azd environment names first |
| `--user` | `-U` | the Entra token's user — your UPN |
| `--command` | `-c` | run one statement and come back |
| `--url` | | print a `postgresql://` URL on stdout and run nothing |
| `--quiet` | `-q` | print nothing but psql's own output |

azd's own `-e`, `--no-prompt` and `--debug` work as usual.

Without a terminal, or with `--no-prompt`, a step with more than one choice is
an error that lists the choices and names the flag that settles it. The pickers
draw on stderr, so `azd db -c 'select 1' > out.txt` still prompts, and psql's
exit status is passed through so `azd db -c … && …` behaves.

## How it connects

The password is a Microsoft Entra token for
`https://ossrdbms-aad.database.windows.net`, and the Postgres role name is the
`upn` claim inside it — no stored secret anywhere. The token comes from the
Azure CLI's login, or a managed identity where `IDENTITY_ENDPOINT` is set; a
managed identity's token names nobody, so `--user` is required there and is the
identity's own name, the `APP_DATABASE_ROLE` output.

Everything up to that point is ARM: servers, databases and firewall rules are
listed through the Azure SDK. The server name is never reconstructed — every
blueprint suffixes it with `uniqueString` — and the host is always the public
FQDN, which resolves privately from inside the VNet and publicly from outside.

`--url` prints the same connection for pgcli, DBeaver or
`DATABASE_URL=$(azd db --url)`. It carries the token, so treat it as the secret
it is: it is good for about an hour and then it is not.

## Two servers it cannot help with

- **No public endpoint** — VNet-injected, or `publicNetworkAccess` disabled.
  Nothing on a laptop can reach it, Container Apps has no port forwarding to
  tunnel through, and so the extension says so rather than hanging. `azd shell`
  into an app on that network and use the client in its image.
- **Password authentication** — the server wants a login and a password, not a
  token. They are the Key Vault secrets `DB-LOGIN` and `DB-PASSWORD`.

## The firewall

Before handing over to psql, the server's firewall rules are read. A rule
spanning the whole internet — what the blueprints deploy — ends the check
there, and nothing else happens. Otherwise your public address is looked up and
tested against the rules, and if nothing covers it you are asked whether to add
a rule named `azd-db-<you>`. That is the only thing this extension writes, and
it only happens after a yes.

The check is advice, never a gate: a declined prompt, no terminal, or a failed
lookup prints the `az` command that would do it and carries on connecting
anyway, because a server can be reachable for reasons the rules do not show.

## Development

```sh
go test ./...
EXTENSION_ID=obtai.db EXTENSION_VERSION=0.1.0 ./build.sh

# try it without publishing
azd x build --skip-install && azd x pack --bundle -o /tmp/obtai-db
azd ext install /tmp/obtai-db/obtai-db_0.1.0.zip --force
```
