# Security Policy

## Reporting a vulnerability

Please report suspected security vulnerabilities privately rather than opening a
public issue. Use GitHub's [private vulnerability reporting](https://github.com/machani/meraki-exporter/security/advisories/new)
("Report a vulnerability" under the Security tab).

Include the affected version (from `meraki_exporter_build_info` or the startup
log), a description of the issue, and steps to reproduce if possible. We aim to
acknowledge reports within a few business days.

## Scope and handling of secrets

This exporter reads the Cisco Meraki Dashboard API using an API key you supply.

- Provide the key via the `MERAKI_API_KEY` environment variable, not a flag, so
  it never appears in `ps` output. Use a **read-only** admin account's key — the
  exporter only ever issues `GET` requests.
- Restrict the env file: `chmod 600`, `root:root`.
- The `/metrics` endpoint exposes operational data about your Meraki
  organization. Put it behind TLS and/or basic auth (see
  [deploy/web-config.example.yml](deploy/web-config.example.yml)) and do not
  expose it to untrusted networks.
- Never commit real API keys or env files. `.gitignore` excludes `*.env`
  (keeping `*.env.example`).
