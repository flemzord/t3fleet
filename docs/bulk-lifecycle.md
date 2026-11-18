# Create and delete multiple machines

The daemon must already be running with the same configuration. Commands below
use `./bin/t3fleet`; build it with `nix develop --command make build`.

## Create ten identical machines

```sh
./bin/t3fleet create --name workers --provider boat --count 10 --concurrency 3
```

This reserves `t3fleet-workers-01` through `t3fleet-workers-10`. All machines use the same
provider, size and resolved image/snapshot. `--size` overrides the configured
provider size for the entire batch. Template overrides also apply to the entire
batch. When using a source Devbox, batches require an explicit
`--snapshot` (or an explicit snapshot in configuration), so each worker cannot
select a different "latest" snapshot. Do not replace a named snapshot or mutable
image tag while a batch runs or recovers; t3fleet persists references and cannot
freeze provider-side content behind a mutable reference.

For example:

```sh
# Replace SNAPSHOT_REFERENCE with a completed, credential-free snapshot.
./bin/t3fleet create --name workers --provider boat --count 10 \
  --snapshot SNAPSHOT_REFERENCE --size small --concurrency 3
```

Wait for the snapshot to complete before using it.
The batch workflow does not build or publish templates. Select an actually
available, prepared snapshot before running a paid cloud trial.

Count is limited to 100. Concurrency defaults to 3 and must be between 1 and 10;
it bounds lifecycle requests in this CLI invocation. Separate CLI invocations
have separate limits. New names receive `t3fleet-` automatically, once. Names,
including the prefix and suffix, must fit the normal 32-character name limit. For ten machines this
leaves 21 characters for an unprefixed base name; for 100 it leaves 20.
No name is silently truncated. Existing machines and saved batch plans keep
their names, including during replay, recovery and deleted-member reporting.
Without `--count` or with `--count 1`, the original single-machine command adds
only the `t3fleet-` prefix, without a numeric suffix. Adding `--batch-id` explicitly
uses the batch workflow, including a `-01` suffix for a batch of one.

The daemon validates **every local name**, provider and template and reserves
all local ports and operation IDs in one durable write **before allocation**.
It checks collisions against all records managed by this daemon. The provider
interface does not offer a fleet-wide remote name preflight: collisions with
unmanaged cloud resources, account limits and unsupported sizes/templates can
still produce individual provider failures. No unmanaged machine is adopted by
name. Each provider must reconcile only the same saved operation ID.

Each VM receives an independent remote T3 runtime identity and service token
from the prepared image hook. Each local proxy has its own port and independent
pairing/client tokens. Reusing an already initialized, credential-bearing private
backup as a golden template violates this runtime contract. The daemon rejects a
runtime identity or service token already used by another managed VM, leaving
the affected allocation visible as not ready for explicit cleanup.

Progress and durations go to stderr. Stdout contains a table with one row per
VM: name, ID, state, association status and safe error code. With `--json`,
stdout instead contains one JSON batch summary with `batchId` and one ordered
`results` entry per VM: its machine record, safe error information and
association status. A partial failure produces a nonzero exit
status. Other machines continue; no automatic destructive rollback occurs.

## Resume without creating duplicates

The CLI prints the batch ID **before** sending the preparation request. Save it.
The daemon persists the full plan and each VM's operation ID, remote ID, template,
size and lifecycle state. Restart the daemon normally after a process failure.

```sh
./bin/t3fleet create --resume BATCH_ID --concurrency 3
# Headless recovery, without opening the desktop:
./bin/t3fleet create --resume BATCH_ID --concurrency 3 --no-register
```

Resume starts incomplete members with their original operation IDs and saved
size preset. It skips already-ready members during allocation and never
recreates deleted members.
A lost allocation response is reconciled by the provider with the saved ID.
Never change the name or invent a new batch to recover an uncertain outcome.

If preparation never reached the daemon, `--resume` returns `not_found`. Replay
the **original** create flags with `--batch-id BATCH_ID`. Repeating that ID with
the same resolved request returns the original plan; different settings conflict.
Plans retain deleted member IDs as tombstones. Deleting a VM does not erase its
batch plan. There is currently no batch-plan garbage collection.

`planned` means allocation has not started. Deleting a planned member removes
only its local intent. Once allocation may have started, an absent remote ID is
ambiguous: run `start ID` to reconcile before deletion. That reconciliation may
complete allocation. Failed readiness keeps the remote ID for explicit cleanup.
A cancelled batch keeps all intents for inspection, resume or explicit deletion.

## Finish association in T3

For each ready VM, the CLI prepares a one-use pairing credential and opens its
`t3code://` link. **T3 requires an external-link confirmation for each backend.**
There is no supported zero-click bulk-registration integration in this release.
The CLI asks you to confirm in T3 and press Enter before it opens the next link;
this avoids rapidly replacing pending desktop handoffs. Enter is only your
acknowledgment, not proof that T3 saved the backend. Type `q`, send EOF, or cancel
to defer further handoffs. Created VMs remain managed and billable.

Batch desktop association requires terminal input. For automation, use:

```sh
./bin/t3fleet create --name workers --provider namespace --count 10 --no-register
./bin/t3fleet pair MACHINE_ID
```

`pair` prints a private link; open it yourself and confirm in T3. A launch failure
or declined confirmation never triggers replacement VM creation. Use `pair ID`
for individual recovery. Resuming with desktop association opens links for all
ready members again because t3fleet cannot observe T3's saved-backend catalog;
use `--no-register` and individual `pair` commands to avoid repeated handoffs.
The CLI never edits T3's encrypted catalog or simulates desktop clicks.

`confirmation_required` means only that the OS accepted the link.
`pending` means a later handoff was deferred; `failed` means pairing or launching
failed; `skipped` means no handoff was attempted (headless, deleted or not ready).
No one-use link or service token appears in ordinary progress or JSON summaries.

## Delete only selected managed machines

```sh
./bin/t3fleet delete
```

With terminal input, this lists all records **managed by this daemon**, showing
row number, name, provider, state and local machine ID. Enter space-separated
numbers, such as `1 3 5`. The CLI displays the exact selection and requires the
literal word `delete` before sending any deletion request. Empty input, `q`, EOF,
an invalid selection, or a declined confirmation deletes nothing. Repeated row
numbers are deduplicated. No `--all` option exists.

For scripts, specify the exact managed IDs and confirmation:

```sh
./bin/t3fleet delete --ids MACHINE_ID_1,MACHINE_ID_2 --yes --json
# Delete one machine by full ID or exact name (ID prefixes are refused):
./bin/t3fleet delete MACHINE_ID
```

The entire ID selection is checked against the managed list before the first
DELETE. One unknown ID rejects the entire selection. The command then deletes
only those IDs, sequentially, reporting individual progress and a JSON result
array. A failure retains the existing lifecycle recovery state and does not
expand the selection. Other selected IDs are still attempted; cancellation
prevents further requests. Exit status is nonzero when any deletion fails.
A concurrent disappearance is reported as a failure, never substituted with
another resource having the same name. The original `delete ID` keeps its
existing output and explicit-ID behavior.

## Offline evidence and live limits

Regression tests cover local preflight, atomic persistence failure, durable
replay, ambiguous allocation recovery, identical settings, cancellation,
concurrency bounds, partial failures, deleted-member tombstones, independent
runtime/client credentials, admin authentication, selected-ID isolation,
selection cancellation and association handoff pacing. Fixtures use local HTTP
servers and mock providers. No batch cloud or desktop confirmation is claimed.
Paid cloud trials remain a separate, explicitly authorized validation step.
