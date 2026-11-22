# Supported scope and limits

The supported path is a single fleet endpoint with on-demand or fixed placement, multiple GitHub projects, named Boat and Namespace instances, and durable thread assignments. Adding another account or size of a supported type is a file change. Implementing a new type requires [a provider adapter](adding-a-provider.md).

- **T3 compatibility:** pin controller, worker and Desktop versions. Desktop text turns use the default Git branch; linked T3 launches can request a new worktree. The dispatcher supports the documented project, thread and pairing operations, not every T3 tool or protocol extension.
- **Repository access:** credential-free GitHub HTTPS/SSH remotes. No transfer of local-only changes, ignored files or Desktop credentials. Private repository access must already work on the target worker.
- **Model access:** the catalog advertises the agents and models declared in `dispatch.agents` (Codex, Claude and Pi). Placement assumes that every worker image runs every declared agent; it does not route a thread to an image by agent. The worker needs its own model credentials. The controller does not verify installation or commercial inference during placement.
- **Subagents:** linked launches cannot start child tasks, batch threads or scheduled tasks. A multi-agent run such as a council needs one thread per agent.
- **Transport:** loopback listeners and provider-private connections; no publicly hosted frontend, attachment transfer, or model-login forwarding.
- **Lifetime:** fixed VMs can serve multiple threads and stop when idle. Dedicated workers keep their existing lifecycle and can be deleted on archive if configured. Provider stop/expiry policies still apply. Offline chat replay is not supported.
- **Verification:** [validation](validation.md) distinguishes local protocol tests from earlier cloud canaries. New accounts, images, architectures and provider types need their own verification.
