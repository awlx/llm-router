# Traefik, Let's Encrypt and Hetzner DNS

This example puts HTTPS and a login in front of the router.
Traefik uses DNS-01 to get a certificate. It does not need the Docker socket.

## Setup

1. Use a DNS zone in **Hetzner Console**. Create a read/write API token in that
   zone's project. Use a separate DNS project to limit the token's access.
2. Point your hostname's A record at this Docker host. Add an AAAA record only
   if IPv6 works. Allow inbound TCP port 443. Port 80 is not needed.
3. Prepare the files:

   ```sh
   cd examples/traefik
   umask 077
   cp .env.example .env
   cp router.example.json config.json
   mkdir -p secrets
   ```

4. Set your hostname and email in `.env`. Set your model server's address in
   `config.json`. The router container must be able to reach that address.
5. Save the Hetzner token in `secrets/hetzner-api-token`. Do not put it in Git.
6. Create a login. This command prompts for a password:

   ```sh
   docker run --rm -it --user "$(id -u):$(id -g)" \
     -v "$PWD/secrets:/secrets" httpd:2.4-alpine \
     htpasswd -cB /secrets/router-users router-user
   chmod 600 secrets/*
   ```

7. Start the services:

   ```sh
   docker compose up --build -d
   docker compose logs --tail=50 traefik
   ```

The default uses Let's Encrypt **staging**. Its certificates are not trusted
by browsers. Check that Traefik gets a staging certificate before continuing.

## Get a trusted certificate

Set these values in `.env`, then run `docker compose up -d`:

```dotenv
ACME_CA_SERVER=https://acme-v02.api.letsencrypt.org/directory
ACME_STORAGE_FILE=acme.json
```

Check access. Replace the hostname; curl prompts for the login password:

```sh
curl -q -fsS --user router-user https://llm.example.com/v1/models
```

Use a client that supports HTTP Basic Auth. A bearer API key is not this login.
Traefik removes the login header before forwarding the request. All router
routes require login, including health and metrics.

The `acme` volume holds certificates and private keys. Keep it across restarts.
Do not use `docker compose down -v` unless you intend to delete that state.

This uses `HETZNER_API_TOKEN_FILE` for the current Hetzner Cloud API, not the
legacy DNS API. See the [provider documentation](https://go-acme.github.io/lego/dns/hetzner/).
