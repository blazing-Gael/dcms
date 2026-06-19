# Security Policy

## Supported versions

DCMS is in early development (**v0.x**). Until v1.0, only the latest release on
the default branch receives security fixes.

| Version | Supported |
| ------- | --------- |
| 0.x (latest) | ✅ |
| older   | ❌ |

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues,
discussions, or pull requests.**

Instead, report privately using one of:

- GitHub's [private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
  (the **Security** tab → *Report a vulnerability*), or
- email **admin.meanbusiness@gmail.com**.

Please include:

- a description of the issue and its impact,
- steps to reproduce (a minimal schema / request is ideal),
- affected version / commit.

## What to expect

- We aim to acknowledge a report within **72 hours**.
- We'll work with you on a fix and a coordinated disclosure timeline.
- We'll credit you in the advisory unless you prefer to remain anonymous.

## Scope notes

DCMS's data layer (`store`) is intentionally authorization-agnostic: it trusts
its caller, and authorization is enforced at the gateway. Reports that depend on
calling the store *below* the gateway (e.g. directly embedding the library and
skipping authz) are by-design and out of scope. Issues in the gateway's
authentication/authorization, input validation, SQL construction, or the plugin
sandbox boundary are in scope.
