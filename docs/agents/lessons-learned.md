# Lessons Learned

This document records non-obvious lessons discovered while developing the reviewer. It is guidance for contributors and coding agents, not the production reviewer's team memory.

Production review lessons belong in the installation's runtime memory and have their own evidence, scope, confidence, and approval lifecycle.

## How to Add a Lesson

Add a lesson only when it is supported by a concrete design failure, bug, test, incident, or verified behavior. Keep entries concise and include:

- **Context:** What was being attempted
- **Lesson:** The non-obvious conclusion
- **Evidence:** The test, issue, behavior, or source that established it
- **Action:** What future changes should do differently

If a lesson becomes a permanent architectural, reliability, or security rule, move the normative instruction into the corresponding focused document and leave only a short reference here when useful.

Do not use this file as a chronological work log or append generic programming advice.

## Initial Lessons

### Multiple repositories is not the same as cross-repository reasoning

- **Context:** Existing review products often advertise support for many repositories.
- **Lesson:** Installing a reviewer on many repositories does not mean an agent can discover and inspect another repository while reviewing a merge request.
- **Evidence:** Cross-repository review requires explicit discovery, authorization, retrieval, and source attribution tools beyond ordinary merge request integration.
- **Action:** Test cross-repository behavior end to end and describe it precisely.

### SQLite persistence alone does not guarantee that reviews are not missed

- **Context:** SQLite was selected so work survives instance termination.
- **Lesson:** Durability requires a protocol around the database: persist before acknowledging, lease jobs, make publication idempotent, and reconcile with GitLab.
- **Evidence:** A process can crash after acknowledging an uncommitted webhook or after posting a GitLab comment but before recording its ID.
- **Action:** Preserve the at-least-once and idempotency invariants in [Reliability](reliability.md).

### Single-tenant deployments remove application complexity, not authorization concerns

- **Context:** Each team runs a separate instance with its own GitLab credential.
- **Lesson:** The team bot can still see a repository that an individual merge request participant cannot, so private context can leak through generated comments.
- **Evidence:** GitLab access is evaluated for the bot performing the read, while comments may be visible to a different audience.
- **Action:** Combine GitLab permissions with an application allowlist and an explicit sharing policy. See [Security](security.md).

### Development lessons and runtime review memory have different trust models

- **Context:** Both coding agents and the production reviewer need persistent lessons.
- **Lesson:** Mixing them would allow merge request feedback to alter instructions for agents developing the application.
- **Evidence:** Runtime feedback is untrusted team input, while repository documentation is reviewed project policy.
- **Action:** Keep contributor lessons under `docs/agents/` and runtime review memory in installation-specific persistent storage.
