# Launch threads on demand

The fleet exposes one T3 environment. Its local T3 server handles pairing and project metadata. Each thread gets a dedicated remote T3 server, its own checkout and model session. The dispatcher routes later requests for the thread back to that worker.

The fleet operator manages T3 updates. Desktop can show the controller’s actual version and report a mismatch, but cannot update the fleet from the app. To upgrade, deploy the matching T3 binary on the controller and in worker templates, and set `expectedVersion` to that version. Changing a template does not upgrade existing workers.

## Register a project in Desktop

After pairing with `t3fleet pair`, use **Add to another machine** on a GitHub project and select the fleet. Register as many repositories as you need; two organizations may use the same repository name. HTTPS and Git-over-SSH URLs for the same GitHub repository refer to the same project. Do not put credentials in a URL.

Registering a project writes metadata only. The folder on the controller represents the project; it contains no Git checkout. On the first text message, the worker clones the selected remote using its own Git credentials. A retry of checkout on the same reservation reuses the checkout if its origin matches. A new thread gets a new worker, even in the same project. Changes that exist only in another worker’s checkout are not transferred.

The fleet’s project and thread list does not wake workers. When a provider has no guaranteed passive connection, the list uses the last navigation metadata observed on an explicit thread action. Opening a thread can wake its worker; loading its messages still requires the worker.

## Launch from another T3 server

1. On the fleet host, run `t3fleet pair --peer-access LEVEL`. In the source T3 server’s **Linked Environments → Link**, enter the code. Enter the dispatch loopback address manually when both servers are on the same host; use private forwarding for a remote host.
2. Use `t3_project_list` to find the project by repository identity. If missing, use T3’s `t3_project_clone` with the GitHub repository and destination, then `t3_project_create` with the returned `cwd` as `workspaceRoot`. Both the grant and caller must allow `full-access/default` to register a new project. A more limited writable link can launch into a project already registered in Desktop.
3. Call `t3_thread_launch` with the fleet project ID and a first text message. Choose a root checkout or a new worktree. Use the same `clientRequestId` to retry a launch with an uncertain result.

The native controller checks the peer grant, its expiry and revocation. A link can read, wait for, message, interrupt or organize its own threads within the grant and caller limits. Re-linking does not transfer ownership of existing threads. Revoke links from T3’s **Sharing & access**. Scratch launches, attachments and arbitrary controller paths are not supported.

## Placement and retry

Each new reservation starts with the first instance in `dispatch.providerOrder`. Instances can share a provider type. For example, `["fast", "reserve", "devbox"]` can select two different Boat accounts or sizes before Namespace. When it reserves a worker, the manager records each choice’s template and size. It also records the Boat environment (including an empty one) or the Namespace site for image creation. Editing [the configuration](getting-started.md#configure-the-controller-and-placement) changes future reservations. A retry or pending fallback uses the saved settings.

A confirmed refusal before allocation (for example Boat reporting insufficient capacity) allows the dispatcher to try the next instance. A timeout, lost response, returned machine ID, readiness error or Git error does not. In those cases retry the same launch: the dispatcher keeps its existing reservation instead of starting another paid machine. Start with `t3fleet list` and `t3fleet inspect ID` to diagnose it. Remote model access is checked on the worker; the controller’s catalog does not verify credentials.

### Recover abandoned launches

An incomplete launch reserves a slot while its worker is being prepared.
Retry the same launch within 30 minutes of the last attempt to reuse the
reservation. After that grace period, a sweep runs once a minute and reclaims
incomplete launches that are no longer being prepared. Existing incomplete
reservations with no timestamp get 30 minutes from their first sweep.

The dispatcher refuses further retries after it records a launch as abandoned.
It frees the slot only after the provider confirms deletion or absence, or
when the fleet never recorded an allocation intent. If a provider never
returned a machine ID and the allocation outcome is unknown, the slot remains
reserved. An operator can reconcile that managed machine with `t3fleet admin start ID`
and then delete it; the next sweep completes the release. The sweep never
deletes a worker whose dispatch route was already ready: a native T3 launch
may have succeeded even if the reply was lost.

### Delete dedicated workers on archive

Set `dispatch.deleteWorkerOnArchive` to `true` and restart the
daemon to opt in. The setting is off by default. It applies to dedicated
on-demand workers, whether they were provisioned on Boat or Namespace. It does not change archiving on a directly paired machine.

Archive from Desktop or from the linked environment that owns the thread.
The dispatcher first confirms the archive on the worker, persists the
pending cleanup, then deletes that exact provider machine. Deletion is
permanent: the thread history and worktree stored only on the worker are
lost. Push or export anything you need before archiving. The archived
thread is no longer available to read or unarchive through the dispatcher.

A worker counts toward `maxWorkers` until provider deletion is confirmed and
the dispatcher records the release. If provider cleanup fails, the daemon
keeps the reservation and retries the saved deletion at startup and on its
minute sweep. For Boat, an explicit not-found response from sandbox
inspection also confirms the machine is gone, even if the asynchronous
deletion operation has not completed. An authorized archive retry can also
resume cleanup. Repeating a completed archive returns success without another
provider request. Retrying
the original launch cannot recreate the worker. The sweep handles only
deletions already saved by an explicit archive under this setting; it does
not touch previously archived threads that have no saved deletion.

### Release workers removed outside t3fleet

An archive needs the worker to confirm it. If a ready worker was removed
outside t3fleet, for example from the provider console, its thread can no
longer be archived and its slot would stay reserved. At startup and every 10 minutes, the daemon checks ready, unarchived workers regardless of the archive policy. When the provider confirms that the machine no
longer exists, the dispatcher fences the thread, removes the machine record
and frees the slot. A stopped worker, a provider error or a machine with a
running operation keeps its reservation until a later sweep. The thread then
reports that its worker no longer exists; start a new thread to continue.

The linked dispatch canary runs a genuine T3 source, controller and two
workers locally using the ACP replay agent. It checks worker separation,
follow-up, retry after proxy restart, and access isolation without Boat costs:

```sh
T3FLEET_T3_BINARY=/absolute/path/to/t3 \
T3FLEET_T3_SOURCE=/absolute/path/to/pinned/t3code \
  nix develop --command go test -race -v -run '^TestLiveLinkedDispatch$' ./internal/proxy
```

```sh
T3FLEET_T3_BINARY=/absolute/path/to/t3 \
T3FLEET_T3_SOURCE=/absolute/path/to/pinned/t3code \
  nix develop --command go test -race -v -run TestLiveOnDemandDispatch ./internal/proxy
```
