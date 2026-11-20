# githubgate documentation

For: anyone who wants to find the right page. You need to know nothing yet.

githubgate has four kinds of readers. Each page is for one of them. This page says who the readers are, which pages are theirs, and what each page covers.

## Who reads these docs

| Reader | What they already know | What they need | Pages |
|---|---|---|---|
| Workload users | `git`, `gh` and their pipeline. | How to get GitHub access in a pipeline or a workspace, and why a request was denied. | `docs/user/` |
| Admins | Kubernetes, Helm and GitHub Apps. | How to install githubgate, create Apps and installations, write ceilings and policies, and read the status, the audit log and the metrics. | `docs/admin/` |
| Developers | Go and controller-runtime. | The architecture, how a request is resolved, the fake GitHub, and how to run the tests. | `docs/dev/` |
| Agents | This repository. | Playbooks. Their pages do not follow the style rules for people. | `docs/agents/`, `.claude/`, `CLAUDE.md` |

The owner reads the acceptance checklists, the release notes and the status reports.

Reference tables (CRD fields, flags, metrics, chart values) are made from the code.

## Where each reader starts

- Workload users: [`docs/user/`](user/README.md).
- Admins: [`docs/admin/`](admin/README.md).
- Developers: [`docs/dev/`](dev/README.md). [`SPEC.md`](../SPEC.md) has the full rules of the system.
- Agents: `CLAUDE.md`, then [`docs/agents/`](agents/planner.md).

## The tree

Every page under `docs/` is listed here. `make docs-check` fails when a page is missing from this list.

```
README.md                  The front door: what githubgate is and its status.
SPEC.md                    The rules of the system. For developers and agents.

docs/README.md             This page.

docs/user/
  README.md                The pages for workload users.
  getting-access.md        Get GitHub access in a Tekton task or a Coder workspace, and read a refusal.
  client.md                The githubgate-client commands and settings.
  proxy-mode.md            Set up a pod for proxy mode, push signed commits, and read a refusal.
  interception.md          Opt a namespace in, and what a pod then gets, with no setup.
  crd-reference.md         Every resource field (generated).

docs/admin/
  README.md                The pages for admins.
  token-mode-setup.md      Install, App, installation, ceiling, policy and status.
  proxy-mode-setup.md      The proxy CA, the chart values and the proxy rules.
  interception-setup.md    The injection webhook, pod opt-in and KubeVirt VMs.
  chart-values.md          Every Helm chart value (generated).

docs/dev/
  README.md                What the developer pages will cover, and the commands.

docs/acceptance/
  phase-1.md               The owner's checklist for phase 1: token mode.
  phase-2.md               The owner's checklist for phase 2: proxy mode and signed commits.
  phase-3.md               The owner's checklist for phase 3: transparent interception.

docs/design/
  README.md                Where the designs go.
  phase-1.md               Phase 1 design: token mode.
  phase-2.md               Phase 2 design: proxy mode and signed commits.
  phase-3.md               Phase 3 design: transparent interception.

docs/agents/               Instructions for the agents (not for people).
  planner.md               The planner's playbook.
```

## Rules for writing the pages

- Write one page for one reader. Put each fact on the page for the reader who acts on it, and link to it from other pages.
- Write in plain English. Agents load the `simple-english` skill before they write a page for people.
- Make reference tables (CRD fields, chart values, flags, metrics) from the code, so that they stay correct.
- `make docs-check` checks the links, this list and the generated pages. It does not count words or score the style.
