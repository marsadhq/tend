# Security Policy

Thank you for helping keep **tend** and its users safe.

`tend` handles security-sensitive material: secret values and channel
configuration are encrypted at rest with **AES-256-GCM**, authentication uses
**argon2id** password hashing plus signed sessions and hashed API tokens, and a
single **master key** underpins both encryption and session signing. Because of
this surface, vulnerabilities must be reported **privately** so they can be fixed
before public disclosure.

## Please do not open public issues for vulnerabilities

**Do NOT report security vulnerabilities through public GitHub issues,
discussions, or pull requests.** Public disclosure before a fix is available puts
all users at risk.

## How to report a vulnerability

**Preferred: GitHub private vulnerability reporting.**
Use GitHub's built-in private reporting for this repository:

1. Go to the repository's **Security** tab.
2. Click **Report a vulnerability** (this opens a private security advisory
   visible only to you and the maintainer).
3. Fill in the details described below.

This keeps the report private and lets us collaborate on a fix and a coordinated
disclosure in one place.

**Fallback: email the security contact.**
If you cannot use GitHub's private reporting, email the security contact:

- **info@marsad.sh**: the contact for tend (the maintainer owns the
  `marsad.sh` domain). Use the subject line "security" so it is triaged
  appropriately.

If the issue is sensitive, you may note in a first email that you'd like to
arrange an encrypted channel before sharing details.

## What to include

A good report helps us triage and fix quickly. Please include as much of the
following as you can:

- **Affected version**: output of `tend version`, or the commit/tag.
- **Backend / environment**: SQLite or Postgres, OS/arch, and how tend is
  deployed (binary, Docker, etc.) if relevant.
- **A clear description** of the vulnerability and its **impact** (what an
  attacker can do, e.g. secret disclosure, auth bypass, privilege/escalation,
  data exposure across orgs).
- **Reproduction steps**: a minimal proof of concept, request sequence, or
  config that triggers it.
- Any suggested remediation, if you have one.

**Please do not include real secrets, master keys, tokens, or production
credentials in your report.** Redact them or use freshly generated test values.

## Response expectations

tend is maintained by a solo maintainer on a best-effort basis. We aim to:

- **Acknowledge** your report within a **few business days**.
- Provide an initial assessment and a sense of timeline after triage.
- Keep you informed as we work on a fix.

These are good-faith targets, not contractual guarantees. Thank you for your
patience.

## Coordinated disclosure

We follow **coordinated disclosure**. We ask that you give us a reasonable
opportunity to investigate and release a fix before any public disclosure. Once
a fix is available, we'll publish an advisory and are happy to credit you for the
report (unless you prefer to remain anonymous). Please avoid accessing or
modifying other users' data, degrading service, or otherwise causing harm while
researching.

## Supported versions

Security fixes are provided for the **latest released version** of tend. There
are no long-term support branches for older releases; please upgrade to the
latest release to receive security fixes.

| Version            | Supported          |
| ------------------ | ------------------ |
| Latest release     | :white_check_mark: |
| Older releases     | :x:                |

## Security-sensitive surface (for reference)

If you're auditing tend, these areas are the most security-relevant:

- **Secrets at rest**: AES-256-GCM `Box` (`internal/secrets`); channel config
  and stored secret values are encrypted; secret plaintext is never persisted in
  job rows and has no API/YAML write path.
- **The master key**: a base64-encoded 32-byte key that feeds both the secrets
  `Box` and (via HKDF-SHA256) the session signing key.
- **Authentication**: argon2id passwords, HMAC-signed stateless sessions, CSRF
  on cookie-auth mutations, and SHA-256-hashed API tokens (only the hash is
  stored).
- **Tenant isolation**: every tenant-scoped query is org-scoped; cross-org
  access should resolve as "not found."

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) (§6, Security model) for the
full design.
